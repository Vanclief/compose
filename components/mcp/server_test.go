package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vanclief/ez"
)

// shopEcho is what the fixture app echoes back for the main routes.
type shopEcho struct {
	ID          int                 `json:"id"`
	Method      string              `json:"method"`
	Key         string              `json:"key"`
	Secret      string              `json:"secret"`
	Path        map[string]string   `json:"path"`
	Query       map[string][]string `json:"query"`
	Body        any                 `json:"body"`
	ContentType string              `json:"content_type"`
	Accept      string              `json:"accept"`
	RequestID   string              `json:"request_id"`
}

// shopError writes the compose error envelope.
func shopError(c echo.Context, status int, code, message string) error {
	return c.JSON(status, map[string]any{"error": map[string]string{"code": code, "message": message, "request_id": "req-1"}})
}

// echoShopRequest answers with everything the request carried.
func echoShopRequest(c echo.Context, status int) error {
	r := c.Request()
	out := shopEcho{
		Method:      r.Method,
		Key:         r.Header.Get("API-KEY"),
		Secret:      r.Header.Get("API-SECRET"),
		Path:        map[string]string{},
		Query:       r.URL.Query(),
		ContentType: r.Header.Get("Content-Type"),
		Accept:      r.Header.Get("Accept"),
		RequestID:   r.Header.Get("X-Request-Id"),
	}
	for _, name := range c.ParamNames() {
		out.Path[name] = c.Param(name)
	}
	id, err := strconv.Atoi(c.Param("order_id"))
	if err != nil {
		return shopError(c, http.StatusBadRequest, ez.EINVALID, "order_id must be an integer")
	}
	out.ID = id

	data, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	if len(data) > 0 {
		err = json.Unmarshal(data, &out.Body)
		if err != nil {
			return shopError(c, http.StatusBadRequest, ez.EINVALID, "body is not JSON")
		}
	}
	return c.JSON(status, out)
}

// shopApp serves the routes of testdata/server.json.
func shopApp() http.Handler {
	e := echo.New()
	e.GET("/me", func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"key": c.Request().Header.Get("API-KEY")})
	})
	e.GET("/orders/:order_id", func(c echo.Context) error {
		if c.Param("order_id") == "999" {
			return shopError(c, http.StatusNotFound, ez.ENOTFOUND, "order 999 not found")
		}
		return echoShopRequest(c, http.StatusOK)
	})
	e.DELETE("/orders/:order_id", func(c echo.Context) error {
		return c.String(http.StatusConflict, "order already shipped\n")
	})
	e.POST("/orders/:order_id/lines", func(c echo.Context) error {
		var body map[string]any
		err := json.NewDecoder(c.Request().Body).Decode(&body)
		if err != nil {
			return shopError(c, http.StatusBadRequest, ez.EINVALID, "body is not JSON")
		}
		if body["sku"] == nil {
			return shopError(c, http.StatusBadRequest, ez.EINVALID, "sku is required")
		}
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		c.Request().Body = io.NopCloser(strings.NewReader(string(data)))
		return echoShopRequest(c, http.StatusCreated)
	})
	e.PUT("/orders/:order_id/tags", func(c echo.Context) error {
		return echoShopRequest(c, http.StatusOK)
	})
	e.GET("/items", func(c echo.Context) error {
		limit, err := strconv.Atoi(c.QueryParam("limit"))
		if err != nil {
			limit = 10
		}
		items := make([]map[string]any, 0, limit)
		for i := range limit {
			items = append(items, map[string]any{"id": i + 1, "name": "item " + strings.Repeat("x", 80)})
		}
		return c.JSON(http.StatusOK, items)
	})
	return e
}

type orderSummaryInput struct {
	OrderID int `json:"order_id" jsonschema:"Order to summarize"`
}

type OrderSummary struct {
	OrderID int    `json:"order_id"`
	Key     string `json:"key"`
	Items   int    `json:"items"`
}

