package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vanclief/ez"
)

// dispatchSpec declares the operations the dispatch tests call.
const dispatchSpec = `{"openapi": "3.1.0", "info": {"title": "Dispatch", "version": "1"}, "paths": {
	"/items/{id}": {
		"parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "integer"}}],
		"get": {"operationId": "items.get", "parameters": [
			{"name": "limit", "in": "query", "schema": {"type": "integer"}},
			{"name": "tags", "in": "query", "explode": false, "schema": {"type": "array", "items": {"type": "string"}}},
			{"name": "pipes", "in": "query", "style": "pipeDelimited", "schema": {"type": "array", "items": {"type": "number"}}},
			{"name": "X-Trace", "in": "header", "schema": {"type": "string"}},
			{"name": "API-KEY", "in": "header", "schema": {"type": "string"}}
		], "responses": {"200": {"description": "OK"}}},
		"patch": {"operationId": "items.patch", "requestBody": {"content": {"application/json": {"schema": {"type": "object", "properties": {
			"note": {"type": ["string", "null"]},
			"size": {"type": "integer"}
		}}}}}, "responses": {"200": {"description": "OK"}}}
	},
	"/wrapped": {"post": {"operationId": "items.wrap", "requestBody": {"content": {"application/json": {"schema": {"type": "object", "properties": {
		"body": {"type": "string"}
	}}}}}, "responses": {"200": {"description": "OK"}}}},
	"/report": {"get": {"operationId": "reports.csv", "responses": {"200": {"description": "OK", "content": {"text/csv": {"schema": {"type": "string"}}}}}}}
}}`

