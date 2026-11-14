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
	"unicode"

	"github.com/vanclief/ez"

	"github.com/vanclief/compose/components/rest/reference"
)

// maxErrorBody bounds the response body quoted in an error that is not the
// compose error envelope, and the Location quoted in an error.
const maxErrorBody = 300

// maxLocation bounds the Location a redirect result returns. A presigned
// object URL takes a few hundred bytes to a few KB, 8192 bytes is the URL
// length many servers and proxies accept, and a result of that size fits the
// default MaxOutputBytes. A longer location is an error, since a location cut
// short cannot be followed.
const maxLocation = 8192

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
	// Server.Handler has already folded X-Forwarded-For into one line ending
	// in the MCP peer, so copying the first line copies the whole chain.
	forward http.Header
}

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
	// A body goes out when a body argument is passed or the body is required
	// and has a schema. A required body with a schema is sent even without
	// arguments, as {}, so the handler's own validation answers: with the
	// missing fields of an object body, or with a type error for a body that
	// is not an object. A required body without a schema is not sent.
	hasBody := f.bodyRequired
	for _, a := range f.Args {
		v, provided := args[a.Name]
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
			// An empty value would leave the segment empty, so /runs/{id}
			// would reach /runs/ and another route could answer.
			value := strings.Join(paramValues(a, v), ",")
			if value == "" {
				return nil, ez.New(ez.EINVALID, fmt.Sprintf("mcp: %s requires a non-empty path argument %s", f.Name, a.Name), nil)
			}
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
			hasBody = true
		}
	}
	// checkArgNames let through only the other keys a body allows.
	if f.otherArgs {
		for name, v := range args {
			if !hasArg(f, name) {
				body[name] = v
				hasBody = true
			}
		}
	}

	// A handler that cleans the path before routing drops a . segment and
	// takes a .. segment with the one before it, so /runs/{id}/logs with an
	// id of .. would be answered as /logs, by another route.
	for _, segment := range strings.Split(path, "/") {
		if segment == "." || segment == ".." {
			return nil, ez.New(ez.EINVALID, fmt.Sprintf("mcp: %s path arguments must not make a path segment of . or ..", f.Name), nil)
		}
	}

	target := path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}

	var reader io.Reader
	if hasBody {
		var payload any = body
		whole, provided := args["body"]
		if f.bodyWhole && provided {
			payload = whole
		}
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, ez.New(ez.EINVALID, "mcp: body of "+f.Name+" is not JSON: "+err.Error(), err)
		}
		reader = bytes.NewReader(data)
	}

	req := httptest.NewRequestWithContext(ctx, f.Method, target, reader)
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
		req.Header.Set("Content-Type", f.bodyType)
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

	// A redirect is not followed: it is the result when it says where the
	// outcome is, and an error naming the location otherwise or when the
	// location is longer than maxLocation.
	location := cleanLocation(rec.Header().Get("Location"))
	if rec.Code < 400 && location != "" {
		// The body of a redirect result is not read. The body of a 3xx that
		// is not a result is read only for the compose error envelope, whose
		// code and message win over the location.
		if redirectIsResult(f.Method, rec.Code) {
			if len(location) > maxLocation {
				message := fmt.Sprintf("%s; the location has %d bytes, more than the %d a redirect result returns", redirectLine(rec.Code, location), len(location), maxLocation)
				return nil, ez.New(ez.HTTPStatusToError(rec.Code), message, nil)
			}
			return redirectResult(rec.Code, location)
		}
		err = envelopeError(rec.Code, data)
		if err != nil {
			return nil, ez.Wrap(err)
		}
		return nil, ez.New(ez.HTTPStatusToError(rec.Code), redirectLine(rec.Code, location), nil)
	}
	return nil, responseError(rec.Code, data)
}

// redirectIsResult reports whether a 3xx status answers a call. Only 301,
// 302, 303, 307 and 308 are redirects; any other status, such as 300, 304,
// 305 or 306, is not a result on any method. A GET or HEAD changed nothing,
// so any of the five redirects is its answer. Another method gets 302 and
// 303 as the place of its outcome, while 301, 307 and 308 say it was not
// carried out and must be repeated at the location.
func redirectIsResult(method string, status int) bool {
	switch status {
	case http.StatusFound, http.StatusSeeOther:
		return true
	case http.StatusMovedPermanently, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return method == http.MethodGet || method == http.MethodHead
	}
	return false
}