// orderSummary is a composite that calls two functions through the Caller.
func orderSummary() Composite {
	c := NewComposite("orders", "summary", func(ctx context.Context, call Caller, in orderSummaryInput) (OrderSummary, error) {
		raw, err := call.Call(ctx, "orders.get_order", map[string]any{"order_id": in.OrderID})
		if err != nil {
			return OrderSummary{}, ez.Wrap(err)
		}
		var order shopEcho
		err = json.Unmarshal(raw, &order)
		if err != nil {
			return OrderSummary{}, ez.Wrap(err)
		}
		raw, err = call.Call(ctx, "items.list_items", map[string]any{"limit": 2})
		if err != nil {
			return OrderSummary{}, ez.Wrap(err)
		}
		var items []json.RawMessage
		err = json.Unmarshal(raw, &items)
		if err != nil {
			return OrderSummary{}, ez.Wrap(err)
		}
		return OrderSummary{OrderID: order.ID, Key: order.Key, Items: len(items)}, nil
	})
	c.Summary = "Summarize an order"
	c.Description = "Reads the order and counts two catalog items."
	c.ReadOnly = true
	return c
}

func shopConfig(t *testing.T) Config {
	t.Helper()
	spec, err := os.ReadFile("testdata/server.json")
	require.NoError(t, err)
	return Config{
		Spec:         spec,
		Handler:      shopApp(),
		Instructions: "Shop is a test store.",
		WhoAmI:       "accounts.me",
		Composites:   []Composite{orderSummary()},
		Concepts: map[string]Concept{
			"Order": {Text: "A purchase placed by a customer.", Aliases: []string{"pedido"}},
		},
	}
}

func newShopServer(t *testing.T, cfg Config) *Server {
	t.Helper()
	s, err := New(cfg)
	require.NoError(t, err)
	return s
}

// bearerTransport adds the bearer token to every request.
type bearerTransport struct {
	token string
}

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if b.token != "" {
		r.Header.Set("Authorization", "Bearer "+b.token)
	}
	return http.DefaultTransport.RoundTrip(r)
}

// connect serves s over HTTP and returns a client session that sends token.
func connect(t *testing.T, s *Server, token string) *mcp.ClientSession {
	t.Helper()
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	hc := &http.Client{Transport: bearerTransport{token: token}}
	client := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: ts.URL, HTTPClient: hc}, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		session.Close()
	})
	return session
}

// callTool calls a tool and returns its text and error flag.
func callTool(t *testing.T, session *mcp.ClientSession, name string, args any) (string, bool) {
	t.Helper()
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	require.NoError(t, err)
	require.Len(t, res.Content, 1)
	text, ok := res.Content[0].(*mcp.TextContent)
	require.True(t, ok, "content is %T", res.Content[0])
	return text.Text, res.IsError
}

func toolNames(t *testing.T, session *mcp.ClientSession) []string {
	t.Helper()
	res, err := session.ListTools(context.Background(), nil)
	require.NoError(t, err)
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	return names
}

func TestServerNewErrors(t *testing.T) {
	cfg := shopConfig(t)
	cfg.Spec = nil
	_, err := New(cfg)
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))

	cfg = shopConfig(t)
	cfg.Handler = nil
	_, err = New(cfg)
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))

	cfg = shopConfig(t)
	cfg.WhoAmI = "accounts.nobody"
	_, err = New(cfg)
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	assert.Contains(t, ez.ErrorMessage(err), "accounts.nobody")

	// whoami calls the function with no arguments, so one that requires an
	// argument would fail on every call.
	cfg = shopConfig(t)
	cfg.WhoAmI = "orders.get_order"
	_, err = New(cfg)
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	assert.Contains(t, ez.ErrorMessage(err), "order_id is required")
}

func TestServerTools(t *testing.T) {
	// The SDK lists tools sorted by name.
	s := newShopServer(t, shopConfig(t))
	session := connect(t, s, "key:secret")
	assert.Equal(t, []string{"call_function", "describe_function", "explain_term", "search_functions", "whoami"}, toolNames(t, session))

	cfg := shopConfig(t)
	cfg.Script.Enabled = true
	scripted := connect(t, newShopServer(t, cfg), "key:secret")
	assert.Equal(t, []string{"call_function", "describe_function", "explain_term", "run_script", "search_functions", "whoami"}, toolNames(t, scripted))
}

