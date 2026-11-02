package mcp

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vanclief/ez"
)

// shopHeader holds the REST headers BearerToHeaders makes of "key:secret".
func shopHeader() http.Header {
	return http.Header{"Api-Key": {"key"}, "Api-Secret": {"secret"}}
}

func toolList(tools []Tool) []string {
	var names []string
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	return names
}

func TestServerToolsList(t *testing.T) {
	fixed := []string{"whoami", "search_functions", "describe_function", "explain_term", "call_function"}

	s := newShopServer(t, shopConfig(t))
	assert.Equal(t, fixed, toolList(s.Tools()))

	cfg := shopConfig(t)
	cfg.Script.Enabled = true
	assert.Equal(t, append(fixed, "run_script"), toolList(newShopServer(t, cfg).Tools()))

	cfg = promotedConfig(t)
	cfg.Script.Enabled = true
	promoted := newShopServer(t, cfg)
	tools := promoted.Tools()
	assert.Equal(t, append(fixed, "run_script", "orders-add_line", "orders-summary", "orders-cancel_order"), toolList(tools))

	// Names, descriptions and schemas are those of tools/list.
	listed := listedTools(t, connect(t, promoted, "key:secret"))
	for _, tool := range tools {
		want := listed[tool.Name]
		require.NotNil(t, want, tool.Name)
		assert.Equal(t, want.Description, tool.Description, tool.Name)
		assert.JSONEq(t, schemaJSON(t, want.InputSchema), string(tool.InputSchema), tool.Name)
	}
	assert.JSONEq(t, `{
		"type": "object",
		"properties": {
			"order_id": {"type": "integer"},
			"sku": {"type": "string", "description": "Stock keeping unit"},
			"quantity": {"type": "integer"},
			"note": {"type": "string"}
		},
		"required": ["order_id", "sku", "quantity"],
		"additionalProperties": false
	}`, string(tools[6].InputSchema))

	byName := map[string]Tool{}
	for _, tool := range tools {
		byName[tool.Name] = tool
	}
	hints := func(tool Tool) [4]bool {
		return [4]bool{tool.ReadOnly, tool.Idempotent, tool.Destructive, tool.OpenWorld}
	}
	assert.Equal(t, [4]bool{true, true, false, false}, hints(byName["whoami"]))
	assert.Equal(t, [4]bool{true, true, false, false}, hints(byName["search_functions"]))
	assert.Equal(t, [4]bool{false, false, true, false}, hints(byName["call_function"]))
	assert.Equal(t, [4]bool{true, false, false, false}, hints(byName["run_script"]))
	// destructiveHint is unset on a write that is not Function.Destructive,
	// which MCP reads as true.
	assert.Equal(t, [4]bool{false, false, true, false}, hints(byName["orders-add_line"]))
	assert.Equal(t, [4]bool{true, true, false, false}, hints(byName["orders-summary"]))
	assert.Equal(t, [4]bool{false, false, true, false}, hints(byName["orders-cancel_order"]))

	// A run_script that may write is destructive, as MCP reads its unset
	// destructiveHint.
	cfg = shopConfig(t)
	cfg.Script.Enabled = true
	cfg.Script.AllowWrites = true
	writes := newShopServer(t, cfg).Tools()
	assert.Equal(t, "run_script", writes[5].Name)
	assert.Equal(t, [4]bool{false, false, true, false}, hints(writes[5]))

	// The slice and its schemas are copies.
	tools[0].Name = "changed"
	tools[6].InputSchema[0] = 'x'
	again := promoted.Tools()
	assert.Equal(t, "whoami", again[0].Name)
	assert.True(t, json.Valid(again[6].InputSchema))
}

