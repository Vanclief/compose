package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sort"
	"strings"

	"github.com/vanclief/ez"

	"github.com/vanclief/compose/components/rest/reference"
)

// maxErrorBody bounds the response body quoted in an error that is not the
// compose error envelope.
const maxErrorBody = 300

// BearerPassthrough returns an AuthFunc that forwards the bearer token of the
// MCP request to the REST API unchanged, as "Authorization: Bearer <token>".
// An empty token yields no headers. It is the default Config.Auth.
func BearerPassthrough() AuthFunc {
	return func(ctx context.Context, token string) (http.Header, error) {
		header := http.Header{}
		if token != "" {
			header.Set("Authorization", "Bearer "+token)
		}
		return header, nil
	}
}

// BearerToHeaders maps a "key:secret" bearer token to the two headers the
// REST API authenticates with. The token is split at the first ":". An empty
// token yields no headers.
func BearerToHeaders(keyHeader, secretHeader string) AuthFunc {
	return func(ctx context.Context, token string) (http.Header, error) {
		header := http.Header{}
		if token == "" {
			return header, nil
		}
		key, secret, found := strings.Cut(token, ":")
		if !found {
			return nil, ez.New(ez.EINVALID, "mcp: bearer token must be key:secret", nil)
		}
		header.Set(keyHeader, key)
		header.Set(secretHeader, secret)
		return header, nil
	}
}

// caller dispatches catalog functions to the REST handler with the headers
// of one MCP request.
type caller struct {
	s      *Server
	header http.Header
	// forward, when set, holds the headers of the MCP request whose
	// X-Forwarded-For and X-Request-Id are copied onto every REST request.
	// The RemoteAddr of the MCP request travels in the context instead, see
	// remoteAddrKey.
	forward http.Header
}

// remoteAddrKey is the context key under which Server.Handler stores the
// RemoteAddr of the MCP request, so every REST request it dispatches carries
// the client's address instead of the httptest placeholder 192.0.2.1:1234.
type remoteAddrKey struct{}

// Call invokes a function by catalog name. Operations go through the REST
// handler in-process; composites run with this caller so their calls carry
// the same credentials.
func (c *caller) Call(ctx context.Context, name string, args map[string]any) (json.RawMessage, error) {
	f, ok := c.s.catalog.Function(name)
	if !ok {
		return nil, ez.New(ez.ENOTFOUND, fmt.Sprintf("mcp: unknown function %s; use search_functions", name), nil)
	}
	if f.Composite {
		return c.callComposite(ctx, f, args)
	}
	return c.callOperation(ctx, f, args)
}

// callComposite runs a hand-written function and encodes its result.
func (c *caller) callComposite(ctx context.Context, f *Function, args map[string]any) (json.RawMessage, error) {
	raw, err := json.Marshal(args)
	if err != nil {
		return nil, ez.New(ez.EINVALID, "mcp: arguments of "+f.Name+" are not JSON: "+err.Error(), err)
	}
	result, err := f.composite.Run(ctx, c, raw)
	if err != nil {
		return nil, ez.Wrap(err)
	}
	data, err := json.Marshal(result)
	if err != nil {
		return nil, ez.New(ez.EINTERNAL, "mcp: result of "+f.Name+" is not JSON: "+err.Error(), err)
	}
	return data, nil
}

