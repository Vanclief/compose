package mcp

import (
	"encoding/json"
	"fmt"
	"net/http"
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
	tools        []*mcp.Tool
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
		cfg.Auth = BearerToHeaders("API-KEY", "API-SECRET")
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
	for _, tool := range s.tools {
		info.Tools = append(info.Tools, toolInfo{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: inputSchemaJSON(tool),
		})
	}
	return info
}

// inputSchemaJSON encodes the input schema of a registered tool. The schemas
// are inferred by jsonschema.For when the tool is registered, so encoding
// them cannot fail; a failure yields nil.
func inputSchemaJSON(tool *mcp.Tool) json.RawMessage {
	data, err := json.Marshal(tool.InputSchema)
	if err != nil {
		return nil
	}
	return data
}
