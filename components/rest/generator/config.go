// Package generator derives an OpenAPI 3.0 document from the source of an Echo application
// built on compose's rest handlers. It reads the code purely syntactically (go/parser, no
// type checking): route files give the operations, handler bodies give the request type, the
// path and query parameters and the content types, and the request type's module function
// gives the response type. Everything application-specific comes in through Config.
package generator

import (
	"os"
	"path/filepath"

	"github.com/vanclief/ez"
)

// Config describes where an application declares its routes and handlers and how its
// document is shaped. Paths are relative to Root. Every hook is optional.
type Config struct {
	// Root is the module root, the directory holding go.mod. Defaults to the working directory.
	Root string
	// RoutesGlob matches the files declaring routes, e.g. "application/interfaces/rest/routes_*.go".
	// Route declarations must fit on one line: `x := parent.Group("/prefix", ...)` and
	// `x.GET("/path", h.Method, ...)`.
	RoutesGlob string
	// HandlersFile holds the struct whose `Name *pkg.Handler` fields are the handler namespaces,
	// routed as `h.Name.Method` (nested mode). Empty selects flat mode: one handler type routed as
	// `h.Method`, with the namespace taken from the first segment of the route path.
	HandlersFile string
	// HandlersStruct names the struct in HandlersFile. Defaults to "Handlers".
	HandlersStruct string
	// HandlerGlob matches the handler sources, e.g. "application/interfaces/rest/handler/*/*.go".
	HandlerGlob string
	// HandlerReceiver is the receiver type of handler methods. Defaults to "Handler".
	HandlerReceiver string
	// ReceiverVar is the variable route files call handlers through. Defaults to "h".
	ReceiverVar string
	// RootGroups maps the route files' root variables to their base path. Defaults to {"e": ""}.
	RootGroups map[string]string
	// OutDir is where Run writes openapi.json and openapi.yaml (the public spec) and
	// openapi.internal.json and openapi.internal.yaml (the full spec), relative to Root.
	OutDir string

	Info     Info
	Servers  []Server
	Security []SecurityScheme

	// RequiredPackagePrefix marks the packages whose structs are server-serialized models: their
	// non-omitempty, non-nilable fields are required. Matched as a substring of the import path,
	// e.g. "/application/models/". Empty marks nothing required.
	RequiredPackagePrefix string
	// SchemaPrefixCollapse maps a module-relative package path prefix to the path every package
	// under it is named after, e.g. {"application/interfaces/rest/handler/":
	// "application/interfaces/rest/handler"} names all handler subpackages' types alike.
	SchemaPrefixCollapse map[string]string
	// SchemaNameAliases maps a module-relative package path to the schema name prefix its types
	// (and its subpackages' types) keep, so moving a package does not rename its schemas.
	SchemaNameAliases map[string]string

	// PathSummaries overrides the summary of the operations on an OpenAPI path.
	PathSummaries map[string]string
	// SummaryVerbs are the leading words a summary is kept as is with; other GET and POST
	// summaries get a "Get" or "Create" prefix. Defaults to DefaultSummaryVerbs.
	SummaryVerbs []string
	// TagNames overrides the tag of a namespace, e.g. {"crm": "CRM"}.
	TagNames map[string]string

	// QueryGetters maps the name of a call reading a query parameter to its schema. Defaults to
	// DefaultQueryGetters; extend that table to add an application's own helpers.
	QueryGetters map[string]QueryGetter
	// ParamGetters maps the name of a call reading a path parameter to its schema. Defaults to
	// DefaultParamGetters.
	ParamGetters map[string]ParamGetter
	// ResponseCalls are the calls whose last argument is the handler's request type. Defaults to
	// JSONResponse, BindedJSONResponse, BindedXMLResponse and BlobResponse.
	ResponseCalls []string
	// BodyBinders are the calls that bind a request body, so DELETE handlers using them get one.
	// Defaults to BindedJSONResponse and BindedXMLResponse.
	BodyBinders []string
	// Negotiate names the call a handler negotiates its response content type with. Handlers
	// calling it respond with NegotiatedContentTypes.
	Negotiate              string
	NegotiatedContentTypes []string
	// DeprecatedWrapper names the function wrapping deprecated route handlers, e.g.
	// "logDeprecated". Deprecated routes are left out of the document.
	DeprecatedWrapper string

	// Skip leaves an operation out of the document. path is the OpenAPI path ({id}, not :id).
	Skip func(method string, path string) bool
	// Internal marks an operation as internal: it is documented with `x-internal: true` and
	// Spec.Public leaves it out. path is the OpenAPI path.
	Internal func(method string, path string) bool
	// Public marks an operation as needing no authentication (`security: []`).
	Public func(method string, path string) bool
	// MultipartPath reports whether a path's request body is multipart/form-data with files.
	MultipartPath func(path string) bool
	// ContentTypes returns a route's response content types; nil falls back to the default
	// (NegotiatedContentTypes for negotiating handlers, application/json otherwise).
	ContentTypes func(r Route) []string
	// RequestSchema replaces the request schema of a handler, keyed by Route.HandlerKey.
	RequestSchema func(handlerKey string) (map[string]any, bool)
	// Schemas runs over all component schemas once they are built, before request-side
	// requiredness is stripped and before the operations are assembled.
	Schemas func(schemas map[string]map[string]any)
	// Operation runs on every operation once it is assembled. op["parameters"] is a []any
	// holding the path and pagination parameters; parameters the hook adds win by name over the
	// ones derived from the handler body, which are appended afterwards.
	Operation func(r Route, op map[string]any)
}

