// Package generator derives an OpenAPI 3.0 document from the source of an Echo application
// built on compose's rest handlers. It reads the code purely syntactically (go/parser, no
// type checking): route files give the operations, handler bodies give the request type, the
// path and query parameters and the content types, and the request type's module function
// gives the response type. Everything application-specific comes in through Config.
//
// The document follows the encoding/json of the toolchain that builds the generator, which is the
// app's own when it runs with `go run` inside the app's module. With the jsonv2 experiment, on by
// default from Go 1.27 (build tag goexperiment.jsonv2), encoding/json runs on encoding/json/v2: it
// also writes a value with MarshalJSONTo(*jsontext.Encoder) error, before MarshalJSON, and with
// AppendText([]byte) ([]byte, error), before MarshalText, and json.Number has a MarshalJSONTo,
// which a struct embedding one gets; a json tag name is taken whole unless it holds a backslash,
// a quote or a backtick, and then up to the first character other than a letter, a number or an
// underscore, as "it" for it's, or not at all, the Go field name being used, when it starts with
// neither a letter nor an underscore. The generator counts a method of either name only with that
// signature, read within the type's own package, whose aliases it follows, one with another
// signature still hiding a method of its name promoted from deeper; a signature it cannot read,
// one that names a type of a package outside the standard library, a type a dot import may
// declare, a generic alias, a type parameter or a type declared differently in several files, fails
// generation for that type until Config.TypeSchemas describes it, and for a struct that embeds
// the type, described or not. Without the experiment, as on Go 1.26 or with
// GOEXPERIMENT=nojsonv2, encoding/json writes with MarshalJSON and MarshalText
// alone and takes a json tag name only when it is made of letters, digits, spaces and
// !#$%&()*+-./:;<=>?@[]^_{|}~, using the Go field name for any other. jsonrules_v1.go and
// jsonrules_v2.go hold the two rule sets. The v2 rules are Go 1.27's; Go 1.25 and 1.26 with
// GOEXPERIMENT=jsonv2 run an earlier encoding/json/v2.
//
// Of the packages it reads types and module functions from, the app's and its dependencies', the
// generator reads every file that some GOOS/GOARCH, with cgo on or off, builds, and leaves out one
// that none builds, such as one marked //go:build ignore or needing a custom build tag; the files
// HandlerGlob matches, test files aside, are read whatever their build constraints, those
// RoutesGlob matches only when a default build on the platform the generator runs on compiles
// them (see RoutesGlob), and a file that needs a tag go/build sets for an architecture's feature
// level, such as amd64.v3, is not read. A type
// declared differently in several of the files read, as one declared per platform may be, fails
// generation wherever the generator builds its schema, not as a map key, which it does not read,
// until Config.TypeSchemas describes it, as the generator does not pick one of the declarations.
// Declarations that are the same, in their type parameters and type, an alias or not, field
// names, field order, struct tags and Deprecated: field comments included, and with DocComments
// the doc comment and field comments too, leaving out fields neither exported nor embedded, and in
// the import path of each package name they qualify an identifier with and of a dot import, are
// read as one, however they are laid out: a struct tag is compared by the value
// reflect.StructTag reads for each key, a field comment by the description and deprecation the
// generator reads from it, and parentheses, blank lines and names declared together, as in
// A, B int, do not count.
// A type declared in only one of the files read is read from it, whichever platforms build it. A
// marshal method declared on fewer platforms than its type, through a file-name suffix such as
// _windows.go, a //go:build line or an import of "C", and not outranked by a marshal method
// declared on all of them, fails generation wherever the type's methods are read, for the type
// and for a struct that embeds it, even one whose own method or a tie leaves it uncalled (a type
// declared from it gets none of them), and so does such an IsZero() bool on the type of an
// omitzero slice or map field, unless Config.TypeSchemas describes the type, as encoding/json
// would write the type differently by platform. The goexperiment.jsonv2 tag files
// are selected with is the one the generator is built with, its other GOEXPERIMENT tags those of
// the environment it runs in. The standard library is read for the platform the generator runs on.
//
// A json tag with a format: option on an exported or embedded field, which encoding/json refuses
// for the whole struct on Go 1.27 unless built with GOEXPERIMENT=nojsonv2 and ignores on Go 1.26
// unless built with GOEXPERIMENT=jsonv2, fails generation on every toolchain for its struct and
// for a struct it is promoted into, from an embedded struct described or not, unless
// Config.TypeSchemas describes that struct; an embedded struct declared differently in several
// files is not read for one. An unsigned integer has minimum 0.
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
	// Test files and files a default build on the platform the generator runs on leaves out, by
	// their name (routes_windows.go), their build constraints (//go:build ignore) or, with cgo
	// off, an import of "C", are not read, so the document is the one of that platform. Each
	// other file is parsed as Go, and routes are the calls `group.GET(path, h.Method, ...)` (or
	// POST, PUT, PATCH, DELETE, HEAD), on one line or several. A non-empty path written without
	// a leading slash is documented with one, as Echo serves it. A path or Group prefix is a
	// string constant: a literal, a constant declared in the function or at the top level of the
	// file's package, or such strings joined with +. A group is a RootGroups variable, a variable
	// set with `:=`, `=` or var to a group, or a Group call on a group, chained as deep as
	// written: `e.Group("/v1").GET("/x", ...)`.
	// Group variables follow Go's scopes: one declared in a block or another function is not seen
	// outside it, and `=` changes the prefix from that statement on. A route call whose path or
	// group the finder cannot resolve fails generation unless SkipUnresolved leaves it out: among
	// others, a group in a parameter not named in RootGroups, or in a variable set to anything
	// but a group, set to another group in a branch, a loop or a function literal and used after
	// it or in that loop or literal (unless every branch of an if with an else, or of a switch
	// with a default, sets it to the same known group), assigned at all in such a body that a
	// break, continue, goto, fallthrough or a return from a function literal may leave early,
	// reassigned anywhere in a function that has a goto, assigned in a loop or a function
	// literal, written through as `*g = ...`, whose address is taken, or used in a function
	// literal while `=` assigns it anywhere in the function. A RootGroups name that is a
	// parameter, declared without a value or set to a call other than Group or to a field, such
	// as `e := echo.New()`, `e, err := newEcho()` or `e := s.echo`, stays its root group; one set
	// to a variable holds what the variable holds, and a named result, a range variable or one
	// set to anything else holds no known group. The finder does not follow calls or fields, so a
	// RootGroups name set to a call other than Group or to a field is taken for its root group
	// whatever the call or field returns: such a name must only ever be set to a root group. A
	// route method used as a value, `get := e.GET`, fails the same way, so does a field named like
	// one (`p.GET`) other than a handler written as a route call reads one (`h.GET` in flat mode,
	// `h.Things.GET` in nested mode), and a route registered with Any, Match, Add, AddRoute,
	// CONNECT, OPTIONS, TRACE or QUERY, with the arguments Echo takes, on a known group, a Group
	// call or a variable that has held a known group, which the document would leave out;
	// SkipUnresolved can leave them out.
	// RouteNotFound, Use, Static, File and other calls that register no API route are not read.
	// Two routes in the document, those Skip does not leave out, whose paths differ only in the
	// names of their parameters, /things/:id and /things/:name, fail generation, as OpenAPI takes
	// them for one path. Not seen: a function literal that sets a group and sets it back around a
	// call that panics under a recover, or that runs in a goroutine; and a group written through a
	// copy of its pointer (`p := g` then `*p = ...`).
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
	// A name set to a call other than Group or to a field is taken for its root group whatever
	// the call or field returns (see RoutesGlob), so such a name must only ever be set to a root
	// group.
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
	// A struct that defines MarshalJSON (or MarshalJSONTo, see the package doc), a struct into
	// whose method set Go promotes a marshal method of an embedded type (MarshalJSON or
	// MarshalText, and MarshalJSONTo or AppendText), described here or not, and a generic type
	// other than sql.Null[T] must be described here, generation fails otherwise. The generator
	// does not substitute a generic type's arguments into its fields, so this holds too for a
	// type declared from an instantiation of a generic type with either method, which it does
	// not get, and for a struct that is one object, as encoding/json resolves its embedded
	// fields, and embeds an instantiation, an alias of one or a type declared from one that its
	// description cannot stand for as an allOf member: one embedded through a pointer at any
	// depth, one with a field name that another field of the struct the generator reads
	// shares, one with MarshalJSON or MarshalText that a tie leaves unpromoted, or one whose
	// declaration embeds a type. Any
	// other is that allOf member, as its field names do not depend on its type arguments. It
	// holds as well for a struct embedding a type from a package missing from the module cache,
	// where that type could promote one. A standard-library type the generator does not list is
	// read from GOROOT's source, from the files selected for the platform the generator runs on,
	// and generation fails wherever one is needed, as a field or embedded, when that source or
	// one of its files cannot be read. A type with MarshalText (or AppendText) and without
	// MarshalJSON (or MarshalJSONTo) is a string unless described here. A type of the app or a
	// dependency with a marshal method only some of the platforms it is declared on build must be
	// described here too wherever its methods are read, for itself and for a struct that embeds
	// it, and one with such an IsZero() bool where it is the type of an omitzero slice or map field
	// (see the package doc). So must a type declared differently in several files, as one declared
	// per platform may be (see the package doc), and a struct with an exported or embedded field,
	// its own or promoted from an embedded struct, described here or not unless declared
	// differently in several files, whose json tag has a format: option, which encoding/json
	// refuses for the whole struct on Go 1.27 unless built with GOEXPERIMENT=nojsonv2 and ignores
	// on Go 1.26 unless built with GOEXPERIMENT=jsonv2.
	// ExtendGenerated builds an entry from the derived schema.
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
	// MaxPageSize, when positive, is the page size the server clamps a larger limit to,
	// documented as the maximum of every limit query parameter.
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
	// DefaultParamGetters. A path parameter the handler's body reads with none of them, with
	// neither echo's PathParam[T] nor PathParamOr[T] (T's schema for bool, string, a float or a
	// predeclared integer type other than uintptr, with T read only from the type argument or,
	// for PathParamOr without one, from a default written as a conversion such as int64(0)),
	// each passing the name as a string literal or a constant declared with one, nor through a
	// param-tagged field of the request a BodyBinders call binds, is a string, whatever its name;
	// the Operation hook can set its schema.
	ParamGetters map[string]ParamGetter
	// ResponseCalls are the calls whose last argument is the handler's request type. Defaults to
	// JSONResponse, BindedJSONResponse, BindedXMLResponse and BlobResponse.
	ResponseCalls []string
	// BodyBinders are the calls that bind a request body, so DELETE handlers using them get one.
	// Defaults to BindedJSONResponse and BindedXMLResponse. A param-tagged field of the request
	// such a call binds documents its path parameter as echo's default binder binds it, so an app
	// whose binder does not bind path values sets the parameter's schema in the Operation hook.
	BodyBinders []string
	// Negotiate names the call a handler negotiates its response content type with. Handlers
	// calling it respond with NegotiatedContentTypes.
	Negotiate              string
	NegotiatedContentTypes []string
	// DeprecatedWrapper names the function wrapping deprecated route handlers, e.g.
	// "logDeprecated", which also matches a call written with any qualifier, such as
	// mw.logDeprecated(...), or "mw.Deprecated", which matches only calls written with exactly
	// that qualifier. A route is
	// deprecated when its call has a call of that function among the arguments after the
	// path. Deprecated routes are documented `deprecated: true` and
	// `x-internal: true`: in the full spec for the clients still calling them, left out of the
	// public one (see Spec.Public and DeprecatedOperationSuffix).
	DeprecatedWrapper string

	// Skip leaves an operation out of the document. path is the OpenAPI path ({id}, not :id).
	Skip func(method string, path string) bool
	// SkipUnresolved leaves out a route registration the route finder cannot resolve (see
	// RoutesGlob), which has no path Skip could be asked about: a route call, a route call
	// whose only argument is a call, a route registered with a method the generator does not
	// document, such as Any, or a route method used as a value, for which, as for AddRoute,
	// UnresolvedRoute.Path is empty. Without it, or when it returns
	// false, generation fails and names the registration, its file and line, and what was not
	// resolved. Leaving one out keeps it out of Spec.Routes too.
	SkipUnresolved func(route UnresolvedRoute) bool
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

