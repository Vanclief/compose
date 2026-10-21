package mcp

import (
	"context"
	"fmt"
	"runtime/debug"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/vanclief/ez"
)

type whoamiInput struct{}

type searchInput struct {
	Module string `json:"module,omitempty" jsonschema:"module name from the server instructions; omit to search every module"`
	Query  string `json:"query,omitempty" jsonschema:"words to find in function names, summaries, descriptions, return types and argument names; every word must match"`
}

type describeInput struct {
	Names []string `json:"names" jsonschema:"full function names, module.function"`
}

type explainInput struct {
	Terms []string `json:"terms" jsonschema:"term names or aliases"`
}

type callInput struct {
	Name string         `json:"name" jsonschema:"full function name, module.function"`
	Args map[string]any `json:"args,omitempty" jsonschema:"one flat object merging the path, query and body arguments listed by describe_function"`
}

type scriptInput struct {
	Code string `json:"code" jsonschema:"JavaScript function body; return the result"`
}

const (
	searchDescription = "Find functions. With no arguments, list the modules. With module, list every function in it. " +
		"With query, list the functions whose name, summary, description, return type or argument names contain every word of it, " +
		"optionally within one module. Each line shows module.function, its summary and what it returns. Next, describe_function."
	describeDescription = "Show how to call functions: the route, every argument with its type, location and required flag, " +
		"the return shape and related terms. Pass full names, module.function, as listed by search_functions. " +
		"Describe a function before its first call_function."
	explainDescription = "Explain domain terms: the fields of a data type and the functions that accept or return it, " +
		"or a concept of the application. Accepts names and aliases, case-insensitively. " +
		"Use it for any unfamiliar word in a summary, field or module name."
	callDescription = "Call one function by its full name, module.function. args is one flat object merging the path, query " +
		"and body arguments listed by describe_function; omit it when there are none. Returns the JSON response. " +
		"Errors come back as one line, error <code>: <message>; invalid arguments also show the argument list. " +
		"Some functions write data: read their description first."
)

// boolPtr returns a pointer to b for the optional annotation hints.
func boolPtr(b bool) *bool {
	return &b
}

// registerTools adds the fixed tool set to the MCP server.
func (s *Server) registerTools() error {
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true}

	whoamiDescription := "Report the server name and version and whether a bearer token was sent."
	if s.cfg.WhoAmI != "" {
		whoamiDescription = "Return the account the current credentials belong to. Call it first to check the connection."
	}
	err := addTool(s, &mcp.Tool{Name: "whoami", Description: whoamiDescription, Annotations: readOnly}, s.whoami)
	if err != nil {
		return ez.Wrap(err)
	}

	err = addTool(s, &mcp.Tool{Name: "search_functions", Description: searchDescription, Annotations: readOnly},
		func(ctx context.Context, req *mcp.CallToolRequest, call *caller, in searchInput) (string, bool) {
			return s.catalog.renderSearch(in.Module, in.Query), false
		})
	if err != nil {
		return ez.Wrap(err)
	}

	err = addTool(s, &mcp.Tool{Name: "describe_function", Description: describeDescription, Annotations: readOnly},
		func(ctx context.Context, req *mcp.CallToolRequest, call *caller, in describeInput) (string, bool) {
			return s.catalog.renderDescribe(in.Names), false
		}, "names")
	if err != nil {
		return ez.Wrap(err)
	}

	err = addTool(s, &mcp.Tool{Name: "explain_term", Description: explainDescription, Annotations: readOnly},
		func(ctx context.Context, req *mcp.CallToolRequest, call *caller, in explainInput) (string, bool) {
			return s.catalog.renderExplain(in.Terms), false
		}, "terms")
	if err != nil {
		return ez.Wrap(err)
	}

	callAnnotations := &mcp.ToolAnnotations{DestructiveHint: boolPtr(true), OpenWorldHint: boolPtr(false)}
	err = addTool(s, &mcp.Tool{Name: "call_function", Description: callDescription, Annotations: callAnnotations}, s.callFunction)
	if err != nil {
		return ez.Wrap(err)
	}

	if !s.cfg.Script.Enabled {
		return nil
	}
	scriptAnnotations := &mcp.ToolAnnotations{ReadOnlyHint: !s.cfg.Script.AllowWrites}
	err = addTool(s, &mcp.Tool{Name: "run_script", Description: scriptDescription(s.cfg.Script), Annotations: scriptAnnotations}, s.runScript)
	if err != nil {
		return ez.Wrap(err)
	}
	return nil
}