func TestServerInstructions(t *testing.T) {
	s := newShopServer(t, shopConfig(t))
	session := connect(t, s, "key:secret")

	instructions := session.InitializeResult().Instructions
	assert.Equal(t, s.Instructions(), instructions)
	assert.True(t, strings.HasPrefix(instructions, "Shop is a test store.\n\nModules (pass one to search_functions):\n"))
	assert.Contains(t, instructions, "- accounts: Who is calling.\n- items: 1 function\n- orders: Customer orders and their lines.\n")
	assert.NotContains(t, instructions, "run_script")

	cfg := shopConfig(t)
	cfg.Script.Enabled = true
	assert.Contains(t, newShopServer(t, cfg).Instructions(), "run_script chains several read-only calls")
}

func TestServerWhoAmI(t *testing.T) {
	s := newShopServer(t, shopConfig(t))
	text, failed := callTool(t, connect(t, s, "key:secret"), "whoami", nil)
	assert.False(t, failed)
	assert.JSONEq(t, `{"key":"key"}`, text)

	cfg := shopConfig(t)
	cfg.WhoAmI = ""
	plain := newShopServer(t, cfg)
	text, failed = callTool(t, connect(t, plain, "key:secret"), "whoami", map[string]any{})
	assert.False(t, failed)
	assert.Equal(t, "server Shop 2.3.0\nbearer token: present", text)

	text, failed = callTool(t, connect(t, plain, ""), "whoami", nil)
	assert.False(t, failed)
	assert.Equal(t, "server Shop 2.3.0\nbearer token: none", text)
}

func TestServerMalformedBearer(t *testing.T) {
	s := newShopServer(t, shopConfig(t))
	session := connect(t, s, "nocolon")

	text, failed := callTool(t, session, "whoami", nil)
	assert.True(t, failed)
	assert.Equal(t, "error invalid: bearer token must be key:secret", text)

	text, failed = callTool(t, session, "call_function", map[string]any{"name": "accounts.me"})
	assert.True(t, failed)
	assert.Equal(t, "error invalid: bearer token must be key:secret", text)
}

func TestServerCatalogTools(t *testing.T) {
	s := newShopServer(t, shopConfig(t))
	session := connect(t, s, "key:secret")
	c := s.Catalog()

	text, failed := callTool(t, session, "search_functions", nil)
	assert.False(t, failed)
	assert.Equal(t, c.renderSearch("", ""), text)

	text, _ = callTool(t, session, "search_functions", map[string]any{"module": "orders", "query": "line"})
	assert.Equal(t, c.renderSearch("orders", "line"), text)
	assert.Contains(t, text, "orders.add_line")

	text, _ = callTool(t, session, "describe_function", map[string]any{"names": []string{"orders.add_line", "orders.summary"}})
	assert.Equal(t, c.renderDescribe([]string{"orders.add_line", "orders.summary"}), text)

	text, _ = callTool(t, session, "explain_term", map[string]any{"terms": []string{"pedido"}})
	assert.Equal(t, c.renderExplain([]string{"Order"}), text)
	assert.True(t, strings.HasPrefix(text, "Order  (aliases: pedido)\n  A purchase placed by a customer."))

	// The schema requires at least one name.
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "describe_function", Arguments: map[string]any{"names": []string{}}})
	require.NoError(t, err)
	assert.True(t, res.IsError)
}

// callEcho calls a function that the fixture echoes and decodes the echo.
func callEcho(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) shopEcho {
	t.Helper()
	text, failed := callTool(t, session, "call_function", map[string]any{"name": name, "args": args})
	require.False(t, failed, text)
	var out shopEcho
	require.NoError(t, json.Unmarshal([]byte(text), &out), text)
	return out
}

