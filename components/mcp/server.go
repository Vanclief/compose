package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/vanclief/ez"

	"github.com/vanclief/compose/components/rest/reference"
)

// defaultMaxOutputBytes caps one tool result when Config.MaxOutputBytes is
// not set: about 6k tokens.
const defaultMaxOutputBytes = 24000

// Server is an MCP server over one REST API. Build it with New and mount
// Handler on an HTTP route.
type Server struct {
	cfg          Config
	catalog      *Catalog
	srv          *mcp.Server
	tools        []registeredTool
	maxOutput    int
	cutHint      string
	instructions string
	scripts      *scriptLimiter
	engine       *scriptEngine
}

// New parses the spec, builds the catalog and registers the tools.
func New(cfg Config) (*Server, error) {
	if len(cfg.Spec) == 0 {
		return nil, ez.New(ez.EINVALID, "mcp: Config.Spec is required", nil)
	}
	if cfg.Handler == nil {
		return nil, ez.New(ez.EINVALID, "mcp: Config.Handler is required", nil)
	}

	doc, err := reference.Parse(cfg.Spec)
	if err != nil {
		return nil, ez.Wrap(err)
	}
	catalog, err := buildCatalog(doc, cfg.Composites, cfg.Concepts)
	if err != nil {
		return nil, ez.Wrap(err)
	}

	if cfg.Name == "" {
		cfg.Name = doc.Info.Title
	}
	if cfg.Version == "" {
		cfg.Version = doc.Info.Version
	}
	if cfg.Auth == nil {
		cfg.Auth = BearerPassthrough()
	}
	if cfg.MaxOutputBytes <= 0 {
		cfg.MaxOutputBytes = defaultMaxOutputBytes
	}
	if cfg.WhoAmI != "" {
		f, ok := catalog.Function(cfg.WhoAmI)
		if !ok {
			return nil, ez.New(ez.EINVALID, fmt.Sprintf("mcp: Config.WhoAmI names unknown function %q", cfg.WhoAmI), nil)
		}
		// whoami calls the function with no arguments.
		for _, arg := range f.Args {
			if arg.Required {
				return nil, ez.New(ez.EINVALID, fmt.Sprintf("mcp: Config.WhoAmI names function %q, whose argument %s is required; it must take none", cfg.WhoAmI, arg.Name), nil)
			}
		}
	}
	if cfg.Script.Enabled && cfg.WhoAmI == "" {
		return nil, ez.New(ez.EINVALID, "mcp: Config.Script.Enabled requires Config.WhoAmI", nil)
	}
	cfg.Tools = slices.Clone(cfg.Tools)
	promoted := map[string]bool{}
	for _, name := range cfg.Tools {
		f, ok := catalog.Function(name)
		if !ok {
			return nil, ez.New(ez.EINVALID, fmt.Sprintf("mcp: Config.Tools names unknown function %q", name), nil)
		}
		if !toolNamePattern.MatchString(f.ToolName()) {
			return nil, ez.New(ez.EINVALID, fmt.Sprintf("mcp: Config.Tools names %q, whose tool name %q breaks claude.ai's rule of 1 to 64 characters from [a-zA-Z0-9_-]", name, f.ToolName()), nil)
		}
		if promoted[name] {
			return nil, ez.New(ez.EINVALID, fmt.Sprintf("mcp: Config.Tools names %q twice", name), nil)
		}
		promoted[name] = true
	}

	// cutHint ends the note of a cut result that succeeded: catalog output
	// and lists are narrowed by the request, one large function response
	// only by a script, which gets it whole. Every way to get the rest runs
	// the call again, which only a read can afford, and Script.Allow may
	// still refuse the caller, hence the hedges.
	cutHint := "Narrow the request: a module, a query or fewer names or terms for a catalog tool, filters or a smaller page size for a read-only function."
	if cfg.Script.Enabled {
		cutHint += fmt.Sprintf(" For one large response of a read-only function, use run_script (where allowed): the script receives the whole response (up to %d MiB, memory permitting) and returns only the field or slice you need.", maxScriptResponseBytes>>20)
	}
	cutHint += " Do not repeat a write."

	s := &Server{
		cfg:       cfg,
		catalog:   catalog,
		maxOutput: cfg.MaxOutputBytes,
		cutHint:   cutHint,
		scripts:   newScriptLimiter(cfg.Script.MaxConcurrent),
	}
	if cfg.Script.Enabled {
		// Build the shared script runtime now so the first script does not
		// pay for it.
		s.engine, err = sharedScriptEngine()
		if err != nil {
			return nil, ez.Wrap(err)
		}
	}
	s.instructions = s.buildInstructions()
	s.srv = mcp.NewServer(&mcp.Implementation{Name: cfg.Name, Version: cfg.Version}, &mcp.ServerOptions{Instructions: s.instructions})

	err = s.registerTools()
	if err != nil {
		return nil, ez.Wrap(err)
	}
	return s, nil
}