// Info is the document's info object.
type Info struct {
	Title       string
	Version     string
	Description string
}

// Server is one entry of the document's servers list. Variables maps each URL variable to
// its default value.
type Server struct {
	URL       string
	Variables map[string]string
}

// SecurityScheme is an API key sent in a request header. Every scheme is required globally;
// Public operations opt out.
type SecurityScheme struct {
	Name   string
	Header string
}

// QueryGetter describes a call that reads one query parameter.
type QueryGetter struct {
	// Schema is the parameter's schema.
	Schema map[string]any
	// NameArg is the position of the parameter's string-literal name among the call arguments.
	NameArg int
	// Name is the parameter's fixed name, for helpers that read one without taking it as an
	// argument (GetListLimit reads "limit"). It wins over NameArg.
	Name string
	// EnumArg, when positive, is the position of a `pkg.<Type>FromString` parse function; the
	// parameter is then an array of the Type component and Schema is ignored.
	EnumArg int
}

// ParamGetter describes a call that reads one path parameter.
type ParamGetter struct {
	Schema  map[string]any
	NameArg int
}

// DefaultSummaryVerbs are the leading words that already make a summary read as an action.
var DefaultSummaryVerbs = []string{
	"Add", "Cancel", "Check", "Compute", "Create", "Delete", "Deliver", "Get", "Issue", "List",
	"Login", "Logout", "Overview", "Register", "Remove", "Report", "Rollback", "Send", "Sync",
	"Unissue", "Update", "Upgrade", "Upload",
}

// DefaultQueryGetters returns the query readers of echo and compose's BaseHandler.
func DefaultQueryGetters() map[string]QueryGetter {
	return map[string]QueryGetter{
		"QueryParam": {
			Schema:  map[string]any{"type": "string"},
			NameArg: 0,
		},
		"GetNumericQueryParam": {
			Schema:  map[string]any{"type": "integer", "format": "int64"},
			NameArg: 1,
		},
		"GetQueryParamInt64": {
			Schema:  map[string]any{"type": "integer", "format": "int64"},
			NameArg: 1,
		},
		"GetQueryParamInt64s": {
			Schema: map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "integer", "format": "int64"},
			},
			NameArg: 1,
		},
		"GetQueryParamTime": {
			Schema:  map[string]any{"type": "string", "format": "date-time"},
			NameArg: 1,
		},
		"GetListLimit": {
			Schema: map[string]any{"type": "integer", "format": "int32"},
			Name:   "limit",
		},
	}
}

// DefaultParamGetters returns the path parameter readers of echo and compose's BaseHandler.
func DefaultParamGetters() map[string]ParamGetter {
	return map[string]ParamGetter{
		"Param": {
			Schema:  map[string]any{"type": "string"},
			NameArg: 0,
		},
		"GetParameterUUID": {
			Schema:  map[string]any{"type": "string", "format": "uuid"},
			NameArg: 1,
		},
		"GetParameterInt": {
			Schema:  map[string]any{"type": "integer", "format": "int64"},
			NameArg: 1,
		},
		"GetParameterInt64": {
			Schema:  map[string]any{"type": "integer", "format": "int64"},
			NameArg: 1,
		},
		"GetParameterString": {
			Schema:  map[string]any{"type": "string"},
			NameArg: 1,
		},
	}
}

func (cfg Config) withDefaults() (Config, error) {
	if cfg.Root == "" {
		wd, err := os.Getwd()
		if err != nil {
			return cfg, ez.Wrap(err)
		}
		cfg.Root = wd
	}

	root, err := filepath.Abs(cfg.Root)
	if err != nil {
		return cfg, ez.Wrap(err)
	}
	cfg.Root = root

	if cfg.RoutesGlob == "" {
		return cfg, ez.New(ez.EINVALID, "generator: RoutesGlob is required", nil)
	}
	if cfg.HandlerGlob == "" {
		return cfg, ez.New(ez.EINVALID, "generator: HandlerGlob is required", nil)
	}

	if cfg.HandlersStruct == "" {
		cfg.HandlersStruct = "Handlers"
	}
	if cfg.HandlerReceiver == "" {
		cfg.HandlerReceiver = "Handler"
	}
	if cfg.ReceiverVar == "" {
		cfg.ReceiverVar = "h"
	}
	if cfg.RootGroups == nil {
		cfg.RootGroups = map[string]string{"e": ""}
	}
	if cfg.SummaryVerbs == nil {
		cfg.SummaryVerbs = DefaultSummaryVerbs
	}
	if cfg.QueryGetters == nil {
		cfg.QueryGetters = DefaultQueryGetters()
	}
	if cfg.ParamGetters == nil {
		cfg.ParamGetters = DefaultParamGetters()
	}
	if cfg.ResponseCalls == nil {
		cfg.ResponseCalls = []string{"JSONResponse", "BindedJSONResponse", "BindedXMLResponse", "BlobResponse"}
	}
	if cfg.BodyBinders == nil {
		cfg.BodyBinders = []string{"BindedJSONResponse", "BindedXMLResponse"}
	}
	if len(cfg.NegotiatedContentTypes) == 0 {
		cfg.NegotiatedContentTypes = []string{"application/json"}
	}

	return cfg, nil
}

func (cfg Config) flat() bool {
	return cfg.HandlersFile == ""
}