// addTool registers a text tool. The input schema is inferred from In; the
// properties named in nonEmpty become arrays that need at least one item.
// The handler gets a caller with the REST headers of the request bearer
// token and returns the text and whether it reports a failure. A panic in
// the handler becomes an internal error result instead of killing the
// process.
func addTool[In any](s *Server, tool *mcp.Tool, handle func(context.Context, *mcp.CallToolRequest, *caller, In) (string, bool), nonEmpty ...string) error {
	schema, err := jsonschema.For[In](nil)
	if err != nil {
		return ez.New(ez.EINTERNAL, "mcp: cannot infer the input schema of "+tool.Name+": "+err.Error(), err)
	}
	for _, name := range nonEmpty {
		prop := schema.Properties[name]
		if prop == nil {
			return ez.New(ez.EINTERNAL, "mcp: tool "+tool.Name+" has no property "+name, nil)
		}
		prop.Type = "array"
		prop.Types = nil
		prop.MinItems = jsonschema.Ptr(1)
	}
	tool.InputSchema = schema

	mcp.AddTool(s.srv, tool, func(ctx context.Context, req *mcp.CallToolRequest, in In) (result *mcp.CallToolResult, output any, err error) {
		defer func() {
			value := recover()
			if value != nil {
				panicked := ez.New(ez.EINTERNAL, fmt.Sprintf("mcp: %s panicked: %v", tool.Name, value), nil)
				if s.cfg.OnError != nil {
					s.cfg.OnError(ctx, ez.New(ez.EINTERNAL, fmt.Sprintf("mcp: %s panicked: %v\n%s", tool.Name, value, debug.Stack()), nil))
				}
				result = s.result(errorText(panicked), true)
				output = nil
				err = nil
			}
		}()
		header, err := s.cfg.Auth(ctx, bearerToken(req))
		if err != nil {
			return s.result(errorText(err), true), nil, nil
		}
		call := &caller{s: s, header: header}
		extra := req.GetExtra()
		if extra != nil {
			call.forward = extra.Header
		}
		text, failed := handle(ctx, req, call, in)
		return s.result(text, failed), nil, nil
	})
	s.tools = append(s.tools, tool)
	return nil
}

// result is the text tool result, cut to the output limit.
func (s *Server) result(text string, failed bool) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: truncate(text, s.maxOutput)}},
		IsError: failed,
	}
}

// bearerToken returns the bearer token of the HTTP request behind req, or ""
// when there is none or the transport is not HTTP.
func bearerToken(req *mcp.CallToolRequest) string {
	extra := req.GetExtra()
	if extra == nil {
		return ""
	}
	value := strings.TrimSpace(extra.Header.Get("Authorization"))
	prefix := "bearer "
	if len(value) < len(prefix) || !strings.EqualFold(value[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(value[len(prefix):])
}

// errorMessage is the message of err without the package prefix the agent
// does not need.
func errorMessage(err error) string {
	return strings.TrimPrefix(ez.ErrorMessage(err), "mcp: ")
}

// errorText is the one-line error the agent sees: "error <code>: <message>".
func errorText(err error) string {
	return "error " + ez.ErrorCode(err) + ": " + errorMessage(err)
}

// whoami calls the configured identity function, or reports the server and
// whether a bearer token was sent.
func (s *Server) whoami(ctx context.Context, req *mcp.CallToolRequest, call *caller, in whoamiInput) (string, bool) {
	if s.cfg.WhoAmI != "" {
		raw, err := call.Call(ctx, s.cfg.WhoAmI, nil)
		if err != nil {
			return errorText(err), true
		}
		return string(raw), false
	}
	token := "none"
	if bearerToken(req) != "" {
		token = "present"
	}
	return "server " + s.cfg.Name + " " + s.cfg.Version + "\nbearer token: " + token, false
}

// callFunction dispatches one function. An invalid-argument error is
// followed by the function description so the agent can fix the call.
func (s *Server) callFunction(ctx context.Context, req *mcp.CallToolRequest, call *caller, in callInput) (string, bool) {
	name := strings.TrimSpace(in.Name)
	raw, err := call.Call(ctx, name, in.Args)
	if err == nil {
		return string(raw), false
	}
	text := errorText(err)
	if ez.ErrorCode(err) == ez.EINVALID {
		text += "\n\n" + s.catalog.renderDescribe([]string{name})
	}
	return text, true
}

// runScript runs the agent's program; on error the error line comes first
// and the log lines collected so far follow it. Scripts are only for
// callers whose credentials pass the WhoAmI call, and each caller, keyed
// by its bearer token or else
// the WhoAmI response, runs one at a time. Other WhoAmI failures, such as an
// outage, are reported as what they are. Internal failures of the runtime
// also go to Config.OnError.
func (s *Server) runScript(ctx context.Context, req *mcp.CallToolRequest, call *caller, in scriptInput) (string, bool) {
	me, err := call.Call(ctx, s.cfg.WhoAmI, nil)
	if err != nil {
		code := ez.ErrorCode(err)
		if code == ez.ENOTAUTHENTICATED || code == ez.ENOTAUTHORIZED {
			return errorText(ez.New(ez.ENOTAUTHENTICATED, "mcp: run_script requires credentials the API accepts", err)), true
		}
		return errorText(ez.New(code, "mcp: run_script could not check the credentials: "+errorMessage(err), err)), true
	}
	if s.cfg.Script.Allow != nil && !s.cfg.Script.Allow(ctx, me) {
		return errorText(ez.New(ez.ENOTAUTHORIZED, "mcp: run_script is not enabled for this account", nil)), true
	}
	key := bearerToken(req)
	if key == "" {
		key = string(me)
	}
	release, err := s.scripts.acquire(ctx, key)
	if err != nil {
		return errorText(err), true
	}
	defer release()
	out, err := s.engine.run(ctx, s.catalog, call, s.cfg.Script, in.Code, s.cfg.OnError)
	if err == nil {
		return out, false
	}
	if ez.ErrorCode(err) == ez.EINTERNAL && s.cfg.OnError != nil {
		s.cfg.OnError(ctx, err)
	}
	// The error comes first, so the output cap cannot cut it off.
	text := "error: " + errorMessage(err)
	if out != "" {
		text += "\n" + out
	}
	return text, true
}