// Handler serves the MCP streamable HTTP transport. It is stateless and
// answers with plain JSON, so it runs behind any load balancer.
//
// Handler completes X-Forwarded-For as a proxy would, before the transport
// sees the request: the transport gets a copy carrying one X-Forwarded-For
// line, every received line in order joined with ", ", followed by the host
// of RemoteAddr, the peer that connected to the MCP endpoint (RemoteAddr
// whole when it does not split into host and port). Tool calls copy that
// line onto the REST requests they dispatch in-process, whose peer stays
// 192.0.2.1:1234, so without it the REST API could not tell who connected:
// it would see only the dispatch peer and whatever the client wrote. An app
// whose IP extractor should resolve the client's address on tool calls
// trusts 192.0.2.1/32, for example with echo.TrustIPRange; the extractor then
// walks the same chain as on a direct request. An app that wrapped Handler to
// append the peer itself should remove that wrapper: a doubled peer entry is
// harmless to a right-to-left walk but is noise.
func (s *Server) Handler() http.Handler {
	transport := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return s.srv
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		transport.ServeHTTP(w, withForwardedFor(r))
	})
}

// withForwardedFor returns a copy of r whose X-Forwarded-For is one line:
// the received lines joined in order, then the peer of r. A RemoteAddr that
// does not split into host and port is appended whole, so a client-written
// entry never ends up last.
func withForwardedFor(r *http.Request) *http.Request {
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		peer = r.RemoteAddr
	}
	chain := peer
	received := r.Header.Values("X-Forwarded-For")
	if len(received) > 0 {
		chain = strings.Join(received, ", ") + ", " + peer
	}
	r = r.Clone(r.Context())
	r.Header.Set("X-Forwarded-For", chain)
	return r
}

// Catalog returns the catalog the tools serve.
func (s *Server) Catalog() *Catalog {
	return s.catalog
}

// Call invokes a catalog function with the REST headers given, as
// call_function would for an MCP request carrying them. The headers are the
// credentials the REST API authenticates with (for example Authorization),
// set after the arguments, so an argument never overrides them. There is no
// MCP request to copy X-Forwarded-For or X-Request-Id from. header may carry
// X-Request-Id along, but X-Forwarded-For only with a client address the host
// resolved itself, for example its own c.RealIP(), never a copy of an
// incoming header, which would let a client name its own address. Without
// one the REST API sees the dispatch peer 192.0.2.1 for every call the host
// starts. Unlike a tool call, Call does not recover a panic in a composite
// nor cut the result to MaxOutputBytes; use CallTool for that.
func (s *Server) Call(ctx context.Context, header http.Header, name string, args map[string]any) (json.RawMessage, error) {
	call := &caller{s: s, header: header}
	raw, err := call.Call(ctx, name, args)
	if err != nil {
		return nil, ez.Wrap(err)
	}
	return raw, nil
}

// Tool describes one tool the server registers, as a client sees it in
// tools/list.
type Tool struct {
	Name        string
	Description string
	// InputSchema is the JSON Schema of the tool's arguments.
	InputSchema json.RawMessage
	// The hints are the tool's annotations as an MCP client reads them. A
	// destructiveHint or openWorldHint the server leaves unset is true, the
	// MCP default, and Destructive is false for a read-only tool, where MCP
	// gives the hint no meaning. A write promoted from Config.Tools that is
	// not Function.Destructive is therefore Destructive: MCP reads a false
	// destructiveHint as additive updates only, which PUT and PATCH are not.
	// Idempotent is idempotentHint as set, also on a read-only tool, where
	// MCP gives it no meaning either, so a read-only run_script reports false
	// and the other read-only tools true.
	ReadOnly    bool
	Idempotent  bool
	Destructive bool
	OpenWorld   bool
}

// Tools returns the tools the server registers, in registration order: the
// fixed set, run_script when scripts are enabled, then the functions of
// Config.Tools. A host that embeds its own agent offers these to its model
// and runs the calls through CallTool, so it never copies a description or
// schema by hand. Each call returns new copies.
func (s *Server) Tools() []Tool {
	tools := make([]Tool, 0, len(s.tools))
	for _, registered := range s.tools {
		tool := registered.tool
		out := Tool{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: inputSchemaJSON(tool),
		}
		hints := tool.Annotations
		if hints == nil {
			hints = &mcp.ToolAnnotations{}
		}
		out.ReadOnly = hints.ReadOnlyHint
		out.Idempotent = hints.IdempotentHint
		out.Destructive = !hints.ReadOnlyHint && (hints.DestructiveHint == nil || *hints.DestructiveHint)
		out.OpenWorld = hints.OpenWorldHint == nil || *hints.OpenWorldHint
		tools = append(tools, out)
	}
	return tools
}

