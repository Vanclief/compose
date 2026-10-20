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
	// ExternalDocs links the document to documentation kept elsewhere.
	ExternalDocs *ExternalDocs

	// RequiredPackagePrefix marks the packages whose structs are server-serialized models,
	// matched as a substring of the import path, e.g. "/application/models/".
	//
	// Every struct's non-omitempty, non-nilable fields are required where only responses reach
	// it, and nothing derived is required where a request reaches it (Go reads a missing key as
	// the zero value). A model is always treated as server-serialized: when requests and
	// responses share it, the component keeps its required list for the responses and the
	// requests reference a copy without it, named with RequestVariantSuffix
	// ("...IncomeToInvoiceInput"). A non-model type both sides share requires nothing.
	// readOnly properties stay required on both sides, and required lists a Schemas hook wrote
	// are left as they are.
	RequiredPackagePrefix string
	// TypeSchemas replaces the schema of a Go type, keyed by "importpath.TypeName", e.g.
	// {"github.com/shopspring/decimal.Decimal": {"type": "string", "format": "decimal"}}. The
	// type keeps its component, named as usual; the entry wins over the generator's mappings.
	// A struct that defines MarshalJSON (or embeds one that does) and a generic type must be
	// described here, generation fails otherwise. ExtendGenerated builds an entry from the
	// derived schema.
	TypeSchemas map[string]map[string]any
	// SchemaPrefixCollapse maps a module-relative package path prefix to the path every package
	// under it is named after, e.g. {"application/interfaces/rest/handler/":
	// "application/interfaces/rest/handler"} names all handler subpackages' types alike.
	SchemaPrefixCollapse map[string]string
	// SchemaNameAliases maps a module-relative package path to the schema name prefix its types
	// (and its subpackages' types) keep, so moving a package does not rename its schemas.
	SchemaNameAliases map[string]string
	// SchemaName names the component of a Go type, replacing the default name (the import
	// path relative to the module with SchemaNameAliases and SchemaPrefixCollapse applied, then
	// the type name, e.g. application_models_finance_Invoice). Generation fails when it gives
	// two types the same name, or a type the name of the error envelope (ErrorResponse,
	// StandardError). The generator finds its own types (pagination.CursorRequest, models,
	// resources) by the Go type a component was built from, so any naming works.
	SchemaName func(importPath string, typeName string) string
	// DocComments turns Go comments into descriptions: a type's doc comment on its component
	// schema, a struct field's doc and line comments on its property, an enum constant's line
	// comment (or a doc comment starting with its name) in x-enum-descriptions and
	// x-enumDescriptions, a handler method's doc comment (else its module function's) on its
	// operation, and a package's doc comment on its tag (see TagDescriptions). Off by default:
	// comments are often notes for maintainers rather than API documentation.
	DocComments bool
	// SchemaTags picks the operation tags a component schema is filed under, written as its
	// x-tags. derived holds the tags deriveSchemaTags found for the schema's Go package (pkgPath)
	// in sorted order; nil keeps them when there is exactly one. Return nil to write none.
	SchemaTags func(name string, pkgPath string, derived []string) []string
	// ResourceTag names the child tag of a resource: a package handlers take their request types
	// from (see tagResources). entity is the Go name of the model the resource's handlers
	// return, "" when none; name is the generator's choice ("Bank accounts"). Return "" to make
	// no child tag, leaving the package's schemas under their operation tag. Nil keeps name.
	ResourceTag func(pkgPath string, entity string, name string) string

	// PathSummaries overrides the summary of the operations on an OpenAPI path.
	PathSummaries map[string]string
	// SummaryVerbs are the leading words a summary is kept as is with; other GET and POST
	// summaries get a "Get" or "Create" prefix. Defaults to DefaultSummaryVerbs.
	SummaryVerbs []string
	// MaxPageSize, when positive, is the largest limit a paginated list accepts, documented as
	// the maximum of every limit query parameter.
	MaxPageSize int
	// TagNames overrides the tag of a namespace, e.g. {"crm": "CRM"}.
	TagNames map[string]string
	// TagDescriptions describes tags by name, operation tags and resource child tags alike.
	// A tag without an entry is described, with DocComments, by its package's doc comment
	// ("Package finance ..."): the handler package of a namespace in nested mode, the module
	// package of a resource.
	TagDescriptions map[string]string

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
	// "logDeprecated". Deprecated routes are documented `deprecated: true` and
	// `x-internal: true`: in the full spec for the clients still calling them, left out of the
	// public one (see Spec.Public and DeprecatedOperationSuffix).
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
	// ContentTypes returns the response content types the handler's compose calls leave open:
	// a BlobResponse whose content type is not a constant, or a handler without a compose
	// response call. It cannot replace what the calls pin down (JSONResponse and
	// BindedJSONResponse serve application/json, BlobResponse with a constant serves that
	// type), only add to it. nil falls back to NegotiatedContentTypes for an open BlobResponse
	// or a negotiating handler, application/json otherwise.
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
	Title          string
	Version        string
	Description    string
	TermsOfService string
	Contact        *Contact
	License        *License
}

// Contact is the info object's contact.
type Contact struct {
	Name  string
	URL   string
	Email string
}

// License is the info object's license.
type License struct {
	Name string
	URL  string
}

// ExternalDocs is the document's externalDocs object.
type ExternalDocs struct {
	Description string
	URL         string
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
	// Description tells clients how to obtain and send the key.
	Description string
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
	// DefaultArg, when positive, is the position of the default value the call falls back to;
	// an integer literal there becomes the parameter's default (GetListLimit(c, 50)).
	DefaultArg int
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
			Schema:     map[string]any{"type": "integer", "format": "int32", "minimum": 1},
			Name:       "limit",
			DefaultArg: 1,
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