func TestServerCallFunction(t *testing.T) {
	s := newShopServer(t, shopConfig(t))
	session := connect(t, s, "key:secret")

	got := callEcho(t, session, "orders.get_order", map[string]any{"order_id": 7, "expand": []string{"lines", "customer"}, "verbose": true})
	assert.Equal(t, "GET", got.Method)
	assert.Equal(t, "key", got.Key)
	assert.Equal(t, "secret", got.Secret)
	assert.Equal(t, map[string]string{"order_id": "7"}, got.Path)
	assert.Equal(t, map[string][]string{"expand": {"lines", "customer"}, "verbose": {"true"}}, got.Query)
	assert.Nil(t, got.Body)
	assert.Equal(t, "", got.ContentType)
	assert.Equal(t, "application/json", got.Accept)

	got = callEcho(t, session, "orders.add_line", map[string]any{"order_id": 7, "sku": "A1", "quantity": 2})
	assert.Equal(t, "POST", got.Method)
	assert.Equal(t, map[string]string{"order_id": "7"}, got.Path)
	assert.Empty(t, got.Query)
	assert.Equal(t, map[string]any{"sku": "A1", "quantity": float64(2)}, got.Body)
	assert.Equal(t, "application/json", got.ContentType)
	assert.Equal(t, "secret", got.Secret)

	got = callEcho(t, session, "orders.set_tags", map[string]any{"order_id": 7, "body": []string{"gift", "rush"}, "X-Request-Id": "r-42"})
	assert.Equal(t, "PUT", got.Method)
	assert.Equal(t, []any{"gift", "rush"}, got.Body)
	assert.Equal(t, "r-42", got.RequestID)

	// A body function called without body arguments sends an empty object.
	got = callEcho(t, session, "orders.set_tags", map[string]any{"order_id": 8})
	assert.Equal(t, map[string]any{}, got.Body)
}

func TestServerCallFunctionErrors(t *testing.T) {
	s := newShopServer(t, shopConfig(t))
	session := connect(t, s, "key:secret")
	describeAddLine := s.Catalog().renderDescribe([]string{"orders.add_line"})
	describeGetOrder := s.Catalog().renderDescribe([]string{"orders.get_order"})

	text, failed := callTool(t, session, "call_function", map[string]any{"name": "orders.add_line", "args": map[string]any{"order_id": 7, "quantity": 1}})
	assert.True(t, failed)
	assert.Equal(t, "error invalid: sku is required\n\n"+describeAddLine, text)

	text, failed = callTool(t, session, "call_function", map[string]any{"name": "orders.get_order", "args": map[string]any{"order_id": 999}})
	assert.True(t, failed)
	assert.Equal(t, "error not_found: order 999 not found", text)

	text, failed = callTool(t, session, "call_function", map[string]any{"name": "orders.nope"})
	assert.True(t, failed)
	assert.Equal(t, "error not_found: unknown function orders.nope; use search_functions", text)

	text, failed = callTool(t, session, "call_function", map[string]any{"name": "orders.get_order", "args": map[string]any{"order_id": 7, "color": "red", "id": 1}})
	assert.True(t, failed)
	assert.Equal(t, "error invalid: unknown arguments color, id for orders.get_order; valid: order_id, expand, verbose\n\n"+describeGetOrder, text)

	text, failed = callTool(t, session, "call_function", map[string]any{"name": "accounts.me", "args": map[string]any{"verbose": true}})
	assert.True(t, failed)
	assert.True(t, strings.HasPrefix(text, "error invalid: accounts.me takes no arguments, got verbose\n\n"), text)

	text, failed = callTool(t, session, "call_function", map[string]any{"name": "orders.get_order", "args": map[string]any{}})
	assert.True(t, failed)
	assert.True(t, strings.HasPrefix(text, "error invalid: orders.get_order requires the path argument order_id\n\n"), text)

	// A response that is not the error envelope is classified by status.
	text, failed = callTool(t, session, "call_function", map[string]any{"name": "orders.cancel_order", "args": map[string]any{"order_id": 7}})
	assert.True(t, failed)
	assert.Equal(t, "error conflict: order already shipped", text)
}