// callOperation builds the REST request of an operation from the flat
// arguments, serves it with the handler and decodes the outcome.
func (c *caller) callOperation(ctx context.Context, f *Function, args map[string]any) (json.RawMessage, error) {
	err := checkArgNames(f, args)
	if err != nil {
		return nil, ez.Wrap(err)
	}

	path := f.Path
	query := url.Values{}
	header := http.Header{}
	body := map[string]any{}
	hasBody := false
	for _, a := range f.Args {
		v, provided := args[a.Name]
		if a.In == "body" {
			hasBody = true
		}
		if a.In == "path" && (!provided || v == nil) {
			return nil, ez.New(ez.EINVALID, fmt.Sprintf("mcp: %s requires the path argument %s", f.Name, a.Name), nil)
		}
		if !provided {
			continue
		}
		// An explicit null is a value in a JSON body, and nothing elsewhere.
		if v == nil && a.In != "body" {
			continue
		}
		switch a.In {
		case "path":
			value := strings.Join(paramValues(a, v), ",")
			err = checkLine(f, a, value)
			if err != nil {
				return nil, ez.Wrap(err)
			}
			path = strings.ReplaceAll(path, "{"+a.Name+"}", url.PathEscape(value))
		case "query":
			for _, value := range paramValues(a, v) {
				query.Add(a.Name, value)
			}
		case "header":
			value := strings.Join(paramValues(a, v), ",")
			err = checkLine(f, a, value)
			if err != nil {
				return nil, ez.Wrap(err)
			}
			header.Set(a.Name, value)
		case "body":
			body[a.Name] = v
		}
	}

	target := path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}

	var reader io.Reader
	if hasBody {
		var payload any = body
		if f.bodyWhole {
			payload = args["body"]
		}
		if payload == nil {
			payload = map[string]any{}
		}
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, ez.New(ez.EINVALID, "mcp: body of "+f.Name+" is not JSON: "+err.Error(), err)
		}
		reader = bytes.NewReader(data)
	}

	req := httptest.NewRequestWithContext(ctx, f.Method, target, reader)
	remoteAddr, _ := ctx.Value(remoteAddrKey{}).(string)
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	for name := range header {
		req.Header.Set(name, header.Get(name))
	}
	for _, name := range []string{"X-Forwarded-For", "X-Request-Id"} {
		value := c.forward.Get(name)
		if value != "" {
			req.Header.Set(name, value)
		}
	}
	// The auth headers go last with canonical keys, so they win over any
	// argument however the AuthFunc spelled them.
	for name, values := range c.header {
		req.Header.Del(name)
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	req.Header.Set("Accept", "application/json")
	if hasBody {
		req.Header.Set("Content-Type", "application/json")
	}

	rec := httptest.NewRecorder()
	c.s.cfg.Handler.ServeHTTP(rec, req)

	data := rec.Body.Bytes()
	if rec.Code < 300 {
		if len(bytes.TrimSpace(data)) == 0 {
			return json.RawMessage("null"), nil
		}
		// A response without a Content-Type counts as JSON when it parses.
		contentType := rec.Header().Get("Content-Type")
		isJSON := strings.Contains(strings.ToLower(contentType), "json") || (contentType == "" && json.Valid(data))
		if !isJSON {
			return nonJSONResult(contentType, len(data))
		}
		return data, nil
	}
	return nil, responseError(rec.Code, data)
}

// checkArgNames rejects argument names the function does not take.
func checkArgNames(f *Function, args map[string]any) error {
	valid := map[string]bool{}
	names := make([]string, 0, len(f.Args))
	for _, a := range f.Args {
		valid[a.Name] = true
		names = append(names, a.Name)
	}

	var unknown []string
	for name := range args {
		if !valid[name] {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)

	if len(names) == 0 {
		return ez.New(ez.EINVALID, fmt.Sprintf("mcp: %s takes no arguments, got %s", f.Name, strings.Join(unknown, ", ")), nil)
	}
	return ez.New(ez.EINVALID, fmt.Sprintf("mcp: unknown arguments %s for %s; valid: %s", strings.Join(unknown, ", "), f.Name, strings.Join(names, ", ")), nil)
}

// paramValues renders a path, query or header argument with the style and
// explode of its parameter. Go slices from composites become []any first,
// with nil elements skipped.
//
// grug: arguments arrive decoded as float64, so integers above 2^53 have
// already lost precision before they get here.
func paramValues(a *Arg, v any) []string {
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array {
		items := []any{}
		for i := range rv.Len() {
			item := rv.Index(i).Interface()
			if item != nil {
				items = append(items, item)
			}
		}
		v = items
	}
	if a.param == nil {
		return []string{reference.ParamString(v)}
	}
	return reference.ParamValues(a.param, v)
}

// checkLine rejects a path or header value with a line break or NUL, which
// would split or end the request line or header.
func checkLine(f *Function, a *Arg, value string) error {
	if strings.ContainsAny(value, "\r\n\x00") {
		return ez.New(ez.EINVALID, fmt.Sprintf("mcp: %s argument %s of %s must not contain line breaks or NUL", a.In, a.Name, f.Name), nil)
	}
	return nil
}

// nonJSONResult describes a success body that is not JSON by its content
// type and size, as a JSON string, instead of returning the raw bytes.
func nonJSONResult(contentType string, size int) (json.RawMessage, error) {
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	data, err := json.Marshal(fmt.Sprintf("%s, %d bytes", contentType, size))
	if err != nil {
		return nil, ez.New(ez.EINTERNAL, "mcp: cannot describe a non-JSON response", err)
	}
	return data, nil
}

// responseError converts a REST error response into an ez error: the code
// and message of the compose error envelope, else the code of the status and
// the start of the body.
func responseError(status int, body []byte) error {
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	err := json.Unmarshal(body, &envelope)
	if err == nil && envelope.Error.Code != "" {
		message := envelope.Error.Message
		if message == "" {
			message = http.StatusText(status)
		}
		return ez.New(envelope.Error.Code, message, nil)
	}

	message := strings.TrimSpace(string(body))
	if len(message) > maxErrorBody {
		message = strings.ToValidUTF8(message[:maxErrorBody], "")
	}
	if message == "" {
		message = http.StatusText(status)
	}
	return ez.New(ez.HTTPStatusToError(status), message, nil)
}