// dispatchEcho is what the dispatch app answers: the request as it arrived.
type dispatchEcho struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Query   string            `json:"query"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

// dispatchApp echoes every request, except /report which answers CSV.
func dispatchApp() http.Handler {
	e := echo.New()
	e.GET("/report", func(c *echo.Context) error {
		return c.Blob(http.StatusOK, "text/csv", []byte("a,b\n1,2\n"))
	})
	e.Any("/*", func(c *echo.Context) error {
		r := c.Request()
		data, err := io.ReadAll(r.Body)
		if err != nil {
			return err
		}
		out := dispatchEcho{
			Method:  r.Method,
			Path:    r.URL.EscapedPath(),
			Query:   r.URL.RawQuery,
			Headers: map[string]string{},
			Body:    string(data),
		}
		for name, values := range r.Header {
			out.Headers[name] = values[0]
		}
		return c.JSON(http.StatusOK, out)
	})
	return e
}

// dispatchCaller builds a server for dispatchSpec and a caller with the
// given auth headers.
func dispatchCaller(t *testing.T, header http.Header) *caller {
	t.Helper()
	srv, err := New(Config{Spec: []byte(dispatchSpec), Handler: dispatchApp()})
	require.NoError(t, err)
	return &caller{s: srv, header: header}
}

// callDispatch calls a function and decodes the echo.
func callDispatch(t *testing.T, c *caller, name string, args map[string]any) dispatchEcho {
	t.Helper()
	raw, err := c.Call(context.Background(), name, args)
	require.NoError(t, err)
	var out dispatchEcho
	require.NoError(t, json.Unmarshal(raw, &out), string(raw))
	return out
}

func TestDispatchNumbersAndStyles(t *testing.T) {
	c := dispatchCaller(t, http.Header{})

	// Arguments arrive as float64 from the SDK.
	got := callDispatch(t, c, "items.get", map[string]any{
		"id":    float64(1234567),
		"limit": float64(1000000),
		"tags":  []any{"a", "b"},
		"pipes": []any{1.5, float64(2000000)},
	})
	assert.Equal(t, "/items/1234567", got.Path)
	assert.Equal(t, "limit=1000000&pipes=1.5%7C2000000&tags=a%2Cb", got.Query)

	// Go slices from composites are serialized the same way.
	got = callDispatch(t, c, "items.get", map[string]any{"id": 7, "tags": []string{"x", "y"}})
	assert.Equal(t, "tags=x%2Cy", got.Query)
}

func TestDispatchBodyPropertyNamedBody(t *testing.T) {
	c := dispatchCaller(t, http.Header{})
	got := callDispatch(t, c, "items.wrap", map[string]any{"body": "hello"})
	assert.JSONEq(t, `{"body":"hello"}`, got.Body)
}

func TestDispatchExplicitNull(t *testing.T) {
	c := dispatchCaller(t, http.Header{})

	got := callDispatch(t, c, "items.patch", map[string]any{"id": "x", "note": nil})
	assert.JSONEq(t, `{"note":null}`, got.Body)

	got = callDispatch(t, c, "items.patch", map[string]any{"id": "x", "size": 2})
	assert.JSONEq(t, `{"size":2}`, got.Body, "arguments not provided are not sent")

	got = callDispatch(t, c, "items.get", map[string]any{"id": 1, "limit": nil, "X-Trace": nil})
	assert.Equal(t, "", got.Query, "null query arguments are skipped")
	_, sent := got.Headers["X-Trace"]
	assert.False(t, sent, "null header arguments are skipped")
}

func TestDispatchHeaders(t *testing.T) {
	// A custom AuthFunc may spell the key in any case.
	c := dispatchCaller(t, http.Header{"api-key": {"from-auth"}})

	got := callDispatch(t, c, "items.get", map[string]any{"id": 1, "API-KEY": "from-agent", "X-Trace": "t-1"})
	assert.Equal(t, "from-auth", got.Headers["Api-Key"], "auth headers win over arguments")
	assert.Equal(t, "t-1", got.Headers["X-Trace"])
	assert.Equal(t, "application/json", got.Headers["Accept"])

	cases := map[string]map[string]any{
		"header CRLF": {"id": 1, "X-Trace": "a\r\nX-Evil: 1"},
		"header LF":   {"id": 1, "X-Trace": "a\nb"},
		"header NUL":  {"id": 1, "X-Trace": "a\x00b"},
		"path CR":     {"id": "1\r"},
		"path LF":     {"id": "1\n2"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := c.Call(context.Background(), "items.get", args)
			require.Error(t, err)
			assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
		})
	}
}

func TestDispatchNonJSONResponse(t *testing.T) {
	c := dispatchCaller(t, http.Header{})
	raw, err := c.Call(context.Background(), "reports.csv", nil)
	require.NoError(t, err)
	assert.Equal(t, `"text/csv, 8 bytes"`, string(raw))
}

func TestDispatchForwardedIdentity(t *testing.T) {
	c := dispatchCaller(t, http.Header{})
	got := callDispatch(t, c, "items.get", map[string]any{"id": 1})
	_, sent := got.Headers["X-Forwarded-For"]
	assert.False(t, sent, "nothing is forwarded without a forward header")

	c.forward = http.Header{}
	c.forward.Set("X-Forwarded-For", "203.0.113.9")
	c.forward.Set("X-Request-Id", "req-9")
	c.forward.Set("Cookie", "secret")
	got = callDispatch(t, c, "items.get", map[string]any{"id": 1})
	assert.Equal(t, "203.0.113.9", got.Headers["X-Forwarded-For"])
	assert.Equal(t, "req-9", got.Headers["X-Request-Id"])
	_, sent = got.Headers["Cookie"]
	assert.False(t, sent, "only the identity headers are forwarded")
}

// forwardApp answers every request with {} and keeps the X-Forwarded-For
// lines and the peer of the last one.
type forwardApp struct {
	mu    sync.Mutex
	lines []string
	peer  string
}

func (a *forwardApp) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.lines = r.Header.Values("X-Forwarded-For")
	a.peer = r.RemoteAddr
	a.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte("{}"))
}

// seen returns the X-Forwarded-For lines and the peer of the last request.
func (a *forwardApp) seen() ([]string, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lines, a.peer
}

// postToolCall posts a call_function call of items.get to handler as the
// peer remoteAddr with the X-Forwarded-For lines given, and returns the
// request it posted.
func postToolCall(t *testing.T, handler http.Handler, remoteAddr string, forwarded []string) *http.Request {
	t.Helper()
	body := `{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": {"name": "call_function", "arguments": {"name": "items.get", "args": {"id": 1}}}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.RemoteAddr = remoteAddr
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for _, line := range forwarded {
		req.Header.Add("X-Forwarded-For", line)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"result"`, rec.Body.String())
	require.NotContains(t, rec.Body.String(), `"isError":true`, rec.Body.String())
	return req
}

func TestHandlerCompletesForwardedFor(t *testing.T) {
	cases := map[string]struct {
		remoteAddr string
		forwarded  []string
		want       string
	}{
		"two lines": {
			remoteAddr: "198.51.100.7:4321",
			forwarded:  []string{"203.0.113.9", "10.0.0.1, 10.0.0.2"},
			want:       "203.0.113.9, 10.0.0.1, 10.0.0.2, 198.51.100.7",
		},
		"none": {
			remoteAddr: "198.51.100.7:4321",
			want:       "198.51.100.7",
		},
		"IPv6 peer": {
			remoteAddr: "[2001:db8::7]:4321",
			forwarded:  []string{"203.0.113.9"},
			want:       "203.0.113.9, 2001:db8::7",
		},
		"peer without port": {
			remoteAddr: "198.51.100.7",
			forwarded:  []string{"203.0.113.9"},
			want:       "203.0.113.9, 198.51.100.7",
		},
		"unsplittable peer": {
			remoteAddr: "pipe",
			forwarded:  []string{"203.0.113.9"},
			want:       "203.0.113.9, pipe",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			app := &forwardApp{}
			srv, err := New(Config{Spec: []byte(dispatchSpec), Handler: app})
			require.NoError(t, err)

			req := postToolCall(t, srv.Handler(), tc.remoteAddr, tc.forwarded)
			lines, peer := app.seen()
			assert.Equal(t, []string{tc.want}, lines, "one line: the received lines, then the peer")
			assert.Equal(t, "192.0.2.1:1234", peer, "the REST request keeps the dispatch peer")
			assert.Equal(t, tc.forwarded, req.Header.Values("X-Forwarded-For"), "the incoming request is not mutated")
			assert.Equal(t, tc.remoteAddr, req.RemoteAddr)
		})
	}
}

func TestHandlerForwardedForResolvesClient(t *testing.T) {
	_, trusted, err := net.ParseCIDR("192.0.2.1/32")
	require.NoError(t, err)
	e := echo.New()
	e.IPExtractor = echo.ExtractIPFromXFFHeader(
		echo.TrustLoopback(false),
		echo.TrustLinkLocal(false),
		echo.TrustPrivateNet(false),
		echo.TrustIPRange(trusted),
	)
	var mu sync.Mutex
	realIP := ""
	e.GET("/items/:id", func(c *echo.Context) error {
		mu.Lock()
		realIP = c.RealIP()
		mu.Unlock()
		return c.JSON(http.StatusOK, map[string]any{})
	})
	srv, err := New(Config{Spec: []byte(dispatchSpec), Handler: e})
	require.NoError(t, err)

	// The client wrote its own X-Forwarded-For; the walk stops at the peer.
	postToolCall(t, srv.Handler(), "198.51.100.7:4321", []string{"203.0.113.9"})
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, "198.51.100.7", realIP)
}

// clientHeaderSpec declares the client address headers as parameters.
const clientHeaderSpec = `{"openapi": "3.1.0", "info": {"title": "Client", "version": "1"}, "paths": {
	"/items/{id}": {"get": {"operationId": "items.get", "parameters": [
		{"name": "id", "in": "path", "required": true, "schema": {"type": "integer"}},
		{"name": "X-Forwarded-For", "in": "header", "schema": {"type": "string"}},
		{"name": "x-real-ip", "in": "header", "schema": {"type": "string"}},
		{"name": "X-Trace", "in": "header", "schema": {"type": "string"}}
	], "responses": {"200": {"description": "OK"}}}},
	"/other": {"get": {"operationId": "items.other", "parameters": [
		{"name": "x-forwarded-for", "in": "header", "schema": {"type": "string"}},
		{"name": "X-REAL-IP", "in": "header", "schema": {"type": "string"}}
	], "responses": {"200": {"description": "OK"}}}}
}}`

func TestClientAddressHeadersAreNotArguments(t *testing.T) {
	app := &forwardApp{}
	srv, err := New(Config{Spec: []byte(clientHeaderSpec), Handler: app})
	require.NoError(t, err)

	get := mustFunction(t, srv.Catalog(), "items.get")
	assert.Equal(t, []string{"id:path", "X-Trace:header"}, argNames(get))
	other := mustFunction(t, srv.Catalog(), "items.other")
	assert.Empty(t, other.Args)

	for _, name := range []string{"X-Forwarded-For", "x-real-ip", "X-Real-IP"} {
		t.Run(name, func(t *testing.T) {
			_, err := srv.Call(context.Background(), http.Header{}, "items.get", map[string]any{"id": 1, name: "203.0.113.9"})
			require.Error(t, err)
			assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
		})
	}
}

func TestServerCallSendsNoForwardedFor(t *testing.T) {
	app := &forwardApp{}
	srv, err := New(Config{Spec: []byte(dispatchSpec), Handler: app})
	require.NoError(t, err)

	_, err = srv.Call(context.Background(), http.Header{"Authorization": {"Bearer t"}}, "items.get", map[string]any{"id": 1})
	require.NoError(t, err)
	lines, peer := app.seen()
	assert.Nil(t, lines, "no X-Forwarded-For without one in the header")
	assert.Equal(t, "192.0.2.1:1234", peer)

	_, err = srv.Call(context.Background(), http.Header{"X-Forwarded-For": {"198.51.100.7"}}, "items.get", map[string]any{"id": 1})
	require.NoError(t, err)
	lines, _ = app.seen()
	assert.Equal(t, []string{"198.51.100.7"}, lines, "the address the host resolved goes through as given")
}

// bodySpec declares request bodies of every kind dispatch sends, and a
// route that redirects.
const bodySpec = `{"openapi": "3.1.0", "info": {"title": "Bodies", "version": "1"}, "paths": {
	"/optional": {"patch": {"operationId": "bodies.optional", "requestBody": {"required": false, "content": {"application/json": {"schema": {"type": "object", "properties": {
		"name": {"type": "string"},
		"size": {"type": "integer"}
	}}}}}, "responses": {"200": {"description": "OK"}}}},
	"/absent": {"patch": {"operationId": "bodies.absent", "requestBody": {"content": {"application/json": {"schema": {"type": "object", "properties": {
		"name": {"type": "string"}
	}}}}}, "responses": {"200": {"description": "OK"}}}},
	"/required": {"post": {"operationId": "bodies.required", "requestBody": {"required": true, "content": {"application/json": {"schema": {"type": "object", "properties": {
		"name": {"type": "string"}
	}}}}}, "responses": {"200": {"description": "OK"}}}},
	"/noschema": {"post": {"operationId": "bodies.noschema", "requestBody": {"required": true, "content": {"application/json": {}}}, "responses": {"200": {"description": "OK"}}}},
	"/readonly": {"post": {"operationId": "bodies.readonly", "requestBody": {"required": true, "content": {"application/json": {"schema": {"type": "object", "properties": {
		"id": {"type": "integer", "readOnly": true}
	}}}}}, "responses": {"200": {"description": "OK"}}}},
	"/tags": {"put": {"operationId": "bodies.tags", "requestBody": {"content": {"application/json": {"schema": {"type": "array", "items": {"type": "string"}}}}}, "responses": {"200": {"description": "OK"}}}},
	"/none": {"post": {"operationId": "bodies.none", "responses": {"200": {"description": "OK"}}}},
	"/merge": {"patch": {"operationId": "bodies.merge", "requestBody": {"content": {"application/merge-patch+json": {"schema": {"type": "object", "properties": {
		"name": {"type": ["string", "null"]},
		"size": {"type": "integer"}
	}}}}}, "responses": {"200": {"description": "OK"}}}},
	"/vnd": {"post": {"operationId": "bodies.vnd", "requestBody": {"required": true, "content": {"application/vnd.api+JSON; ext=bulk": {"schema": {"type": "object", "properties": {
		"data": {"type": "object"}
	}}}}}, "responses": {"200": {"description": "OK"}}}},
	"/both": {"patch": {"operationId": "bodies.both", "requestBody": {"content": {
		"application/merge-patch+json": {"schema": {"type": "object", "properties": {"patch": {"type": "string"}}}},
		"application/json": {"schema": {"type": "object", "properties": {"name": {"type": "string"}}}}
	}}, "responses": {"200": {"description": "OK"}}}},
	"/charset": {"post": {"operationId": "bodies.charset", "requestBody": {"content": {"application/json; charset=utf-8": {"schema": {"type": "object", "properties": {
		"name": {"type": "string"}
	}}}}}, "responses": {"200": {"description": "OK"}}}},
	"/upload": {"post": {"operationId": "bodies.upload", "requestBody": {"content": {"multipart/form-data": {"schema": {"type": "object", "properties": {
		"file": {"type": "string", "format": "binary"}
	}}}}}, "responses": {"200": {"description": "OK"}}}},
	"/lines": {"post": {"operationId": "bodies.lines", "requestBody": {"content": {"application/jsonl": {"schema": {"type": "object", "properties": {
		"name": {"type": "string"}
	}}}}}, "responses": {"200": {"description": "OK"}}}},
	"/sequence": {"post": {"operationId": "bodies.sequence", "requestBody": {"content": {"application/json-seq": {"schema": {"type": "object", "properties": {
		"name": {"type": "string"}
	}}}}}, "responses": {"200": {"description": "OK"}}}},
	"/suffix": {"post": {"operationId": "bodies.suffix", "requestBody": {"content": {"+json": {"schema": {"type": "object", "properties": {
		"name": {"type": "string"}
	}}}}}, "responses": {"200": {"description": "OK"}}}},
	"/slashless": {"post": {"operationId": "bodies.slashless", "requestBody": {"content": {"foo+json": {"schema": {"type": "object", "properties": {
		"name": {"type": "string"}
	}}}}}, "responses": {"200": {"description": "OK"}}}},
	"/patch": {"patch": {"operationId": "bodies.patch", "requestBody": {"content": {"application/json-patch+json": {"schema": {"type": "array", "items": {"type": "object", "properties": {
		"op": {"type": "string"},
		"path": {"type": "string"},
		"value": {}
	}}}}}}, "responses": {"200": {"description": "OK"}}}},
	"/redirect/{status}": {
		"get": {"operationId": "redirects.go", "parameters": [
			{"name": "status", "in": "path", "required": true, "schema": {"type": "integer"}},
			{"name": "to", "in": "query", "schema": {"type": "string"}},
			{"name": "error", "in": "query", "schema": {"type": "string"}}
		], "responses": {"200": {"description": "OK"}}},
		"head": {"operationId": "redirects.check", "parameters": [
			{"name": "status", "in": "path", "required": true, "schema": {"type": "integer"}},
			{"name": "to", "in": "query", "schema": {"type": "string"}}
		], "responses": {"200": {"description": "OK"}}},
		"post": {"operationId": "redirects.send", "parameters": [
			{"name": "status", "in": "path", "required": true, "schema": {"type": "integer"}},
			{"name": "to", "in": "query", "schema": {"type": "string"}},
			{"name": "error", "in": "query", "schema": {"type": "string"}}
		], "responses": {"200": {"description": "OK"}}}
	}
}}`

// bodyRequest is what bodyApp keeps of the last request.
type bodyRequest struct {
	method      string
	path        string
	contentType []string
	body        string
}

// bodyApp records the last request and answers {}, except under /redirect/
// where it answers the status of the path with the Location of the "to"
// query, when set, percent-decoded once more so a test can send bytes that
// are not UTF-8. A status below 300 there answers {"id":7}; a status of 300
// or more answers the compose error envelope with the code of the "error"
// query when set, else an HTML link.
type bodyApp struct {
	mu   sync.Mutex
	last bodyRequest
}

func (a *bodyApp) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	a.mu.Lock()
	a.last = bodyRequest{method: r.Method, path: r.URL.Path, contentType: r.Header.Values("Content-Type"), body: string(data)}
	a.mu.Unlock()

	status, found := strings.CutPrefix(r.URL.Path, "/redirect/")
	if found {
		code, err := strconv.Atoi(status)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		to, err := url.PathUnescape(r.URL.Query().Get("to"))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if to != "" {
			w.Header().Set("Location", to)
		}
		if code < 300 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"id":7}`))
			return
		}
		errorCode := r.URL.Query().Get("error")
		if errorCode != "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"error":{"code":"` + errorCode + `","message":"order changed"}}`))
			return
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte("<a href=\"" + to + "\">moved</a>"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte("{}"))
}