func TestServerTruncatesOutput(t *testing.T) {
	s := newShopServer(t, shopConfig(t))
	session := connect(t, s, "key:secret")

	text, failed := callTool(t, session, "call_function", map[string]any{"name": "items.list_items", "args": map[string]any{"limit": 500}})
	assert.False(t, failed)
	assert.Contains(t, text, "\n\n[output cut at 24000 bytes of ")
	assert.Less(t, len(text), 24300)
}

func TestServerComposite(t *testing.T) {
	s := newShopServer(t, shopConfig(t))
	session := connect(t, s, "other:pass")

	text, failed := callTool(t, session, "call_function", map[string]any{"name": "orders.summary", "args": map[string]any{"order_id": 7}})
	assert.False(t, failed, text)
	assert.JSONEq(t, `{"order_id":7,"key":"other","items":2}`, text)

	text, failed = callTool(t, session, "call_function", map[string]any{"name": "orders.summary", "args": map[string]any{"order_id": 999}})
	assert.True(t, failed)
	assert.Equal(t, "error not_found: order 999 not found", text)
}

func TestServerRunScript(t *testing.T) {
	cfg := shopConfig(t)
	cfg.Script.Enabled = true
	s := newShopServer(t, cfg)
	session := connect(t, s, "key:secret")

	code := `const o = api.orders.get_order({order_id: 7})
const items = api.items.list_items({limit: 3})
console.log("items", items.length)
return {id: o.id, key: o.key, items: items.length}`
	text, failed := callTool(t, session, "run_script", map[string]any{"code": code})
	assert.False(t, failed, text)
	assert.Equal(t, `{"id":7,"key":"key","items":3}`+"\n"+`log: items 3`, text)

	code = `console.log("before")
api.orders.cancel_order({order_id: 7})`
	text, failed = callTool(t, session, "run_script", map[string]any{"code": code})
	assert.True(t, failed)
	assert.True(t, strings.HasPrefix(text, "error: script error: "), text)
	assert.Contains(t, text, "\nlog: before")
	assert.Contains(t, text, "not read-only")
}

func TestServerRunScriptHostileValue(t *testing.T) {
	cfg := shopConfig(t)
	cfg.Script.Enabled = true
	session := connect(t, newShopServer(t, cfg), "key:secret")

	text, failed := callTool(t, session, "run_script", map[string]any{"code": `return {get x() { throw new Error("boom") }}`})
	assert.True(t, failed)
	assert.Equal(t, "error: script error: Error: boom (line 1)", text)

	text, failed = callTool(t, session, "run_script", map[string]any{"code": `throw {toString() { throw 1 }}`})
	assert.True(t, failed)
	assert.Equal(t, "error: script error: [object Object]", text)

	text, failed = callTool(t, session, "run_script", map[string]any{"code": `return 1`})
	assert.False(t, failed)
	assert.Equal(t, "1", text)
}

func TestServerRunScriptRequiresWhoAmI(t *testing.T) {
	cfg := shopConfig(t)
	cfg.Script.Enabled = true
	cfg.WhoAmI = ""
	_, err := New(cfg)
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	assert.Contains(t, err.Error(), "Config.Script.Enabled requires Config.WhoAmI")
}

// TestServerRunScriptChecksWhoAmI: BearerToHeaders accepts any key:secret
// token, so with WhoAmI set run_script runs only for credentials the REST
// handler accepts.
func TestServerRunScriptAllow(t *testing.T) {
	cfg := shopConfig(t)
	cfg.Script.Enabled = true
	var seen json.RawMessage
	cfg.Script.Allow = func(ctx context.Context, whoami json.RawMessage) bool {
		seen = whoami
		return strings.Contains(string(whoami), `"role": "admin"`)
	}
	s := newShopServer(t, cfg)

	text, failed := callTool(t, connect(t, s, "key:secret"), "run_script", map[string]any{"code": "return 1"})
	assert.True(t, failed)
	assert.Equal(t, "error not_authorized: run_script is not enabled for this account", text)
	assert.Contains(t, string(seen), "key")
}