func TestServerCallTool(t *testing.T) {
	s := newShopServer(t, promotedConfig(t))
	ctx := context.Background()
	header := shopHeader()
	describeAddLine := s.Catalog().renderDescribe([]string{"orders.add_line"})

	text, failed, err := s.CallTool(ctx, header, "search_functions", map[string]any{"query": "order"})
	require.NoError(t, err)
	assert.False(t, failed)
	assert.Equal(t, s.Catalog().renderSearch("", "order"), text)

	text, failed, err = s.CallTool(ctx, header, "describe_function", map[string]any{"names": []string{"orders.add_line"}})
	require.NoError(t, err)
	assert.False(t, failed)
	assert.Equal(t, describeAddLine, text)

	text, failed, err = s.CallTool(ctx, header, "explain_term", map[string]any{"terms": []string{"pedido"}})
	require.NoError(t, err)
	assert.False(t, failed)
	assert.Equal(t, s.Catalog().renderExplain([]string{"pedido"}), text)

	// The headers reach the REST handler as given.
	text, failed, err = s.CallTool(ctx, header, "whoami", nil)
	require.NoError(t, err)
	assert.False(t, failed)
	assert.JSONEq(t, `{"key":"key"}`, text)

	text, failed, err = s.CallTool(ctx, header, "call_function", map[string]any{"name": "orders.get_order", "args": map[string]any{"order_id": 7}})
	require.NoError(t, err)
	require.False(t, failed, text)
	var got shopEcho
	require.NoError(t, json.Unmarshal([]byte(text), &got), text)
	assert.Equal(t, 7, got.ID)
	assert.Equal(t, "key", got.Key)
	assert.Equal(t, "secret", got.Secret)

	text, failed, err = s.CallTool(ctx, header, "call_function", map[string]any{"name": "orders.add_line", "args": map[string]any{"order_id": 7, "quantity": 1}})
	require.NoError(t, err)
	assert.True(t, failed)
	assert.Equal(t, "error invalid: sku is required\n\n"+describeAddLine, text)

	text, failed, err = s.CallTool(ctx, header, "call_function", map[string]any{"name": "orders.nope"})
	require.NoError(t, err)
	assert.True(t, failed)
	assert.Equal(t, "error not_found: unknown function orders.nope; use search_functions", text)

	text, failed, err = s.CallTool(ctx, header, "orders-summary", map[string]any{"order_id": 7})
	require.NoError(t, err)
	assert.False(t, failed, text)
	assert.JSONEq(t, `{"order_id":7,"key":"key","items":2}`, text)

	text, failed, err = s.CallTool(ctx, header, "orders-add_line", map[string]any{"order_id": 7, "quantity": 1})
	require.NoError(t, err)
	assert.True(t, failed)
	assert.Equal(t, "error invalid: sku is required\n\n"+describeAddLine, text)

	// Without headers the REST API sees no credentials.
	text, failed, err = s.CallTool(ctx, nil, "whoami", nil)
	require.NoError(t, err)
	assert.False(t, failed)
	assert.JSONEq(t, `{"key":""}`, text)

	// A schema violation is a tool error, as the transport answers it.
	text, failed, err = s.CallTool(ctx, header, "describe_function", map[string]any{"names": "orders.add_line"})
	require.NoError(t, err)
	assert.True(t, failed)
	assert.True(t, strings.HasPrefix(text, `validating "arguments": `), text)

	text, failed, err = s.CallTool(ctx, header, "call_function", map[string]any{"name": "items.list_items", "args": map[string]any{"limit": 500}})
	require.NoError(t, err)
	assert.False(t, failed)
	assert.Contains(t, text, "\n\n[output cut at 24000 bytes of ")
	assert.Less(t, len(text), 24300)
}

func TestServerCallToolErrors(t *testing.T) {
	s := newShopServer(t, promotedConfig(t))
	ctx := context.Background()

	_, _, err := s.CallTool(ctx, shopHeader(), "orders.summary", nil)
	assert.Equal(t, ez.ENOTFOUND, ez.ErrorCode(err))
	assert.Equal(t, "mcp: unknown tool orders.summary; Server.Tools lists the tools", ez.ErrorMessage(err))

	_, _, err = s.CallTool(ctx, shopHeader(), "run_script", map[string]any{"code": "return 1"})
	assert.Equal(t, ez.ENOTFOUND, ez.ErrorCode(err))

	_, _, err = s.CallTool(ctx, shopHeader(), "describe_function", map[string]any{"names": []float64{math.NaN()}})
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	assert.Contains(t, ez.ErrorMessage(err), "arguments of describe_function cannot be encoded as JSON")

	_, _, err = s.CallTool(ctx, shopHeader(), "orders-summary", map[string]any{"order_id": func() {}})
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
}

