// Package mcp exposes a REST API described by an OpenAPI 3.x document as a
// Model Context Protocol server with a fixed set of tools:
//
//   - whoami: who the caller is.
//   - search_functions: what exists, by module or free text, one dense line per function.
//   - describe_function: how to call one or more functions (typed args, return type).
//   - explain_term: the glossary (schemas and concepts, with the functions that use them).
//   - call_function: invoke any function through the REST handler in-process.
//   - run_script: optional, run a short JavaScript program that chains calls.
//
// The catalog is derived from the document alone. Modules are the operation
// tags, functions are the operations, terms are the named component schemas.
// Nothing in this package knows about a particular application; projects add
// composites and concepts through Config.
//
// Mounting on an Echo app that already serves its OpenAPI document:
//
//	server, err := mcp.New(mcp.Config{
//		Spec:         openapiJSON,
//		Handler:      e,
//		Instructions: "Acme manages widgets and the people who order them.",
//		WhoAmI:       "accounts.me",
//		Concepts:     map[string]mcp.Concept{"Order": {Text: "A purchase placed by a customer.", Aliases: []string{"pedido"}}},
//		Script:       mcp.ScriptConfig{Enabled: true},
//	})
//	if err != nil {
//		return ez.Wrap(err)
//	}
//	e.Any("/mcp", echo.WrapHandler(server.Handler()))
//
// Clients connect over streamable HTTP with "Authorization: Bearer key:secret";
// the default Auth maps that token to the API-KEY and API-SECRET headers. The
// server is stateless, so it runs behind any load balancer. Hosted clients
// that cannot hold an API key, such as claude.ai and ChatGPT connectors, log
// in through the OAuth server of the oauth subpackage, which supplies Auth
// and guards the endpoint.
//
// Server.Snapshot returns the catalog as JSON; commit it and compare it in a
// test to catch unintended changes. Server.Reference renders a page that
// shows every tool, function and term exactly as the agent sees them.
package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/vanclief/ez"

	"github.com/vanclief/compose/components/rest/reference"
)

// Config configures a server. Spec and Handler are required.
type Config struct {
	// Name identifies the server to clients. Defaults to info.title.
	Name string
	// Version is reported to clients. Defaults to info.version.
	Version string
	// Spec is the OpenAPI 3.x document, JSON or YAML.
	Spec []byte
	// Handler serves the REST API. call_function builds an HTTP request and
	// dispatches it to Handler in-process, so auth, validation and errors
	// are exactly those of the REST API.
	Handler http.Handler
	// Instructions is the application paragraph placed before the generated
	// module list in the server instructions.
	Instructions string
	// Auth maps the bearer token of an MCP request to the headers the REST
	// API authenticates with. Defaults to BearerToHeaders("API-KEY", "API-SECRET").
	Auth AuthFunc
	// WhoAmI names the catalog function that whoami calls with no arguments,
	// for example "users.me"; New refuses a function with a required
	// argument. Empty makes whoami report the server identity only.
	WhoAmI string
	// Composites are hand-written functions added to the catalog.
	Composites []Composite
	// Concepts adds glossary entries for terms that are not schemas, or sets
	// the description and aliases of a schema term. Keys are term names.
	Concepts map[string]Concept
	// Script enables and bounds run_script.
	Script ScriptConfig
	// MaxOutputBytes caps the text of one tool result. Longer results are cut
	// with a hint to narrow the request. Defaults to 24000 (about 6k tokens).
	MaxOutputBytes int
	// OnError, when set, receives failures the agent sees only as generic
	// text: a panic in a tool handler or in a function a script calls, with
	// its stack, and internal failures of the script runtime. Optional.
	OnError func(ctx context.Context, err error)
}

// AuthFunc turns the bearer token of an MCP request into REST headers. An
// error means the token is not accepted; an empty token yields no headers.
type AuthFunc func(ctx context.Context, token string) (http.Header, error)

