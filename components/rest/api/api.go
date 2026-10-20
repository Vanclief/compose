// Package api is compose's declarative REST route registry. Every operation
// is registered once with its Go request and response types; the registry
// mounts it on Echo, produces an OpenAPI document (3.1 and 3.0.3) and
// validates every request against that document before the app sees it.
//
// Registration is boot-time: invalid registrations (lint errors, path
// placeholders without a path:"x" field, duplicate operationIds, ...) panic,
// and Handle after Ready panics. Call Ready once every route is
// registered, before serving traffic.
//
// Tags: an operation's tag is the last non-parameter segment of its group's
// full prefix (/runs -> runs, /runs/:id/issues -> issues, /v1/runs -> runs).
// Its operationId is "<tag>.<name>", or just "<name>" when the prefix has no
// non-parameter segment.
package api

import (
	"reflect"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/danielgtaylor/huma/v2"
	"github.com/labstack/echo/v4"
	"github.com/vanclief/ez"

	"github.com/vanclief/compose/components/rest/api/openapi"
	"github.com/vanclief/compose/components/rest/handler"
)

// securitySchemeName is the name of the API key security scheme.
const securitySchemeName = "apiKey"

// DefaultMaxBody is the request body limit in bytes when neither MaxBody
// nor MaxBodyBytes sets one.
const DefaultMaxBody = 1 << 20

var errorResponseType = reflect.TypeFor[handler.ErrorResponse]()

// Registry holds every registered operation of an Echo instance.
//
// A body sent without a Content-Type header is decoded as JSON; both apps
// authenticate with a header, so this is not a CSRF vector for them. Apps
// that authenticate with cookies must add a CSRF middleware.
type Registry struct {
	echo    *echo.Echo
	handler *handler.BaseHandler

	info         huma.Info
	servers      []*huma.Server
	apiKeyHeader string
	maxBody      int64
	namer        func(reflect.Type) string

	reflector *openapi.Reflector
	ops       []*operation
	ids       map[string]bool
	routes    map[string]string // routeKey -> operationId

	mu       sync.Mutex
	ready    atomic.Bool
	readyErr error
	result   *openapi.Result
	specs    map[Version][]byte
	strict   []byte
}

// RegistryOption configures a Registry.
type RegistryOption func(*Registry)

// Info sets the document title and version.
func Info(title, version string) RegistryOption {
	return func(r *Registry) {
		r.info.Title = title
		r.info.Version = version
	}
}

// Description sets the document description.
func Description(description string) RegistryOption {
	return func(r *Registry) {
		r.info.Description = description
	}
}

// APIKeyHeader declares an API key security scheme read from the given
// header and requires it on every operation not marked Public.
func APIKeyHeader(name string) RegistryOption {
	return func(r *Registry) {
		r.apiKeyHeader = name
	}
}

// Server adds a server URL to the document.
func Server(url string) RegistryOption {
	return func(r *Registry) {
		r.servers = append(r.servers, &huma.Server{URL: url})
	}
}

// MaxBody sets the default request body limit in bytes for every operation
// (DefaultMaxBody when unset). MaxBodyBytes overrides it per group or
// operation. Panics when n is not positive.
func MaxBody(n int64) RegistryOption {
	if n <= 0 {
		panic(ez.New(ez.EINVALID, "api: MaxBody must be positive", nil))
	}
	return func(r *Registry) {
		r.maxBody = n
	}
}

// Namer sets the component namer (openapi.WithNamer), for example
// openapi.SegmentsNamer(2) when two packages declare the same type name.
func Namer(namer func(reflect.Type) string) RegistryOption {
	return func(r *Registry) {
		r.namer = namer
	}
}

// New creates a registry bound to an Echo instance and a compose handler,
// whose App receives the requests and whose ManageError writes the errors.
func New(e *echo.Echo, h *handler.BaseHandler, opts ...RegistryOption) *Registry {
	r := &Registry{
		echo:    e,
		handler: h,
		info:    huma.Info{Title: "API", Version: "0.0.0"},
		ids:     map[string]bool{},
		routes:  map[string]string{},
		maxBody: DefaultMaxBody,
	}
	for _, opt := range opts {
		opt(r)
	}
	r.reflector = openapi.NewReflector(openapi.WithNamer(r.namer))

	err := r.reflector.AddResponse(errorResponseType)
	if err != nil {
		panic(ez.New(ez.EINTERNAL, "api: could not register handler.ErrorResponse", err))
	}

	return r
}

// Version selects the OpenAPI version produced by Spec.
type Version int

const (
	V31 Version = iota // OpenAPI 3.1.0
	V30                // OpenAPI 3.0.3
)

// OperationInfo describes a registered operation.
type OperationInfo struct {
	Method      string
	Path        string // Echo syntax, /runs/:id
	SpecPath    string // OpenAPI syntax, /runs/{id}
	OperationID string
	Tag         string
	Hidden      bool
	Produces    []string // 200 response media types
}

// Operations returns the registered operations in registration order.
func (r *Registry) Operations() []OperationInfo {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]OperationInfo, len(r.ops))
	for i, op := range r.ops {
		out[i] = OperationInfo{
			Method:      op.method,
			Path:        op.path,
			SpecPath:    specPath(op.path),
			OperationID: op.id,
			Tag:         op.tag,
			Hidden:      op.hidden,
			Produces:    slices.Clone(op.produces),
		}
	}
	return out
}