// request returns the last request the app received.
func (a *bodyApp) request() bodyRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.last
}

// bodySession serves bodySpec with a bodyApp and connects a client session.
func bodySession(t *testing.T, cfg Config) (*bodyApp, *Server, *mcp.ClientSession) {
	t.Helper()
	app := &bodyApp{}
	cfg.Spec = []byte(bodySpec)
	cfg.Handler = app
	srv, err := New(cfg)
	require.NoError(t, err)
	return app, srv, connect(t, srv, "")
}

// callBody calls a function through call_function and returns what the app
// received.
func callBody(t *testing.T, app *bodyApp, session *mcp.ClientSession, name string, args map[string]any) bodyRequest {
	t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	text, failed := callTool(t, session, "call_function", map[string]any{"name": name, "args": args})
	require.False(t, failed, text)
	return app.request()
}

func TestDispatchRequestBody(t *testing.T) {
	app, _, session := bodySession(t, Config{})

	cases := []struct {
		name        string
		function    string
		args        map[string]any
		contentType []string
		body        string
	}{
		{"optional without arguments", "bodies.optional", map[string]any{}, nil, ""},
		{"required absent without arguments", "bodies.absent", nil, nil, ""},
		{"required without arguments", "bodies.required", nil, []string{"application/json"}, `{}`},
		{"required without a schema", "bodies.noschema", nil, nil, ""},
		{"required with only readOnly properties", "bodies.readonly", nil, []string{"application/json"}, `{}`},
		{"optional with one argument", "bodies.optional", map[string]any{"size": 3}, []string{"application/json"}, `{"size":3}`},
		{"optional array without arguments", "bodies.tags", nil, nil, ""},
		{"optional array", "bodies.tags", map[string]any{"body": []string{"a"}}, []string{"application/json"}, `["a"]`},
		{"whole body null", "bodies.tags", map[string]any{"body": nil}, []string{"application/json"}, `null`},
		{"no request body", "bodies.none", nil, nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := callBody(t, app, session, tc.function, tc.args)
			assert.Equal(t, tc.contentType, got.contentType)
			assert.Equal(t, tc.body, got.body)
		})
	}
}

