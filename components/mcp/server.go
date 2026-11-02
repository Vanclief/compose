package mcp

import (
	"context"
	"encoding/json"
	"fmt"
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

	s := &Server{
		cfg:       cfg,
		catalog:   catalog,
		maxOutput: cfg.MaxOutputBytes,
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
func (s *Server) Handler() http.Handler {
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return s.srv
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
}

// Catalog returns the catalog the tools serve.
func (s *Server) Catalog() *Catalog {
	return s.catalog
}

// Call invokes a catalog function with the REST headers given, as
// call_function would for an MCP request carrying them. The headers are the
// credentials the REST API authenticates with (for example Authorization),
// set after the arguments, so an argument never overrides them. There is no
// MCP request to copy X-Forwarded-For or X-Request-Id from; set them in
// header if the REST API should see them. Unlike a tool call, Call does not
// recover a panic in a composite nor cut the result to MaxOutputBytes; use
// CallTool for that.
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
// X-Forwarded-For or X-Request-Id from; set them in header if the REST API
// should see them.
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

	rules := "Workflow: search_functions to find a function, describe_function to read its arguments, then call_function. " +
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
