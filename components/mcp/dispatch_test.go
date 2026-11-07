package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
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
	Method     string            `json:"method"`
	Path       string            `json:"path"`
	Query      string            `json:"query"`
	Headers    map[string]string `json:"headers"`
	Body       string            `json:"body"`
	RemoteAddr string            `json:"remote_addr"`
}

// dispatchApp echoes every request, except /report which answers CSV.
func dispatchApp() http.Handler {
	e := echo.New()
	e.GET("/report", func(c echo.Context) error {
		return c.Blob(http.StatusOK, "text/csv", []byte("a,b\n1,2\n"))
	})
	e.Any("/*", func(c echo.Context) error {
		r := c.Request()
		data, err := io.ReadAll(r.Body)
		if err != nil {
			return err
		}
		out := dispatchEcho{
			Method:     r.Method,
			Path:       r.URL.EscapedPath(),
			Query:      r.URL.RawQuery,
			Headers:    map[string]string{},
			Body:       string(data),
			RemoteAddr: r.RemoteAddr,
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

func TestDispatchRemoteAddr(t *testing.T) {
	c := dispatchCaller(t, http.Header{})
	got := callDispatch(t, c, "items.get", map[string]any{"id": 1})
	assert.Equal(t, "192.0.2.1:1234", got.RemoteAddr, "without an MCP request the httptest placeholder stays")

	ctx := context.WithValue(context.Background(), remoteAddrKey{}, "203.0.113.9:4321")
	raw, err := c.Call(ctx, "items.get", map[string]any{"id": 1})
	require.NoError(t, err)
	var out dispatchEcho
	require.NoError(t, json.Unmarshal(raw, &out), string(raw))
	assert.Equal(t, "203.0.113.9:4321", out.RemoteAddr)
}

func TestDispatchRemoteAddrThroughHandler(t *testing.T) {
	srv, err := New(Config{Spec: []byte(dispatchSpec), Handler: dispatchApp()})
	require.NoError(t, err)
	session := connect(t, srv, "")

	text, failed := callTool(t, session, "call_function", map[string]any{"name": "items.get", "args": map[string]any{"id": 1}})
	require.False(t, failed, text)
	var got dispatchEcho
	require.NoError(t, json.Unmarshal([]byte(text), &got), text)
	assert.True(t, strings.HasPrefix(got.RemoteAddr, "127.0.0.1:"), "the app sees the MCP client's address, got %q", got.RemoteAddr)
	assert.NotEqual(t, "192.0.2.1:1234", got.RemoteAddr)
}