func TestDispatchJSONMediaTypes(t *testing.T) {
	app, srv, session := bodySession(t, Config{})

	text, failed := callTool(t, session, "describe_function", map[string]any{"names": []string{"bodies.merge", "bodies.vnd"}})
	require.False(t, failed, text)
	assert.Equal(t, "bodies.merge  PATCH /merge\n  args:\n    name  string?\n    size  integer\n\nbodies.vnd  POST /vnd\n  args:\n    data  object", text)

	text, failed = callTool(t, session, "search_functions", map[string]any{"module": "bodies"})
	require.False(t, failed, text)
	assert.Contains(t, text, "bodies.merge")
	assert.Contains(t, text, "bodies.vnd")
	assert.Contains(t, text, "bodies.patch")
	_, ok := srv.Catalog().Function("bodies.patch")
	assert.True(t, ok, "application/json-patch+json is a +json type")

	// None of these is JSON, so the operation is left out.
	for _, name := range []string{"bodies.upload", "bodies.lines", "bodies.sequence", "bodies.suffix", "bodies.slashless"} {
		assert.NotContains(t, text, name)
		_, ok = srv.Catalog().Function(name)
		assert.False(t, ok, name)
	}

	cases := []struct {
		name        string
		function    string
		args        map[string]any
		contentType string
		body        string
	}{
		// Merge patch deletes a member with null, so the null is sent.
		{"merge patch", "bodies.merge", map[string]any{"name": nil, "size": 2}, "application/merge-patch+json", `{"name":null,"size":2}`},
		{"json patch", "bodies.patch", map[string]any{"body": []any{map[string]any{"op": "remove", "path": "/name"}}}, "application/json-patch+json", `[{"op":"remove","path":"/name"}]`},
		{"vnd type in canonical form with parameters", "bodies.vnd", map[string]any{"data": map[string]any{"id": "1"}}, "application/vnd.api+json; ext=bulk", `{"data":{"id":"1"}}`},
		{"required +json without arguments", "bodies.vnd", nil, "application/vnd.api+json; ext=bulk", `{}`},
		{"application/json preferred", "bodies.both", map[string]any{"name": "x"}, "application/json", `{"name":"x"}`},
		{"application/json with charset", "bodies.charset", map[string]any{"name": "x"}, "application/json", `{"name":"x"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := callBody(t, app, session, tc.function, tc.args)
			assert.Equal(t, []string{tc.contentType}, got.contentType)
			assert.JSONEq(t, tc.body, got.body)
		})
	}
}

// mediaSpec returns a document with the one operation media.send, whose
// request body declares the given media type keys in order, each with an
// object schema of one property, name.
func mediaSpec(t *testing.T, keys ...string) []byte {
	t.Helper()
	var content []string
	for _, key := range keys {
		name, err := json.Marshal(key)
		require.NoError(t, err)
		content = append(content, string(name)+`: {"schema": {"type": "object", "properties": {"name": {"type": "string"}}}}`)
	}
	return []byte(`{"openapi": "3.1.0", "info": {"title": "Media", "version": "1"}, "paths": {
	"/send": {"post": {"operationId": "media.send", "requestBody": {"content": {` + strings.Join(content, ", ") + `}}, "responses": {"200": {"description": "OK"}}}}
}}`)
}

func TestDispatchMediaTypeKeys(t *testing.T) {
	// A +json key that does not parse, holds a "*" or a control byte other
	// than a tab, or has an empty part around the slash is not JSON, and
	// neither is a type that only starts with application/json.
	refused := []string{
		"application/x+json; p=\"a\r\nX-Injected: 1\"",
		"application/x\r\nX-Injected: y+json",
		"application/x\x00+json",
		"application/*+json",
		"application/ merge-patch +json",
		"application/+json",
		"/x+json",
		"application/x+json; p=\"a\x7fb\"",
		"application/json*",
		"application/json, text/plain",
		"application/jsonl",
		"application/x+json; p*=utf-8''%0D%0A",
		"application/x+json; profile*=iso-8859-1''abc",
		"application/x+json; p*0=a; p*2=c",
	}
	for _, key := range refused {
		srv, err := New(Config{Spec: mediaSpec(t, key), Handler: &bodyApp{}})
		require.NoError(t, err, "%q", key)
		_, ok := srv.Catalog().Function("media.send")
		assert.False(t, ok, "%q", key)
	}

	cases := []struct {
		name        string
		keys        []string
		contentType string
	}{
		{"type in lower case, parameters kept", []string{"application/vnd.api+JSON; ext=bulk"}, "application/vnd.api+json; ext=bulk"},
		{"surrounding space", []string{" application/merge-patch+json "}, "application/merge-patch+json"},
		{"application/json in any case, parameters dropped", []string{"Application/JSON; charset=utf-8"}, "application/json"},
		{"application/json before an earlier +json key", []string{"application/merge-patch+json", "Application/JSON; charset=utf-8"}, "application/json"},
		{"first +json key in document order", []string{"application/merge-patch+json", "application/vnd.api+json"}, "application/merge-patch+json"},
		{"first +json key in reverse order", []string{"application/vnd.api+json", "application/merge-patch+json"}, "application/vnd.api+json"},
		{"refused key skipped", []string{"application/*+json", "application/merge-patch+json"}, "application/merge-patch+json"},
		{"tab around a parameter", []string{"application/json;\tcharset=utf-8"}, "application/json"},
		{"application/json with a parameter without a value", []string{"application/json; charset"}, "application/json"},
		{"application/json with a parameter given twice", []string{"application/json; v=1; v=2"}, "application/json"},
		{"application/json with an unquoted URL parameter", []string{"application/json; profile=https://example.com/schema"}, "application/json"},
		{"application/json with a star in a parameter", []string{"application/json; profile=\"https://example.com/*\""}, "application/json"},
		{"application/json with a control byte in a parameter", []string{"application/json; p=\"a\x7fb\""}, "application/json"},
		{"application/json with a no-break space after it", []string{"application/json\u00a0"}, "application/json"},
		{"application/json with a no-break space before its parameters", []string{"application/json\u00a0; charset=utf-8"}, "application/json"},
		{"application/json with a line break after it", []string{"application/json\r\n"}, "application/json"},
		{"application/json with parameters before a +json key", []string{"application/merge-patch+json", "application/json; charset"}, "application/json"},
		{"tab inside a quoted parameter", []string{"application/x+json; p=\"a\tb\""}, "application/x+json; p=\"a\tb\""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := &bodyApp{}
			srv, err := New(Config{Spec: mediaSpec(t, tc.keys...), Handler: app})
			require.NoError(t, err)
			session := connect(t, srv, "")
			got := callBody(t, app, session, "media.send", map[string]any{"name": "x"})
			assert.Equal(t, []string{tc.contentType}, got.contentType)
			assert.JSONEq(t, `{"name":"x"}`, got.body)
		})
	}
}

func TestDispatchRedirect(t *testing.T) {
	var mu sync.Mutex
	var reported []error
	app, _, session := bodySession(t, Config{
		WhoAmI: "bodies.none",
		Script: ScriptConfig{Enabled: true},
		OnError: func(ctx context.Context, err error) {
			mu.Lock()
			defer mu.Unlock()
			reported = append(reported, err)
		},
	})
	call := func(name string, status int, to string) (string, bool) {
		args := map[string]any{"status": status}
		if to != "" {
			args["to"] = to
		}
		return callTool(t, session, "call_function", map[string]any{"name": name, "args": args})
	}

	// A GET changed nothing, so every redirect status is its result.
	for _, status := range []int{301, 302, 303, 307, 308} {
		text, failed := call("redirects.go", status, "/orders/7")
		assert.False(t, failed, text)
		assert.Equal(t, fmt.Sprintf(`{"redirect":{"status":%d,"location":"/orders/7"}}`, status), text)
		assert.Equal(t, bodyRequest{method: "GET", path: fmt.Sprintf("/redirect/%d", status)}, app.request(), "the redirect is not followed")
	}

	// A HEAD changed nothing either.
	for _, status := range []int{301, 302, 303, 307, 308} {
		text, failed := call("redirects.check", status, "/orders/7")
		assert.False(t, failed, text)
		assert.Equal(t, fmt.Sprintf(`{"redirect":{"status":%d,"location":"/orders/7"}}`, status), text)
		assert.Equal(t, "HEAD", app.request().method)
	}

	// A write gets 302 and 303 as its result.
	for _, status := range []int{302, 303} {
		text, failed := call("redirects.send", status, "/orders/7")
		assert.False(t, failed, text)
		assert.Equal(t, fmt.Sprintf(`{"redirect":{"status":%d,"location":"/orders/7"}}`, status), text)
	}

	// A write answered with 301, 307 or 308 was not carried out.
	for _, status := range []int{301, 307, 308} {
		text, failed := call("redirects.send", status, "/orders/7")
		assert.True(t, failed, text)
		assert.Equal(t, fmt.Sprintf("error internal: %d %s, Location: /orders/7", status, http.StatusText(status)), text)
	}

	// Other 3xx statuses are errors that name the location.
	for _, status := range []int{300, 304} {
		text, failed := call("redirects.go", status, "/orders/7")
		assert.True(t, failed, text)
		assert.Equal(t, fmt.Sprintf("error internal: %d %s, Location: /orders/7", status, http.StatusText(status)), text)
	}

	// A 3xx that is not a result and answers the compose error envelope is
	// that error; a redirect result still ignores the body.
	errorCall := func(name string, status int) (string, bool) {
		return callTool(t, session, "call_function", map[string]any{"name": name, "args": map[string]any{"status": status, "to": "/orders/7", "error": ez.ECONFLICT}})
	}
	text, failed := errorCall("redirects.send", 307)
	assert.True(t, failed, text)
	assert.Equal(t, "error "+ez.ECONFLICT+": order changed", text)
	text, failed = errorCall("redirects.go", 300)
	assert.True(t, failed, text)
	assert.Equal(t, "error "+ez.ECONFLICT+": order changed", text)
	text, failed = errorCall("redirects.send", 303)
	assert.False(t, failed, text)
	assert.Equal(t, `{"redirect":{"status":303,"location":"/orders/7"}}`, text)

	// A 4xx or 5xx with a Location is the error of its body, without the
	// location.
	text, failed = call("redirects.go", 404, "/orders/7")
	assert.True(t, failed, text)
	assert.Equal(t, "error "+ez.ENOTFOUND+`: <a href="/orders/7">moved</a>`, text)
	text, failed = call("redirects.send", 500, "/orders/7")
	assert.True(t, failed, text)
	assert.Equal(t, `error internal: <a href="/orders/7">moved</a>`, text)

	// Without a Location the error quotes the body.
	text, failed = call("redirects.go", 302, "")
	assert.True(t, failed, text)
	assert.Equal(t, `error internal: <a href="">moved</a>`, text)

	// Space around a location is trimmed, and a location of only space is
	// no location.
	text, failed = call("redirects.go", 302, "  /orders/7  ")
	assert.False(t, failed, text)
	assert.Equal(t, `{"redirect":{"status":302,"location":"/orders/7"}}`, text)
	text, failed = call("redirects.go", 302, "   ")
	assert.True(t, failed, text)
	assert.Equal(t, `error internal: <a href="   ">moved</a>`, text)

	// A success with a Location is an ordinary result.
	for _, status := range []int{200, 201} {
		text, failed = call("redirects.send", status, "/orders/7")
		assert.False(t, failed, text)
		assert.Equal(t, `{"id":7}`, text)
	}

	// HTML characters arrive as written.
	text, failed = call("redirects.go", 302, "/search?a=1&b=<c>")
	assert.False(t, failed, text)
	assert.Equal(t, `{"redirect":{"status":302,"location":"/search?a=1&b=<c>"}}`, text)

	// Line breaks and control characters are dropped.
	text, failed = call("redirects.go", 302, "/a%0D%0Ab%09c%01d")
	assert.False(t, failed, text)
	assert.Equal(t, `{"redirect":{"status":302,"location":"/abcd"}}`, text)
	assert.NotContains(t, text, "\n")
	assert.NotContains(t, text, "\r")

	// Line and paragraph separators and format characters are dropped.
	text, failed = call("redirects.go", 302, "/a b ‮c")
	assert.False(t, failed, text)
	assert.Equal(t, `{"redirect":{"status":302,"location":"/abc"}}`, text)
	text, failed = call("redirects.send", 307, "/a b ‮c")
	assert.True(t, failed, text)
	assert.Equal(t, "error internal: 307 Temporary Redirect, Location: /abc", text)

	// Bytes that are not UTF-8 are dropped.
	text, failed = call("redirects.go", 302, "/a%FFb")
	assert.False(t, failed, text)
	assert.Equal(t, `{"redirect":{"status":302,"location":"/ab"}}`, text)

	// A long location is cut on a character boundary.
	long := "/" + strings.Repeat("é", 400)
	text, failed = call("redirects.go", 302, long)
	assert.False(t, failed, text)
	assert.Equal(t, `{"redirect":{"status":302,"location":"`+long[:299]+`","truncated":true}}`, text)
	text, failed = call("redirects.send", 307, long)
	assert.True(t, failed, text)
	assert.Equal(t, "error internal: 307 Temporary Redirect, Location: "+long[:299]+" [truncated]", text)

	// A script receives the object.
	text, failed = callTool(t, session, "run_script", map[string]any{"code": `const r = api.redirects.go({status: 302, to: "/x"})
return [r.redirect.status, r.redirect.location]`})
	assert.False(t, failed, text)
	assert.Equal(t, `[302,"/x"]`, text)

	mu.Lock()
	defer mu.Unlock()
	assert.Empty(t, reported, "neither a redirect nor a REST error goes to OnError")
}