func TestServerRunScriptChecksWhoAmI(t *testing.T) {
	shop := shopApp()
	cfg := shopConfig(t)
	cfg.Script.Enabled = true
	cfg.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("API-KEY") == "revoked" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error": {"code": "not_authenticated", "message": "invalid API key", "request_id": "req-1"}}`)
			return
		}
		shop.ServeHTTP(w, r)
	})
	s := newShopServer(t, cfg)
	code := `console.log("ran")
return 1`

	text, failed := callTool(t, connect(t, s, "revoked:x"), "run_script", map[string]any{"code": code})
	assert.True(t, failed)
	assert.Equal(t, "error not_authenticated: run_script requires credentials the API accepts", text)

	text, failed = callTool(t, connect(t, s, "key:secret"), "run_script", map[string]any{"code": code})
	assert.False(t, failed, text)
	assert.Equal(t, "1\nlog: ran", text)
}

func TestServerRecoversToolPanic(t *testing.T) {
	boom := NewComposite("orders", "boom", func(ctx context.Context, call Caller, in orderSummaryInput) (OrderSummary, error) {
		panic("kaboom")
	})
	boom.Summary = "Always panics"
	cfg := shopConfig(t)
	cfg.Composites = append(cfg.Composites, boom)
	session := connect(t, newShopServer(t, cfg), "key:secret")

	text, failed := callTool(t, session, "call_function", map[string]any{"name": "orders.boom", "args": map[string]any{"order_id": 7}})
	assert.True(t, failed)
	assert.Equal(t, "error internal: call_function panicked: kaboom", text)

	text, failed = callTool(t, session, "call_function", map[string]any{"name": "orders.summary", "args": map[string]any{"order_id": 7}})
	assert.False(t, failed, text)
}

// TestServerRunScriptReportsFailures: a panic in a function a script calls
// and an internal failure of the runtime reach Config.OnError; the agent
// sees generic text.
func TestServerRunScriptReportsFailures(t *testing.T) {
	boom := NewComposite("orders", "boom", func(ctx context.Context, call Caller, in orderSummaryInput) (OrderSummary, error) {
		panic("kaboom")
	})
	boom.Summary = "Always panics"
	boom.ReadOnly = true
	cfg := shopConfig(t)
	cfg.Script.Enabled = true
	cfg.Composites = append(cfg.Composites, boom)
	var mu sync.Mutex
	var reported []error
	cfg.OnError = func(ctx context.Context, err error) {
		mu.Lock()
		defer mu.Unlock()
		reported = append(reported, err)
	}
	s := newShopServer(t, cfg)
	session := connect(t, s, "key:secret")

	text, failed := callTool(t, session, "run_script", map[string]any{"code": `return api.orders.boom({order_id: 7})`})
	assert.True(t, failed)
	assert.Equal(t, "error: script error: Error: orders.boom: the call failed (line 1)", text)
	mu.Lock()
	require.Len(t, reported, 1)
	assert.Contains(t, reported[0].Error(), "orders.boom panicked: kaboom")
	assert.Contains(t, reported[0].Error(), "goroutine ")
	mu.Unlock()

	broken, err := newScriptEngine()
	require.NoError(t, err)
	require.NoError(t, broken.runtime.Close(context.Background()))
	s.engine = broken
	text, failed = callTool(t, session, "run_script", map[string]any{"code": `return 1`})
	assert.True(t, failed)
	assert.Equal(t, "error: cannot start the script runtime", text)
	mu.Lock()
	require.Len(t, reported, 2)
	assert.Equal(t, ez.EINTERNAL, ez.ErrorCode(reported[1]))
	mu.Unlock()
}

// TestServerSharesScriptEngine: every Server of the process runs scripts on
// the one engine, so creating Servers does not accumulate compiled modules.
func TestServerSharesScriptEngine(t *testing.T) {
	cfg := shopConfig(t)
	cfg.Script.Enabled = true
	first := newShopServer(t, cfg)
	second := newShopServer(t, cfg)
	assert.Same(t, first.engine, second.engine)
}