// checkArgNames rejects argument names the function does not take. A
// function with otherArgs takes any other name, except one that is an
// argument name or a body property in another letter case, which a handler
// decoding JSON without regard to case could read as that one, or a
// read-only body property.
func checkArgNames(f *Function, args map[string]any) error {
	valid := map[string]bool{}
	names := make([]string, 0, len(f.Args))
	for _, a := range f.Args {
		valid[a.Name] = true
		names = append(names, a.Name)
	}

	var unknown []string
	for name := range args {
		if valid[name] {
			continue
		}
		if f.otherArgs && !reservedArg(f, name) {
			continue
		}
		unknown = append(unknown, name)
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)

	if f.otherArgs {
		accepted := "other keys"
		if len(names) > 0 {
			accepted = strings.Join(names, ", ") + ", and other keys"
		}
		return ez.New(ez.EINVALID, fmt.Sprintf("mcp: arguments %s for %s repeat an argument or a body property in another letter case, or name a read-only body property; valid: %s, sent in the body", strings.Join(unknown, ", "), f.Name, accepted), nil)
	}
	if len(names) == 0 {
		return ez.New(ez.EINVALID, fmt.Sprintf("mcp: %s takes no arguments, got %s", f.Name, strings.Join(unknown, ", ")), nil)
	}
	return ez.New(ez.EINVALID, fmt.Sprintf("mcp: unknown arguments %s for %s; valid: %s", strings.Join(unknown, ", "), f.Name, strings.Join(names, ", ")), nil)
}

// hasArg reports whether f has an argument named name.
func hasArg(f *Function, name string) bool {
	for _, a := range f.Args {
		if a.Name == name {
			return true
		}
	}
	return false
}

// reservedArg reports whether name, which is not an argument of f, is an
// argument name or a body property of f in any letter case.
func reservedArg(f *Function, name string) bool {
	for _, a := range f.Args {
		if strings.EqualFold(a.Name, name) {
			return true
		}
	}
	for _, key := range f.bodyKeys {
		if strings.EqualFold(key, name) {
			return true
		}
	}
	return false
}

// paramValues renders a path, query or header argument with the style and
// explode of its parameter. Go slices from composites become []any first,
// with nil elements skipped. reference.ParamValues does not model the label
// and matrix styles, which would add a prefix, so an empty value renders as
// "" in every style.
//
// Numbers from call_function and promoted tools arrive as float64, except
// an integer written without a fraction or an exponent whose magnitude is
// 2^53 or more, which arrives as a json.Number and renders with the digits
// the agent wrote. grug: numbers in run_script are JavaScript doubles, so a
// script cannot pass every integer above 2^53 exactly; written as a string
// it reaches a path, query or header argument with its digits, but not a
// body integer. Exact script numbers would need the script bridge to carry
// them as text.
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

// cleanLocation returns a Location header kept to valid UTF-8, without the
// characters dropControl drops or surrounding space. Without those
// characters the location cannot add a line to the text that quotes it, nor
// reorder how that text reads.
func cleanLocation(location string) string {
	location = strings.Map(dropControl, strings.ToValidUTF8(location, ""))
	return strings.TrimSpace(location)
}

// dropControl removes, from a string given to strings.Map, control
// characters, the line and paragraph separators U+2028 and U+2029, and
// format characters such as U+202E (right-to-left override).
func dropControl(r rune) rune {
	if unicode.IsControl(r) || unicode.In(r, unicode.Zl, unicode.Zp, unicode.Cf) {
		return -1
	}
	return r
}

// redirectResult describes a redirect as a JSON object, such as
// {"redirect":{"status":302,"location":"/orders/7"}}, with the whole
// location. HTML characters are not escaped, so the location reads as the
// handler wrote it.
func redirectResult(status int, location string) (json.RawMessage, error) {
	type redirect struct {
		Status   int    `json:"status"`
		Location string `json:"location"`
	}
	result := struct {
		Redirect redirect `json:"redirect"`
	}{redirect{Status: status, Location: location}}

	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	err := encoder.Encode(result)
	if err != nil {
		return nil, ez.New(ez.EINTERNAL, "mcp: cannot describe a redirect response", err)
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// redirectLine names the status and location of a redirect in an error, such
// as "307 Temporary Redirect, Location: /orders/7", with a location of more
// than maxErrorBody bytes cut on a character boundary and marked
// [truncated].
func redirectLine(status int, location string) string {
	statusLine := strings.TrimSpace(fmt.Sprintf("%d %s", status, http.StatusText(status)))
	if len(location) <= maxErrorBody {
		return statusLine + ", Location: " + location
	}
	return statusLine + ", Location: " + strings.ToValidUTF8(location[:maxErrorBody], "") + " [truncated]"
}

// responseError converts a REST error response into an ez error: the code
// and message of the compose error envelope, else the code of the status and
// the start of the body.
func responseError(status int, body []byte) error {
	err := envelopeError(status, body)
	if err != nil {
		return ez.Wrap(err)
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

// envelopeError returns the error of a body that is the compose error
// envelope with a code: its code and message, else the status text when the
// message is empty. It returns nil for any other body.
func envelopeError(status int, body []byte) error {
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	err := json.Unmarshal(body, &envelope)
	if err != nil || envelope.Error.Code == "" {
		return nil
	}
	message := envelope.Error.Message
	if message == "" {
		message = http.StatusText(status)
	}
	return ez.New(envelope.Error.Code, message, nil)
}