// CallTool runs one tool of Tools with the REST headers given, as the
// transport runs it for an MCP request, and returns the same text and the
// same failed flag a client gets: tool errors such as invalid arguments, an
// unknown function or a REST error come back as text with failed set, the
// text of a tool that runs is cut to MaxOutputBytes, and a panic becomes an
// internal error that also goes to Config.OnError. The arguments of a fixed
// tool are validated against its input schema and decoded into its input
// before it runs, as the MCP SDK does for the transport, and a failure of
// either is the uncut text of a failed call, as the SDK answers it.
//
// The headers are the credentials the REST API authenticates with, used as
// given: Config.Auth does not run, and there is no bearer token, so whoami
// without Config.WhoAmI reports none and run_script keys its one-at-a-time
// limit by the WhoAmI response. There is no MCP request to copy
// X-Forwarded-For or X-Request-Id from. header may carry X-Request-Id along,
// but X-Forwarded-For only with a client address the host resolved itself,
// for example its own c.RealIP(), never a copy of an incoming header, which
// would let a client name its own address. Without one the REST API sees the
// dispatch peer 192.0.2.1 for every call the host starts.
//
// The error is ez.ENOTFOUND for a name that is not a registered tool, and
// ez.EINVALID when args cannot be encoded as JSON.
func (s *Server) CallTool(ctx context.Context, header http.Header, name string, args map[string]any) (string, bool, error) {
	i := slices.IndexFunc(s.tools, func(registered registeredTool) bool {
		return registered.tool.Name == name
	})
	if i < 0 {
		return "", false, ez.New(ez.ENOTFOUND, fmt.Sprintf("mcp: unknown tool %s; Server.Tools lists the tools", name), nil)
	}
	var raw json.RawMessage
	if args != nil {
		data, err := json.Marshal(args)
		if err != nil {
			return "", false, ez.New(ez.EINVALID, "mcp: arguments of "+name+" cannot be encoded as JSON: "+err.Error(), err)
		}
		raw = data
	}
	handle, invalid := s.tools[i].prepare(raw)
	if handle == nil {
		// The SDK answers arguments it cannot validate or decode before the
		// handler runs, uncut.
		return invalid, true, nil
	}
	call := &caller{s: s, header: header}
	text, failed := s.runTool(ctx, name, func() (string, bool) {
		return handle(ctx, nil, call)
	})
	return text, failed, nil
}

// Instructions returns the server instructions sent to clients on initialize.
func (s *Server) Instructions() string {
	return s.instructions
}

// buildInstructions joins the application paragraph, the module list and the
// workflow rules.
func (s *Server) buildInstructions() string {
	var parts []string
	intro := strings.TrimSpace(s.cfg.Instructions)
	if intro != "" {
		parts = append(parts, intro)
	}

	lines := []string{"Modules (pass one to search_functions):"}
	for _, m := range s.catalog.Modules {
		description := firstLine(m.Description)
		if description == "" {
			description = plural(len(m.Functions), "function")
		}
		lines = append(lines, "- "+m.Name+": "+description)
	}
	parts = append(parts, strings.Join(lines, "\n"))

	rules := "Workflow: search_functions to find a function; a result of five functions or fewer shows their arguments, " +
		"so call_function directly, otherwise describe_function first, then call_function. " +
		"Use explain_term for any unfamiliar term or module concept. " +
		"Function names are module.function and arguments are one flat object. " +
		"Errors come back as one line with a code, such as \"error invalid: ...\"."
	if s.cfg.Script.Enabled {
		chained := "read-only calls"
		if s.cfg.Script.AllowWrites {
			chained = "calls"
		}
		rules += " run_script chains several " + chained + " in one round trip."
	}
	if len(s.cfg.Tools) > 0 {
		var tools []string
		for _, name := range s.cfg.Tools {
			f, _ := s.catalog.Function(name)
			tools = append(tools, name+" as "+f.ToolName())
		}
		rules += " These functions are also tools you can call directly: " + strings.Join(tools, ", ") +
			". Everything else is reached through search_functions and call_function."
	}
	parts = append(parts, rules)

	return strings.Join(parts, "\n\n")
}

// Reference renders the human-readable reference page of this server: the
// instructions, the tools, and every function and term exactly as the agent
// sees them. Render it once at startup and serve the bytes as text/html.
func (s *Server) Reference() ([]byte, error) {
	html, err := renderReference(s.catalog, s.referenceInfo())
	if err != nil {
		return nil, ez.Wrap(err)
	}
	return html, nil
}

// referenceInfo describes the server and its tools for the reference page.
func (s *Server) referenceInfo() referenceInfo {
	info := referenceInfo{
		Name:         s.cfg.Name,
		Version:      s.cfg.Version,
		Instructions: s.instructions,
	}
	for _, registered := range s.tools {
		tool := registered.tool
		info.Tools = append(info.Tools, toolInfo{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: inputSchemaJSON(tool),
		})
	}
	return info
}

// inputSchemaJSON encodes the input schema of a registered tool. The fixed
// schemas are inferred by jsonschema.For and the promoted ones are encoded
// once when the tool is registered, so encoding them cannot fail; a failure
// yields nil.
func inputSchemaJSON(tool *mcp.Tool) json.RawMessage {
	data, err := json.Marshal(tool.InputSchema)
	if err != nil {
		return nil
	}
	return data
}
