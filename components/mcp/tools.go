package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"runtime/debug"
	"strings"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/vanclief/ez"

	"github.com/vanclief/compose/components/rest/requests"
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

// The limits of the catalog tools' arguments. search_functions,
// describe_function and explain_term answer in-process, past the REST API
// and its rate limits, so their arguments bound the work of a call before
// any of it is done. No honest call comes near them, and a result is cut at
// MaxOutputBytes anyway. The input schemas carry maxItems; the lengths and
// the word count are checked by the handlers, because the validator's
// maxLength error would quote the whole string back, uncut.
const (
	// maxListItems is the most names describe_function, or terms
	// explain_term, takes in one call. The describe blocks of the emma
	// catalog average about 500 bytes, so fewer than 50 of them fill the
	// default 24000-byte output.
	maxListItems = 50
	// maxNameLength is the most characters of one name or term, or of the
	// module search_functions is given, whitespace around it aside. The
	// names of the emma catalog run under 100.
	maxNameLength = 256
	// maxQueryLength is the most characters of a search_functions query.
	maxQueryLength = 256
	// maxQueryWords is the most words of a search_functions query. A search
	// costs about the number of words times the size of the catalog, and
	// every word must match, so a long query finds nothing anyway.
	maxQueryWords = 32
)

type callInput struct {
	Name string         `json:"name" jsonschema:"full function name, module.function"`
	Args map[string]any `json:"args,omitempty" jsonschema:"one flat object merging the path, query and body arguments listed by search_functions or describe_function"`
}

type scriptInput struct {
	Code string `json:"code" jsonschema:"JavaScript function body; return the result"`
}

const (
	searchDescription = "Find functions. With no arguments, list the modules. With module, list every function in it. " +
		"With query, list the functions whose name, summary, description, return type or argument names contain every word of it, " +
		"optionally within one module. Each line shows module.function, its summary and what it returns. " +
		"A result of five functions or fewer also shows each one's route, description and arguments, and a function shown " +
		"without arguments takes none: call_function next, after explain_term for an argument type you do not know. " +
		"Otherwise describe_function."
	describeDescription = "Show how to call functions: the route, every argument with its type, location and required flag, " +
		"the return shape and related terms. Pass full names, module.function, as listed by search_functions. " +
		"Describe a function whose arguments search_functions did not show, or when you need the return shape."
	explainDescription = "Explain domain terms: the fields of a data type and the functions that accept or return it, " +
		"or a concept of the application. Accepts names and aliases, case-insensitively. " +
		"Use it for any unfamiliar word in a summary, field or module name."
)

// callDescription is the call_function tool description with the output
// limit of the server. It names run_script only when scripts are enabled,
// so the agent is never sent to a tool the server does not have.
func callDescription(maxOutput int, scripts bool) string {
	around := "for a read-only function, narrow the arguments (a filter or a smaller page size)"
	if scripts {
		around += " or use run_script to return only the part you need"
	}
	return "Call one function by its full name, module.function. args is one flat object merging the path, query " +
		"and body arguments listed by search_functions or describe_function; omit it when there are none. Returns the JSON response. " +
		fmt.Sprintf("A response longer than %d bytes is cut and ends with an [incomplete: ...] note; %s; do not repeat a write. ", maxOutput, around) +
		"Errors come back as one line, error <code>: <message>; invalid arguments also show the argument list. " +
		"Some functions write data: read their description first."
}

// boolPtr returns a pointer to b for the optional annotation hints.
func boolPtr(b bool) *bool {
	return &b
}