// Concept is a glossary entry supplied by the project.
type Concept struct {
	// Text is the paragraph explaining the term.
	Text string
	// Aliases are other names the agent may use for the term, in any language.
	Aliases []string
}

// ScriptConfig bounds run_script. The zero value disables it.
//
// Runtime: each script runs in a fresh QuickJS runtime compiled to
// WebAssembly and executed by wazero inside this process. The module is
// compiled once, when New enables scripts, and instantiated anew for every
// run, so nothing survives from one script to the next. The guest sees only
// its own linear memory; it cannot read or write Go memory or the Go stack.
//
// Memory: the embedded quickjs.wasm declares a 64 MiB maximum for its
// linear memory and the glue's allocator refuses past 48 MiB, so a script
// that allocates without end fails with a JavaScript out-of-memory error
// instead of growing the process. A script that catches that error and
// goes on allocating without freeing gets a few more such errors, then
// null, since QuickJS cannot build an error with no memory at all; an
// uncaught one is always reported as out of memory. On unix each run's
// memory is an anonymous mapping outside the Go heap, committed as the
// script touches it and released when the run ends, and a run whose
// memory cannot be mapped is refused with ERESOURCEEXHAUSTED and reported
// to OnError, since a limit on the process makes every run fail that way;
// elsewhere wazero grows it on the Go heap, copying as it grows, which can
// allocate a few hundred MiB for a run that fills its memory. The binary
// is this package's own build of quickjs-ng; quickjs/README.md and
// script.go record the recipe and the checksum.
//
// Stack: a few thousand nested calls fit; a deeper recursion throws a
// RangeError the script can catch. The runtime stack sits at the bottom of
// linear memory, so native code that overflows it traps and the script
// fails with "the script exhausted the runtime stack or memory".
//
// Timeout: the deadline is checked inside the compiled code, so native
// builtins such as JSON.stringify, regular expressions and array scans stop
// at the timeout too, and the runtime is then discarded. The timeout covers
// the whole run, function calls included. Go host code cannot be
// interrupted, so a Caller that ignores its context can hold a script, and
// its slot, past the timeout.
//
// Reach: the guest has no network, no environment, no files, no timers and
// no modules: QuickJS's standard library is not linked, so there is no std,
// os or bjson module, and there is no module loader, so every import()
// fails. Its stdout and stderr are discarded. Credentials never enter the
// guest; its only way out is two host functions, one that calls a catalog
// function and one that records a console.log line.
//
// Go enforces every rule on that way out: the function must exist in the
// catalog and be read-only unless AllowWrites is set, MaxCalls bounds the
// calls, the arguments must be a JSON object under a size cap, a response
// over its cap becomes an Error the script can catch instead of entering the
// guest, a result over its cap fails the script, and console.log output is
// capped in lines and bytes. The source of a script is capped as well. A
// panic in a function call reaches the script as a generic error and
// Config.OnError with its stack. The tool result puts the script's result,
// or its error, first and the console.log lines after it, so the output cap
// of the tool cuts logs, not the result.
//
// Concurrency: MaxConcurrent bounds the scripts running at once across all
// callers, and each caller runs one script at a time. Both refusals are
// ERESOURCEEXHAUSTED: a caller whose script is still running is refused at
// once, and a script that finds every slot busy waits briefly first. Callers
// are told apart by a hash of their bearer token, or of the JSON their
// WhoAmI call returns when the request carries none. Every request claims
// its caller before it takes a slot: by its bearer token before the WhoAmI
// call, or by the WhoAmI response after it, so it is refused at once while
// its own script is still running. Every request makes the WhoAmI call and
// the Allow check before it takes a slot, so a caller without credentials
// the API accepts never takes one. A script takes only the memory it uses,
// up to the linear memory maximum, so the scripts of one Server hold at most
// MaxConcurrent times that maximum plus overhead; several Servers
// in one process each allow that many. The ceiling does not bound the REST
// handler or the rest of the process, and decoding a call's arguments in
// Go can briefly cost tens of MiB of heap per MiB of JSON.
//
// Who may run scripts: New requires Config.WhoAmI when run_script is
// enabled, and every run first calls that function with the caller's
// credentials and refuses the script when the call fails. BearerToHeaders,
// the usual Fallback of the oauth package, only parses the token and
// accepts any key:secret pair, so the WhoAmI call is what keeps anonymous
// callers out; name a function that fails without valid credentials. Set
// Allow to limit scripts to callers the operator trusts, for example by
// role in the WhoAmI response, and keep run_script disabled otherwise.
//
// Limit: this is an in-process sandbox, not a process boundary. It keeps a
// script from crashing the process through recursion, from running a native
// builtin past the timeout and from exhausting the Go heap, but a bug in
// wazero's compiler would be a way out, and a script can keep one CPU core
// busy for the whole timeout. The WhoAmI call still keeps anonymous callers
// out, and Allow is still recommended for callers the operator does not
// fully trust.
type ScriptConfig struct {
	// Enabled registers the run_script tool.
	Enabled bool
	// Allow, when set, decides per caller whether run_script may run: it
	// receives the JSON the WhoAmI function returned for the caller's
	// credentials. Use it to limit scripts to operators, for example by
	// role. Nil allows every caller WhoAmI accepts.
	Allow func(ctx context.Context, whoami json.RawMessage) bool
	// AllowWrites lets scripts call functions that are not read-only.
	AllowWrites bool
	// Timeout stops the script, function calls included. Defaults to 5s.
	Timeout time.Duration
	// MaxCalls bounds the function calls one script may make. Defaults to 200.
	MaxCalls int
	// MaxConcurrent bounds the scripts running at once across all callers;
	// defaults to 4. Each caller runs one script at a time.
	MaxConcurrent int
}