func TestServerSnapshot(t *testing.T) {
	cfg := shopConfig(t)
	cfg.Script.Enabled = true
	s := newShopServer(t, cfg)

	first, err := s.Snapshot()
	require.NoError(t, err)
	second, err := s.Snapshot()
	require.NoError(t, err)
	assert.Equal(t, string(first), string(second))
	golden(t, "snapshot", string(first))

	info := s.referenceInfo()
	assert.Equal(t, "Shop", info.Name)
	assert.Equal(t, "2.3.0", info.Version)
	require.Len(t, info.Tools, 6)
	assert.Equal(t, "call_function", info.Tools[4].Name)
	assert.Contains(t, string(info.Tools[4].InputSchema), `"required":["name"]`)
}

func TestBearerToHeaders(t *testing.T) {
	auth := BearerToHeaders("X-Key", "X-Secret")

	header, err := auth(context.Background(), "")
	require.NoError(t, err)
	assert.Empty(t, header)

	header, err = auth(context.Background(), "abc:def:ghi")
	require.NoError(t, err)
	assert.Equal(t, "abc", header.Get("X-Key"))
	assert.Equal(t, "def:ghi", header.Get("X-Secret"))

	_, err = auth(context.Background(), "abc")
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
}

func TestServerReference(t *testing.T) {
	s := newShopServer(t, shopConfig(t))
	html, err := s.Reference()
	require.NoError(t, err)
	page := string(html)
	assert.Contains(t, page, "MCP reference")
	assert.Contains(t, page, `id="tool-call_function"`)
	for _, f := range s.Catalog().Functions() {
		assert.Contains(t, page, f.Name)
	}
}

// TestServerRunScriptSlotBeforeWhoAmI: a run takes its slot before the
// WhoAmI call, so callers cannot make identity requests while every slot is
// busy.
func TestServerRunScriptSlotBeforeWhoAmI(t *testing.T) {
	shop := shopApp()
	var whoami atomic.Int32
	entered := make(chan struct{})
	unblock := make(chan struct{})
	cfg := shopConfig(t)
	cfg.Script.Enabled = true
	cfg.Script.MaxConcurrent = 1
	cfg.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/me" {
			whoami.Add(1)
		}
		if r.URL.Path == "/items" {
			close(entered)
			select {
			case <-unblock:
			case <-r.Context().Done():
			}
		}
		shop.ServeHTTP(w, r)
	})
	s := newShopServer(t, cfg)
	first := connect(t, s, "first:x")
	second := connect(t, s, "second:x")

	done := make(chan *mcp.CallToolResult)
	go func() {
		res, err := first.CallTool(context.Background(), &mcp.CallToolParams{Name: "run_script", Arguments: map[string]any{"code": `return api.items.list_items({limit: 1}).length`}})
		assert.NoError(t, err)
		done <- res
	}()
	<-entered

	text, failed := callTool(t, second, "run_script", map[string]any{"code": "return 1"})
	assert.True(t, failed)
	assert.Equal(t, "error "+ez.ERESOURCEEXHAUSTED+": too many scripts are running; try again shortly", text)
	assert.Equal(t, int32(1), whoami.Load())

	close(unblock)
	res := <-done
	require.NotNil(t, res)
	require.Len(t, res.Content, 1)
	assert.False(t, res.IsError)
	assert.Equal(t, "1", res.Content[0].(*mcp.TextContent).Text)
	assert.Equal(t, int32(1), whoami.Load())
}

// TestServerScriptKey: callers are keyed by a hash of the bearer token, or
// of the WhoAmI response, in separate key spaces.
func TestServerScriptKey(t *testing.T) {
	token := scriptKey("key:secret", []byte(`{"id":1}`))
	assert.True(t, strings.HasPrefix(token, "token:"), token)
	assert.NotContains(t, token, "secret")
	assert.Equal(t, token, scriptKey("key:secret", []byte(`{"id":2}`)))

	me := scriptKey("", []byte(`{"id":1}`))
	assert.True(t, strings.HasPrefix(me, "whoami:"), me)
	assert.NotContains(t, me, "id")
	assert.NotEqual(t, me, scriptKey("", []byte(`{"id":2}`)))

	// A token equal to a WhoAmI response does not share its key.
	assert.NotEqual(t, scriptKey(`{"id":1}`, nil), me)
}