// SecurityScheme is an API key sent in a request header or an http (bearer) credential.
// The global requirement puts every apiKey scheme in one requirement object, so they must
// be sent together, and each http scheme in its own, so each is an alternative credential.
// Public operations opt out.
type SecurityScheme struct {
	Name string
	// Type is "apiKey" (default, header named by Header) or "http".
	Type string
	// Header names the header of an apiKey scheme.
	Header string
	// Scheme is the http scheme, "bearer". Required when Type is "http".
	Scheme string
	// BearerFormat is a hint for clients, for example "opaque".
	BearerFormat string
	// Description tells clients how to obtain and send the credential.
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
		"GetQueryParams": {
			Schema: map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "string"},
			},
			NameArg: 1,
		},
		"GetQueryParamBool": {
			Schema:  map[string]any{"type": "boolean"},
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
	for _, scheme := range cfg.Security {
		switch scheme.Type {
		case "", "apiKey":
		case "http":
			if scheme.Scheme == "" {
				return cfg, ez.New(ez.EINVALID, "generator: security scheme "+scheme.Name+" of type http needs a Scheme", nil)
			}
		default:
			return cfg, ez.New(ez.EINVALID, "generator: security scheme "+scheme.Name+" has unknown type "+scheme.Type, nil)
		}
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