func TestServerCallToolWhoAmIWithoutFunction(t *testing.T) {
	cfg := shopConfig(t)
	cfg.WhoAmI = ""
	s := newShopServer(t, cfg)

	text, failed, err := s.CallTool(context.Background(), http.Header{"Authorization": {"Bearer key:secret"}}, "whoami", nil)
	require.NoError(t, err)
	assert.False(t, failed)
	assert.Equal(t, "server Shop 2.3.0\nbearer token: none", text)
}

func TestServerCallToolRecoversPanic(t *testing.T) {
	boom := NewComposite("orders", "boom", func(ctx context.Context, call Caller, in orderSummaryInput) (OrderSummary, error) {
		panic("kaboom")
	})
	boom.Summary = "Always panics"
	cfg := shopConfig(t)
	cfg.Composites = append(cfg.Composites, boom)
	cfg.Tools = []string{"orders.boom"}
	var reported []error
	cfg.OnError = func(ctx context.Context, err error) {
		reported = append(reported, err)
	}
	s := newShopServer(t, cfg)

	text, failed, err := s.CallTool(context.Background(), shopHeader(), "call_function", map[string]any{"name": "orders.boom", "args": map[string]any{"order_id": 7}})
	require.NoError(t, err)
	assert.True(t, failed)
	assert.Equal(t, "error internal: call_function panicked: kaboom", text)

	text, failed, err = s.CallTool(context.Background(), shopHeader(), "orders-boom", map[string]any{"order_id": 7})
	require.NoError(t, err)
	assert.True(t, failed)
	assert.Equal(t, "error internal: orders-boom panicked: kaboom", text)

	require.Len(t, reported, 2)
	assert.Equal(t, ez.EINTERNAL, ez.ErrorCode(reported[0]))
	assert.Contains(t, ez.ErrorMessage(reported[0]), "call_function panicked: kaboom\n")
}

// TestServerCallToolMatchesTransport: CallTool returns the text and flag a
// client gets for the same call over the MCP transport.
func TestServerCallToolMatchesTransport(t *testing.T) {
	s := newShopServer(t, promotedConfig(t))
	session := connect(t, s, "key:secret")

	calls := []struct {
		name string
		args map[string]any
	}{
		{"whoami", nil},
		{"search_functions", nil},
		{"search_functions", map[string]any{"module": "orders"}},
		{"describe_function", map[string]any{"names": []string{"orders.add_line", "orders.nope"}}},
		{"explain_term", map[string]any{"terms": []string{"Order"}}},
		{"call_function", map[string]any{"name": "orders.get_order", "args": map[string]any{"order_id": 7, "expand": []string{"lines"}}}},
		{"call_function", map[string]any{"name": "orders.add_line", "args": map[string]any{"order_id": 7}}},
		{"call_function", map[string]any{"name": "orders.nope"}},
		{"call_function", map[string]any{"name": "items.list_items", "args": map[string]any{"limit": 500}}},
		{"orders-add_line", map[string]any{"order_id": 7, "sku": "A1", "quantity": 2}},
		{"orders-add_line", nil},
		{"orders-cancel_order", map[string]any{"order_id": 7}},
		{"describe_function", map[string]any{"names": "orders.add_line"}},
		{"describe_function", map[string]any{"names": []string{}}},
		{"describe_function", map[string]any{"names": []string{"orders.add_line"}, "extra": 1}},
		{"call_function", map[string]any{"args": map[string]any{}}},
	}
	for _, c := range calls {
		var args any
		if c.args != nil {
			args = c.args
		}
		want, wantFailed := callTool(t, session, c.name, args)
		text, failed, err := s.CallTool(context.Background(), shopHeader(), c.name, c.args)
		require.NoError(t, err, c.name)
		assert.Equal(t, want, text, "%s %v", c.name, c.args)
		assert.Equal(t, wantFailed, failed, "%s %v", c.name, c.args)
	}
}
