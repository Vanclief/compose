// Package api is compose's declarative REST route registry. Every operation
// is registered once with a typed handler, func(ctx, *Req) (Res, error); the
// registry mounts it on Echo, produces an OpenAPI document (3.1 and 3.0.3)
// and validates every request against that document before the handler sees
// it.
//
// Registration is boot-time: invalid registrations (lint errors, path
// placeholders without a path:"x" field, duplicate operationIds, ...) panic,
// as do Handle after Ready and a Ready that cannot build the documents,
// finds a protected operation, hidden or not, without an authenticator, or
// finds an Echo error handler that does not answer an ez error with a 4xx
// (see below), or finds an operation whose route a later Echo route on the
// same method and path replaced. Call Ready once every route is registered,
// before serving traffic.
//
// Each request is authenticated, then validated against the schema (a JSON
// body nested deeper than 64 levels is refused before that), decoded
// once and checked with Body.Validate, in that order, before exactly one
// handler runs: the Produces handler of the negotiated media type, else the
// operation's own. Authentication runs before any body byte is read, so an
// unauthenticated caller learns nothing about the request's shape.
// Authenticate sets the authenticator for api.New, a group or an operation,
// the nearest one winning, and marks those operations protected; Public skips
// authentication and drops the security requirement from the spec. The
// request context derives from the Echo request's; Echo middleware after
// next(c) reads the request the registry built, and the context the
// authenticator produced, through handler.Request (see also Accept).
//
// Errors, the pipeline's and the handler's, are returned to Echo, so
// middleware after next(c) gets them from next(c), and are answered by the
// app's HTTPErrorHandler: rest.ErrorHandler answers them in the
// handler.ErrorResponse envelope the spec documents, and its
// WithErrorTranslator and WithErrorReporter options localize and report
// them. Echo's default handler would answer every
// registry error as a 500 {"message":"Internal Server Error"}, so Ready calls
// the installed handler once with an ez EINVALID error and panics unless it
// answers with a 4xx; that call reaches the handler's reporter and
// translator, with handler.Request nil.
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

	"github.com/labstack/echo/v5"
	"github.com/vanclief/ez"

	"github.com/vanclief/compose/components/rest/api/internal/huma"
	"github.com/vanclief/compose/components/rest/api/openapi"
	"github.com/vanclief/compose/components/rest/handler"
)

// securitySchemeName is the name of the API key security scheme.
const securitySchemeName = "apiKey"

// DefaultMaxBody is the request body limit in bytes of an operation that no
// MaxBodyBytes applies to.
const DefaultMaxBody = 1 << 20

var errorResponseType = reflect.TypeFor[handler.ErrorResponse]()

// Registry holds every registered operation of an Echo instance.
//
// A body sent without a Content-Type header is decoded as JSON. That is not
// a CSRF vector for an app that authenticates with a header; an app that
// authenticates with cookies must add a CSRF middleware.
type Registry struct {
	echo *echo.Echo

	info         huma.Info
	servers      []*huma.Server
	apiKeyHeader string
	namer        func(reflect.Type) string
	opts         []Option // operation options of New, applied before the group's

	reflector *openapi.Reflector
	ops       []*operation
	ids       map[string]bool
	routes    map[string]string // routeKey -> operationId

	mu     sync.Mutex
	ready  atomic.Bool
	result *openapi.Result
	specs  map[Version][]byte
	strict []byte
}

// Info sets the document title and version. Registry option.
func Info(title, version string) Option {
	return Option{name: "api.Info", registry: func(r *Registry) {
		r.info.Title = title
		r.info.Version = version
	}}
}

// Description sets the document description. Registry option.
func Description(description string) Option {
	return Option{name: "api.Description", registry: func(r *Registry) {
		r.info.Description = description
	}}
}

// APIKeyHeader declares an API key security scheme read from the given
// header and requires it on every operation not marked Public. Registry
// option.
func APIKeyHeader(name string) Option {
	return Option{name: "api.APIKeyHeader", registry: func(r *Registry) {
		r.apiKeyHeader = name
	}}
}

// Server adds a server URL to the document. Registry option.
func Server(url string) Option {
	return Option{name: "api.Server", registry: func(r *Registry) {
		r.servers = append(r.servers, &huma.Server{URL: url})
	}}
}

// Namer sets the component namer (openapi.WithNamer), for example
// openapi.SegmentsNamer(2) when two packages declare the same type name.
// Registry option.
func Namer(namer func(reflect.Type) string) Option {
	return Option{name: "api.Namer", registry: func(r *Registry) {
		r.namer = namer
	}}
}

// New creates a registry bound to an Echo instance. Its operations return
// their errors to Echo, so the app sets e.HTTPErrorHandler to
// rest.ErrorHandler for the handler.ErrorResponse envelope the spec
// documents; with Echo's default handler every registry error would be a 500
// {"message":"Internal Server Error"}, so Ready panics while it is installed.
// Authentication runs before the body is read, and handler.Request returns
// the request an operation built. Operation options such as Authenticate,
// Timeout or MaxBodyBytes apply to every operation, before the options of its
// groups and its own. Panics when an option is an operation option
// (ContentType, Produces).
func New(e *echo.Echo, opts ...Option) *Registry {
	err := checkOptions(opts, registryScope)
	if err != nil {
		panic(ez.New(ez.EINVALID, "api: New: "+ez.ErrorMessage(err), err))
	}

	r := &Registry{
		echo:   e,
		info:   huma.Info{Title: "API", Version: "0.0.0"},
		ids:    map[string]bool{},
		routes: map[string]string{},
	}
	for _, opt := range opts {
		if opt.registry == nil {
			r.opts = append(r.opts, opt)
			continue
		}
		opt.registry(r)
	}
	r.reflector = openapi.NewReflector(openapi.WithNamer(r.namer))

	err = r.reflector.AddResponse(errorResponseType)
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
	Public      bool     // no authentication (api.Public)
	Produces    []string // 200 response media types, the primary one first
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
			Public:      op.public,
			Produces:    slices.Clone(op.produces),
		}
	}
	return out
}
