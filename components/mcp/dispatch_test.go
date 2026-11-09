package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/labstack/echo/v5"
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

	got = callDispatch(t, c, "items.patch", map[string]any{"id": "x"})
	assert.JSONEq(t, `{}`, got.Body, "arguments not provided are not sent")

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