// Caller invokes catalog functions with the credentials of the current MCP
// request. Composites and scripts use it so every call goes through the REST
// handler.
type Caller interface {
	// Call invokes the function by catalog name with a flat argument object
	// and returns the response body. Errors carry the ez code of the REST
	// error.
	Call(ctx context.Context, name string, args map[string]any) (json.RawMessage, error)
}

// Composite is a hand-written function that lives in a module next to the
// functions derived from the document. Build one with NewComposite.
type Composite struct {
	// Module is the module name the function is listed under.
	Module string
	// Name is the function name inside the module, snake_case.
	Name string
	// Summary is the one-line description shown by search_functions.
	Summary string
	// Description is the longer text shown by describe_function.
	Description string
	// ReadOnly marks the composite as safe for read-only scripts.
	ReadOnly bool
	// Input and Output are zero values of the argument and result types.
	// Their JSON Schemas are derived by reflection.
	Input  any
	Output any
	// Run executes the composite with the decoded arguments.
	Run func(ctx context.Context, call Caller, args json.RawMessage) (any, error)
}

// NewComposite builds a Composite from a typed run function. In and Out are
// structs with json tags; their schemas are derived by reflection.
func NewComposite[In, Out any](module, name string, run func(context.Context, Caller, In) (Out, error)) Composite {
	var in In
	var out Out
	return Composite{
		Module: module,
		Name:   name,
		Input:  in,
		Output: out,
		Run: func(ctx context.Context, call Caller, raw json.RawMessage) (any, error) {
			var args In
			if len(raw) > 0 {
				err := json.Unmarshal(raw, &args)
				if err != nil {
					return nil, ez.New(ez.EINVALID, "mcp: arguments of "+module+"."+name+" do not match: "+err.Error(), err)
				}
			}
			return run(ctx, call, args)
		},
	}
}