// toolNamePattern is the tool name rule of claude.ai, which rejects the whole
// tool list when one name breaks it, and of the LLM APIs.
var toolNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// propertyNamePattern is the rule the Claude API applies to the top-level
// property names of a tool's input schema; one bad name fails the request.
var propertyNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,64}$`)

// toolHandler runs one tool call whose arguments are already decoded and
// returns the text of the result and whether the call failed. req is nil for
// an in-process call.
type toolHandler func(ctx context.Context, req *mcp.CallToolRequest, call *caller) (string, bool)

// registeredTool is a tool of the server with what Server.CallTool needs to
// run it without the transport.
type registeredTool struct {
	tool *mcp.Tool
	// prepare decodes the JSON arguments of a call as the transport does. It
	// returns the handler to run, or the text of the tool error the
	// transport answers when the arguments break the input schema or cannot
	// be decoded into the tool's input type.
	prepare func(args json.RawMessage) (toolHandler, string)
}

// registerTools adds the fixed tool set to the MCP server, then the promoted
// functions of Config.Tools.
func (s *Server) registerTools() error {
	// Every tool reaches only the catalog or the REST API, so none is open
	// world; MCP reads an unset openWorldHint as true.
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: boolPtr(false)}

	whoamiDescription := "Report the server name and version and whether a bearer token was sent."
	if s.cfg.WhoAmI != "" {
		whoamiDescription = "Return the account the current credentials belong to. Call it when you need the caller's identity or ids."
	}
	err := addTool(s, &mcp.Tool{Name: "whoami", Description: whoamiDescription, Annotations: readOnly}, s.whoami)
	if err != nil {
		return ez.Wrap(err)
	}

	err = addTool(s, &mcp.Tool{Name: "search_functions", Description: searchDescription, Annotations: readOnly},
		func(ctx context.Context, req *mcp.CallToolRequest, call *caller, in searchInput) (string, bool) {
			module := strings.TrimSpace(in.Module)
			query := strings.TrimSpace(in.Query)
			err := checkQuery(module, query)
			if err != nil {
				return errorText(err), true
			}
			return s.catalog.renderSearch(module, query), false
		})
	if err != nil {
		return ez.Wrap(err)
	}

	err = addTool(s, &mcp.Tool{Name: "describe_function", Description: describeDescription, Annotations: readOnly},
		func(ctx context.Context, req *mcp.CallToolRequest, call *caller, in describeInput) (string, bool) {
			err := checkNames("names", in.Names)
			if err != nil {
				return errorText(err), true
			}
			return s.catalog.renderDescribe(in.Names), false
		}, "names")
	if err != nil {
		return ez.Wrap(err)
	}

	err = addTool(s, &mcp.Tool{Name: "explain_term", Description: explainDescription, Annotations: readOnly},
		func(ctx context.Context, req *mcp.CallToolRequest, call *caller, in explainInput) (string, bool) {
			err := checkNames("terms", in.Terms)
			if err != nil {
				return errorText(err), true
			}
			return s.catalog.renderExplain(in.Terms), false
		}, "terms")
	if err != nil {
		return ez.Wrap(err)
	}

	callAnnotations := &mcp.ToolAnnotations{DestructiveHint: boolPtr(true), OpenWorldHint: boolPtr(false)}
	err = addTool(s, &mcp.Tool{Name: "call_function", Description: callDescription(s.maxOutput, s.cfg.Script.Enabled), Annotations: callAnnotations}, s.callFunction)
	if err != nil {
		return ez.Wrap(err)
	}

	if s.cfg.Script.Enabled {
		scriptAnnotations := &mcp.ToolAnnotations{ReadOnlyHint: !s.cfg.Script.AllowWrites, OpenWorldHint: boolPtr(false)}
		err = addTool(s, &mcp.Tool{Name: "run_script", Description: scriptDescription(s.cfg.Script, s.maxOutput), Annotations: scriptAnnotations}, s.runScript)
		if err != nil {
			return ez.Wrap(err)
		}
	}

	for _, name := range s.cfg.Tools {
		err = s.addFunctionTool(name)
		if err != nil {
			return ez.Wrap(err)
		}
	}
	return nil
}

// checkNames refuses a list whose entry, trimmed, is longer than
// maxNameLength characters. arg is the argument name the error reports.
func checkNames(arg string, names []string) error {
	for i, name := range names {
		n := utf8.RuneCountInString(strings.TrimSpace(name))
		if n > maxNameLength {
			return ez.New(ez.EINVALID, fmt.Sprintf("mcp: %s[%d] has %d characters, more than %d", arg, i, n, maxNameLength), nil)
		}
	}
	return nil
}

// checkQuery refuses a search whose module name is longer than maxNameLength
// characters, or whose query is longer than maxQueryLength characters or of
// more than maxQueryWords words. The caller trims both first, so the length
// check bounds the split into words and the search that follows.
func checkQuery(module, query string) error {
	n := utf8.RuneCountInString(module)
	if n > maxNameLength {
		return ez.New(ez.EINVALID, fmt.Sprintf("mcp: module has %d characters, more than %d", n, maxNameLength), nil)
	}
	n = utf8.RuneCountInString(query)
	if n > maxQueryLength {
		return ez.New(ez.EINVALID, fmt.Sprintf("mcp: query has %d characters, more than %d; search for a few words", n, maxQueryLength), nil)
	}
	words := len(searchWords(query))
	if words > maxQueryWords {
		return ez.New(ez.EINVALID, fmt.Sprintf("mcp: query has %d words, more than %d; search for a few words", words, maxQueryWords), nil)
	}
	return nil
}

// addTool registers a text tool. The input schema is inferred from In; the
// properties named in lists become arrays of 1 to maxListItems items. The
// handler runs through runTool.
func addTool[In any](s *Server, tool *mcp.Tool, handle func(context.Context, *mcp.CallToolRequest, *caller, In) (string, bool), lists ...string) error {
	schema, err := jsonschema.For[In](nil)
	if err != nil {
		return ez.New(ez.EINTERNAL, "mcp: cannot infer the input schema of "+tool.Name+": "+err.Error(), err)
	}
	for _, name := range lists {
		prop := schema.Properties[name]
		if prop == nil {
			return ez.New(ez.EINTERNAL, "mcp: tool "+tool.Name+" has no property "+name, nil)
		}
		prop.Type = "array"
		prop.Types = nil
		prop.MinItems = jsonschema.Ptr(1)
		prop.MaxItems = jsonschema.Ptr(maxListItems)
	}
	tool.InputSchema = schema
	resolved, err := schema.Resolve(&jsonschema.ResolveOptions{ValidateDefaults: true})
	if err != nil {
		return ez.New(ez.EINTERNAL, "mcp: cannot resolve the input schema of "+tool.Name+": "+err.Error(), err)
	}

	// prepare validates and decodes the arguments of a call before the
	// handler runs, as the SDK's typed tools do, for the transport and for
	// Server.CallTool alike. The tool is registered without the SDK's typed
	// handler because that one decodes every number as a float64 and encodes
	// it again, which rounds an integer a float64 cannot hold before the
	// handler sees it. The input is decoded with unmarshalExact, so a number
	// reaches an interface value of the input, such as callInput.Args, as a
	// json.Number, which callFunction settles with exactNumbers.
	prepare := func(args json.RawMessage) (toolHandler, string) {
		input, invalid := applyInputSchema(args, resolved)
		if invalid != "" {
			return nil, invalid
		}
		var in In
		err := unmarshalExact(input, &in)
		if err != nil {
			return nil, err.Error()
		}
		return func(ctx context.Context, req *mcp.CallToolRequest, call *caller) (string, bool) {
			return handle(ctx, req, call, in)
		}, ""
	}

	s.srv.AddTool(tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		handle, invalid := prepare(req.Params.Arguments)
		if handle == nil {
			// Arguments that break the schema are answered uncut, as the
			// SDK's typed tools answer them.
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: invalid}}, IsError: true}, nil
		}
		result := s.serveTool(ctx, req, tool.Name, func(call *caller) (string, bool) {
			return handle(ctx, req, call)
		})
		return result, nil
	})
	s.tools = append(s.tools, registeredTool{tool: tool, prepare: prepare})
	return nil
}

// applyInputSchema applies the defaults of the input schema to the JSON
// arguments of a call and validates them, as the MCP SDK does before it
// decodes the input of a typed tool. It returns the arguments with the
// defaults, or the text of the tool error the SDK's typed tools answer when
// they break the schema.
//
// The schema is checked on the arguments decoded with every number as a
// float64, the SDK's way: the validator reads a float64 as an integer or a
// number and a json.Number as a string. The arguments returned hold each
// number as the SDK's typed tools write it again, 1.0 as 1, except an
// integer exactNumbers keeps, which keeps its digits. A number no float64
// holds, such as 1e400, is refused here as the SDK's typed tools refuse it,
// in the words of encoding/json.
//
// The JSON-RPC layer of the SDK decodes the whole message with its own
// decoder, which refuses JSON nested more than 1000 levels deep, so the
// transport answers such a request with a protocol error, Bad Request over
// HTTP, before any tool runs. This uses encoding/json, so deeper nesting
// reaches the tool through Server.CallTool. A key in another case breaks
// the schema, which allows no other properties.
func applyInputSchema(args json.RawMessage, resolved *jsonschema.Resolved) (json.RawMessage, string) {
	object := map[string]any{}
	exact := map[string]any{}
	if len(args) > 0 {
		err := json.Unmarshal(args, &object)
		if err != nil {
			return nil, `validating "arguments": unmarshaling arguments: ` + err.Error()
		}
		err = unmarshalExact(args, &exact)
		if err != nil {
			return nil, `validating "arguments": unmarshaling arguments: ` + err.Error()
		}
	}
	// Arguments of null leave both maps nil, and a default cannot be set in
	// a nil map, so null is read as no arguments.
	if object == nil {
		object = map[string]any{}
	}
	if exact == nil {
		exact = map[string]any{}
	}
	var value any = object
	err := resolved.ApplyDefaults(&value)
	if err != nil {
		return nil, "validating \"arguments\": applying schema defaults:\n" + err.Error()
	}
	err = resolved.Validate(&value)
	if err != nil {
		return nil, `validating "arguments": ` + err.Error()
	}
	// Defaults are set where a property is missing, whatever the numbers
	// are, so they fill the exact arguments as they filled the checked ones.
	var input any = exact
	err = resolved.ApplyDefaults(&input)
	if err != nil {
		return nil, "validating \"arguments\": applying schema defaults:\n" + err.Error()
	}
	data, err := json.Marshal(exactNumbers(input))
	if err != nil {
		return nil, `validating "arguments": marshalling with defaults: ` + err.Error()
	}
	return data, ""
}

// unmarshalExact decodes data into v as json.Unmarshal does, except that a
// number decoded into an interface value becomes a json.Number, which keeps
// the digits as written; exactNumbers then turns all but the integers of
// magnitude 2^53 or more back into float64. It returns the error of
// encoding/json unwrapped, as json.Unmarshal does, because callers quote its
// text.
func unmarshalExact(data []byte, v any) error {
	if !json.Valid(data) {
		// The decoder below would read the first value and leave the rest;
		// json.Unmarshal reports the syntax error.
		return json.Unmarshal(data, v)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	return decoder.Decode(v)
}

// exactNumbers replaces, in arguments decoded by unmarshalExact, each
// json.Number by the float64 json.Unmarshal would have given, except an
// integer written without a fraction or an exponent whose magnitude is 2^53
// or more, such as 9007199254740993 or 100000000000000000000000: that one
// stays a json.Number, so the path, query, header, body and a composite's
// input get its digits. Every other number, 1.0, 1e3 or 2.50 among them, is
// a float64 as before; a float64 holds every integer below 2^53 exactly and
// prints it with the same digits, -0 included. It changes maps and slices
// in place, so it only runs on values this package decoded.
func exactNumbers(v any) any {
	switch t := v.(type) {
	case json.Number:
		// The float64 decode before this refuses a number no float64 holds,
		// so Float64 does not fail here; if it did, the number would stay
		// as written.
		f, err := t.Float64()
		if err != nil || (isIntegerLiteral(t.String()) && math.Abs(f) >= 1<<53) {
			return t
		}
		return f
	case map[string]any:
		for key, item := range t {
			t[key] = exactNumbers(item)
		}
	case []any:
		for i, item := range t {
			t[i] = exactNumbers(item)
		}
	}
	return v
}

// isIntegerLiteral reports whether a JSON number is written as an integer:
// an optional minus sign and digits, without a fraction or an exponent.
func isIntegerLiteral(n string) bool {
	return !strings.ContainsAny(n, ".eE")
}

// addFunctionTool registers the catalog function name as a tool named
// Function.ToolName that calls it as call_function would. Its schema is the
// function's InputSchema, and the arguments reach the function unvalidated
// by the SDK, so a bad call gets the same error and description as
// call_function. A schema that cannot be encoded fails here rather than in
// tools/list. DestructiveHint is true for a Destructive function and unset
// otherwise: MCP reads false as "only additive updates", which PUT, PATCH
// and write composites are not.
func (s *Server) addFunctionTool(name string) error {
	f, _ := s.catalog.Function(name)
	schema, err := f.InputSchema()
	if err != nil {
		return ez.Wrap(err)
	}
	_, err = json.Marshal(schema)
	if err != nil {
		return ez.New(ez.EINVALID, fmt.Sprintf("mcp: Config.Tools names %q, whose input schema cannot be encoded: %v", name, err), err)
	}
	for _, key := range schema.PropertyOrder {
		if !propertyNamePattern.MatchString(key) {
			return ez.New(ez.EINVALID, fmt.Sprintf("mcp: Config.Tools names %q, whose argument %q breaks the Claude API's rule of 1 to 64 characters from [a-zA-Z0-9_.-]", name, key), nil)
		}
	}
	var destructive *bool
	if f.Destructive {
		destructive = boolPtr(true)
	}
	tool := &mcp.Tool{
		Name:        f.ToolName(),
		Description: s.catalog.toolDescription(f),
		InputSchema: schema,
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:    f.ReadOnly,
			DestructiveHint: destructive,
			IdempotentHint:  f.ReadOnly,
			OpenWorldHint:   boolPtr(false),
		},
	}

	// The transport and Server.CallTool both run this handler on the raw
	// arguments. The first decode refuses what it always refused, a number
	// no float64 holds among them; the second keeps the digits that
	// callFunction's exactNumbers needs.
	run := func(ctx context.Context, req *mcp.CallToolRequest, call *caller, raw json.RawMessage) (string, bool) {
		var args map[string]any
		if len(raw) > 0 {
			var checked map[string]any
			err := json.Unmarshal(raw, &checked)
			if err != nil {
				return s.callErrorText(name, ez.New(ez.EINVALID, "mcp: arguments of "+name+" must be a JSON object", err)), true
			}
			err = unmarshalExact(raw, &args)
			if err != nil {
				return s.callErrorText(name, ez.New(ez.EINVALID, "mcp: arguments of "+name+" must be a JSON object", err)), true
			}
		}
		return s.callFunction(ctx, req, call, callInput{Name: name, Args: args})
	}
	prepare := func(args json.RawMessage) (toolHandler, string) {
		return func(ctx context.Context, req *mcp.CallToolRequest, call *caller) (string, bool) {
			return run(ctx, req, call, args)
		}, ""
	}

	s.srv.AddTool(tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		result := s.serveTool(ctx, req, tool.Name, func(call *caller) (string, bool) {
			return run(ctx, req, call, req.Params.Arguments)
		})
		return result, nil
	})
	s.tools = append(s.tools, registeredTool{tool: tool, prepare: prepare})
	return nil
}

// toolDescription is the description of a promoted tool: "Deprecated." when
// the function is, its summary, its description without a repeat of the
// summary, and what it returns, the words describe_function shows.
func (c *Catalog) toolDescription(f *Function) string {
	var lines []string
	if f.Deprecated {
		lines = append(lines, "Deprecated.")
	}
	summary := summaryLine(f)
	if summary != "" {
		lines = append(lines, summary)
	}
	description := trimSummary(strings.TrimSpace(f.Description), summary)
	if description != "" {
		lines = append(lines, description)
	}
	returns := c.returnLabel(f)
	if returns != "" {
		lines = append(lines, "Returns "+returns)
	}
	return strings.Join(lines, "\n")
}

// serveTool answers one tool call of the transport: it maps the bearer token
// of req to REST headers with Config.Auth, forwards the request headers and
// runs handle through runTool.
func (s *Server) serveTool(ctx context.Context, req *mcp.CallToolRequest, name string, handle func(*caller) (string, bool)) *mcp.CallToolResult {
	text, failed := s.runTool(ctx, name, func() (string, bool) {
		header, err := s.cfg.Auth(ctx, bearerToken(req))
		if err != nil {
			return errorText(err), true
		}
		call := &caller{s: s, header: header}
		extra := req.GetExtra()
		if extra != nil {
			call.forward = extra.Header
		}
		return handle(call)
	})
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
		IsError: failed,
	}
}

// runTool runs one tool call and returns the text handle reports, cut to the
// output limit, and whether the call failed. The cut of a result that
// succeeded ends with the hint of the server; the cut of a failed one has
// the note alone. A panic in handle becomes an internal error instead of
// killing the process, and goes to Config.OnError with its stack. The
// transport and Server.CallTool both run through it.
func (s *Server) runTool(ctx context.Context, name string, handle func() (string, bool)) (text string, failed bool) {
	defer func() {
		value := recover()
		if value != nil {
			panicked := ez.New(ez.EINTERNAL, fmt.Sprintf("mcp: %s panicked: %v", name, value), nil)
			if s.cfg.OnError != nil {
				s.cfg.OnError(ctx, ez.New(ez.EINTERNAL, fmt.Sprintf("mcp: %s panicked: %v\n%s", name, value, debug.Stack()), nil))
			}
			text = truncate(errorText(panicked), s.maxOutput, "")
			failed = true
		}
	}()
	text, failed = handle()
	if failed {
		// The error line comes first; the hint is about getting the rest of
		// a response, which a failed call has none of.
		return truncate(text, s.maxOutput, ""), failed
	}
	return truncate(text, s.maxOutput, s.cutHint), failed
}

// bearerToken returns the bearer token of the HTTP request behind req, or ""
// when there is none, the Authorization header is malformed, the transport
// is not HTTP, or there is no request because the call is in-process.
func bearerToken(req *mcp.CallToolRequest) string {
	if req == nil {
		return ""
	}
	extra := req.GetExtra()
	if extra == nil {
		return ""
	}
	token, err := requests.ParseBearer(extra.Header)
	if err != nil {
		return ""
	}
	return token
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
	// in.Args was decoded by unmarshalExact, for call_function and for a
	// promoted tool alike.
	exactNumbers(in.Args)
	raw, err := call.Call(ctx, name, in.Args)
	if err == nil {
		return string(raw), false
	}
	return s.callErrorText(name, err), true
}

// callErrorText is the error line of a failed call. An invalid-argument
// error is followed by the function description so the agent can fix the
// call.
func (s *Server) callErrorText(name string, err error) string {
	text := errorText(err)
	if ez.ErrorCode(err) == ez.EINVALID {
		text += "\n\n" + s.catalog.renderDescribe([]string{name})
	}
	return text
}

// runScript runs the agent's program. The result, or on error the error
// line, comes first and the log lines collected follow it, so the output cap
// cuts the logs first and reaches the result only when the result alone is
// over the cap. Scripts are only for callers whose credentials pass the
// WhoAmI call, and each caller, keyed by its bearer token or else the
// WhoAmI response, runs one at a time. Every caller is claimed before it
// takes a slot: by its bearer token before the WhoAmI call, or by the
// WhoAmI response after it, so it is refused at once while its own script
// still runs. The slot is taken only after the WhoAmI call and the Allow
// check, so a caller without credentials the API accepts never takes one.
// Other WhoAmI failures, such as an outage, are reported as what they are.
// Internal failures of the runtime also go to Config.OnError.
func (s *Server) runScript(ctx context.Context, req *mcp.CallToolRequest, call *caller, in scriptInput) (string, bool) {
	token := bearerToken(req)
	if token != "" {
		release, err := s.scripts.claim(scriptKey(token, nil))
		if err != nil {
			return errorText(err), true
		}
		defer release()
	}
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
	if token == "" {
		release, err := s.scripts.claim(scriptKey("", me))
		if err != nil {
			return errorText(err), true
		}
		defer release()
	}
	releaseSlot, err := s.scripts.slot(ctx)
	if err != nil {
		return errorText(err), true
	}
	defer releaseSlot()
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

// scriptKey identifies the caller of a script by its bearer token, or by its
// WhoAmI response when it sent none. Both are hashed, so the limiter holds no
// credentials, and prefixed, so a token cannot pose as a WhoAmI response.
func scriptKey(token string, me []byte) string {
	if token != "" {
		sum := sha256.Sum256([]byte(token))
		return "token:" + hex.EncodeToString(sum[:])
	}
	sum := sha256.Sum256(me)
	return "whoami:" + hex.EncodeToString(sum[:])
}