// Catalog is the agent-facing view of the document: modules holding
// functions, and terms. Order is deterministic so tool output and snapshots
// are stable.
type Catalog struct {
	Modules []*Module
	Terms   []*Term

	functions map[string]*Function
	modules   map[string]*Module
	terms     map[string]*Term
	// exact maps exact term names, aliases and component refs to their term,
	// so "User" finds its term when "user" holds the lowercase lookup key.
	exact map[string]*Term
}

// Module groups the functions of one tag.
type Module struct {
	// Name is the snake_case tag name.
	Name string
	// Description is the tag description, when the document has one.
	Description string
	// Functions are sorted by name.
	Functions []*Function
}

// Function is one operation or composite as the agent sees it.
type Function struct {
	// Name is the full name, "module.function".
	Name string
	// Module is the module name.
	Module string
	// Summary is the one-line description.
	Summary string
	// Description is the longer text, may be empty.
	Description string
	// Method and Path are the HTTP operation; empty for composites.
	Method string
	Path   string
	// Args is the flat argument list: path, query and header parameters
	// followed by the body properties. When a body property name collides
	// with a parameter the body is exposed as one "body" argument instead.
	Args []*Arg
	// Returns is the term name of the success response, or empty.
	Returns string
	// ReturnSchema is the resolved success response schema, or nil.
	ReturnSchema *reference.Schema
	// Deprecated mirrors the operation flag.
	Deprecated bool
	// Composite is true for hand-written functions.
	Composite bool
	// ReadOnly is true for GET and HEAD operations and read-only composites.
	ReadOnly bool
	// Destructive is true for DELETE operations.
	Destructive bool

	composite *Composite
	// bodyWhole is true when the request body is the single "body" argument
	// rather than one argument per property.
	bodyWhole bool
}

// Arg is one argument of a function.
type Arg struct {
	// Name is the argument name as the agent passes it.
	Name string
	// In is where the REST request carries it: path, query, header or body.
	In string
	// Required mirrors the parameter flag or the body required list.
	Required bool
	// Description is the parameter or property description.
	Description string
	// Schema is the resolved schema of the argument.
	Schema *reference.Schema

	// param is the document parameter of a path, query or header argument,
	// nil for body arguments. Dispatch serializes the value with its style.
	param *reference.Parameter
}

// Term is a glossary entry: a named schema, a project concept, or both.
type Term struct {
	// Name is the display name, unique within the catalog.
	Name string
	// Description is the schema description or the concept text.
	Description string
	// Aliases are other names that resolve to the term.
	Aliases []string
	// Schema is the component schema, or nil for concept-only terms.
	Schema *reference.Schema
	// AcceptedBy lists the functions whose arguments reference the schema.
	AcceptedBy []string
	// ReturnedBy lists the functions whose success response is the schema.
	ReturnedBy []string
}

// Function returns the function with the given full name.
func (c *Catalog) Function(name string) (*Function, bool) {
	f, ok := c.functions[name]
	return f, ok
}

// Module returns the module with the given name.
func (c *Catalog) Module(name string) (*Module, bool) {
	m, ok := c.modules[name]
	return m, ok
}

// Term returns the term by exact name, else by name or alias
// case-insensitively.
func (c *Catalog) Term(name string) (*Term, bool) {
	t, ok := c.exact[strings.TrimSpace(name)]
	if ok {
		return t, true
	}
	t, ok = c.terms[normalizeTerm(name)]
	return t, ok
}

// Functions lists every function in module order, then name order.
func (c *Catalog) Functions() []*Function {
	var out []*Function
	for _, m := range c.Modules {
		out = append(out, m.Functions...)
	}
	return out
}

// referenceInfo is what the reference page needs besides the catalog.
type referenceInfo struct {
	Name         string
	Version      string
	Instructions string
	Tools        []toolInfo
}

// toolInfo is one entry of tools/list as the reference page shows it.
type toolInfo struct {
	Name        string
	Description string
	// InputSchema is the JSON Schema of the tool arguments.
	InputSchema json.RawMessage
}
