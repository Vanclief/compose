package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vanclief/ez"
)

// bigInt is 2^53 + 1, the first integer a float64 cannot hold: decoded as a
// float64 it becomes 9007199254740992.
const bigInt = "9007199254740993"

// numbersSpec declares an operation that takes an integer in every place an
// argument can go.
const numbersSpec = `{"openapi": "3.1.0", "info": {"title": "Numbers", "version": "1"}, "paths": {
	"/numbers/{id}": {"post": {"operationId": "numbers.send", "parameters": [
		{"name": "id", "in": "path", "required": true, "schema": {"type": "integer"}},
		{"name": "limit", "in": "query", "schema": {"type": "integer"}},
		{"name": "ids", "in": "query", "schema": {"type": "array", "items": {"type": "integer"}}},
		{"name": "pipes", "in": "query", "style": "pipeDelimited", "schema": {"type": "array", "items": {"type": "integer"}}},
		{"name": "filter", "in": "query", "schema": {"type": "object", "properties": {"n": {"type": "integer"}}}},
		{"name": "X-Count", "in": "header", "schema": {"type": "integer"}}
	], "requestBody": {"content": {"application/json": {"schema": {"type": "object", "properties": {
		"count": {"type": "integer"},
		"nested": {"type": "object", "properties": {"count": {"type": "integer"}}}
	}}}}}, "responses": {"200": {"description": "OK"}}}}
}}`

type exactInput struct {
	ID    int64  `json:"id"`
	Size  uint64 `json:"size"`
	Count int    `json:"count"`
}

// numbersServer serves numbersSpec with the echoing dispatch app, the
// operation and a composite that returns its typed input promoted to tools.
func numbersServer(t *testing.T) *Server {
	t.Helper()
	exact := NewComposite("numbers", "exact", func(ctx context.Context, call Caller, in exactInput) (exactInput, error) {
		return in, nil
	})
	exact.Summary = "Return the input"
	srv, err := New(Config{
		Spec:       []byte(numbersSpec),
		Handler:    dispatchApp(),
		Composites: []Composite{exact},
		Tools:      []string{"numbers.send", "numbers.exact"},
	})
	require.NoError(t, err)
	return srv
}

// bigArgs puts bigInt in the path, a query argument, an item of an exploded
// and of a pipe-delimited query array, a property of a query object, a
// header, a body property and a nested body property.
func bigArgs() map[string]any {
	big := json.Number(bigInt)
	return map[string]any{
		"id":      big,
		"limit":   big,
		"ids":     []any{json.Number("1"), big},
		"pipes":   []any{big, json.Number("1")},
		"filter":  map[string]any{"n": big},
		"X-Count": big,
		"count":   big,
		"nested":  map[string]any{"count": big},
	}
}

// assertBigEcho checks that the request the app received holds bigInt with
// its own digits in every place bigArgs put it.
func assertBigEcho(t *testing.T, text string) {
	t.Helper()
	var got dispatchEcho
	require.NoError(t, json.Unmarshal([]byte(text), &got), text)
	assert.Equal(t, "/numbers/"+bigInt, got.Path)
	assert.Equal(t, "filter=%7B%22n%22%3A"+bigInt+"%7D&ids=1&ids="+bigInt+"&limit="+bigInt+"&pipes="+bigInt+"%7C1", got.Query)
	assert.Equal(t, bigInt, got.Headers["X-Count"])
	assert.Equal(t, `{"count":`+bigInt+`,"nested":{"count":`+bigInt+`}}`, got.Body)
}

func TestCallFunctionKeepsBigIntegers(t *testing.T) {
	srv := numbersServer(t)
	session := connect(t, srv, "")
	ctx := context.Background()

	// call_function over the transport.
	text, failed := callTool(t, session, "call_function", map[string]any{"name": "numbers.send", "args": bigArgs()})
	require.False(t, failed, text)
	assertBigEcho(t, text)

	// call_function through Server.CallTool.
	text, failed, err := srv.CallTool(ctx, nil, "call_function", map[string]any{"name": "numbers.send", "args": bigArgs()})
	require.NoError(t, err)
	require.False(t, failed, text)
	assertBigEcho(t, text)

	// The promoted tool over the transport and through Server.CallTool.
	text, failed = callTool(t, session, "numbers-send", bigArgs())
	require.False(t, failed, text)
	assertBigEcho(t, text)
	text, failed, err = srv.CallTool(ctx, nil, "numbers-send", bigArgs())
	require.NoError(t, err)
	require.False(t, failed, text)
	assertBigEcho(t, text)
}

func TestCompositeKeepsBigIntegers(t *testing.T) {
	srv := numbersServer(t)
	session := connect(t, srv, "")
	args := map[string]any{"id": json.Number("-" + bigInt), "size": json.Number("18446744073709551615")}
	want := `{"id":-` + bigInt + `,"size":18446744073709551615,"count":0}`

	text, failed := callTool(t, session, "call_function", map[string]any{"name": "numbers.exact", "args": args})
	require.False(t, failed, text)
	assert.Equal(t, want, text)

	text, failed = callTool(t, session, "numbers-exact", args)
	require.False(t, failed, text)
	assert.Equal(t, want, text)

	text, failed, err := srv.CallTool(context.Background(), nil, "numbers-exact", args)
	require.NoError(t, err)
	require.False(t, failed, text)
	assert.Equal(t, want, text)
}

// TestCallFunctionKeepsIntegerDigits: an integer written without a fraction
// or an exponent whose magnitude is 2^53 or more reaches the request with
// its digits, negative, past 1e21 or far past 2^64 alike.
func TestCallFunctionKeepsIntegerDigits(t *testing.T) {
	srv := numbersServer(t)
	session := connect(t, srv, "")

	for _, digits := range []string{"9007199254740992", "-" + bigInt, "18446744073709551615", "100000000000000000000000", "123456789012345678901234567890"} {
		args := map[string]any{"id": json.Number(digits), "limit": json.Number(digits), "count": json.Number(digits)}
		for _, tool := range []string{"call_function", "numbers-send"} {
			var text string
			var failed bool
			if tool == "call_function" {
				text, failed = callTool(t, session, tool, map[string]any{"name": "numbers.send", "args": args})
			} else {
				text, failed = callTool(t, session, tool, args)
			}
			require.False(t, failed, text)
			var got dispatchEcho
			require.NoError(t, json.Unmarshal([]byte(text), &got), text)
			assert.Equal(t, "/numbers/"+digits, got.Path, tool)
			assert.Equal(t, "limit="+digits, got.Query, tool)
			assert.Equal(t, `{"count":`+digits+`}`, got.Body, tool)
		}
	}
}

// TestCallFunctionOtherNumbersAsBefore: every number other than an integer
// written without a fraction or an exponent whose magnitude is 2^53 or more
// is sent as a float64, as it was before integers kept their digits.
func TestCallFunctionOtherNumbersAsBefore(t *testing.T) {
	srv := numbersServer(t)
	session := connect(t, srv, "")
	ctx := context.Background()

	args := map[string]any{
		"id":      json.Number("1.0"),
		"limit":   json.Number("1e3"),
		"X-Count": json.Number("-0"),
		"pipes":   []any{json.Number("-0"), json.Number("9007199254740991")},
		"count":   json.Number("2.50"),
		// Not an integer literal, so it is rounded as before.
		"nested": map[string]any{"count": json.Number(bigInt + ".0")},
	}
	for _, tool := range []string{"call_function", "numbers-send"} {
		var text string
		var failed bool
		if tool == "call_function" {
			text, failed = callTool(t, session, tool, map[string]any{"name": "numbers.send", "args": args})
		} else {
			text, failed = callTool(t, session, tool, args)
		}
		require.False(t, failed, text)
		var got dispatchEcho
		require.NoError(t, json.Unmarshal([]byte(text), &got), text)
		assert.Equal(t, "/numbers/1", got.Path, tool)
		assert.Equal(t, "limit=1000&pipes=-0%7C9007199254740991", got.Query, tool)
		assert.Equal(t, "-0", got.Headers["X-Count"], tool)
		assert.Equal(t, `{"count":2.5,"nested":{"count":9007199254740992}}`, got.Body, tool)
	}

	// A composite's int field given 1.0 decodes.
	text, failed := callTool(t, session, "numbers-exact", map[string]any{"count": json.Number("1.0")})
	require.False(t, failed, text)
	assert.Equal(t, `{"id":0,"size":0,"count":1}`, text)

	// A number no float64 holds is refused. Over HTTP the SDK refuses the
	// request of a client on MCP protocol 2026-07-28 or later, which the Go
	// client negotiates, as it cannot read the tool name past it. Through
	// Server.CallTool, call_function refuses it in the words of encoding/json
	// and a promoted tool as arguments that are not a JSON object.
	_, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "numbers-send", Arguments: json.RawMessage(`{"id": 1e400}`)})
	require.Error(t, err)

	huge := map[string]any{"id": json.Number("1e400")}
	text, failed, err = srv.CallTool(ctx, nil, "call_function", map[string]any{"name": "numbers.send", "args": huge})
	require.NoError(t, err)
	assert.True(t, failed)
	assert.Equal(t, `validating "arguments": unmarshaling arguments: json: cannot unmarshal number 1e400 into Go value of type float64`, text)

	text, failed, err = srv.CallTool(ctx, nil, "numbers-send", huge)
	require.NoError(t, err)
	assert.True(t, failed)
	assert.Equal(t, "error invalid: arguments of numbers.send must be a JSON object\n\n"+srv.Catalog().renderDescribe([]string{"numbers.send"}), text)
}

// TestApplyInputSchemaMatchesSDK: applyInputSchema gives the answers of the
// SDK's typed tools, except that it keeps the digits of an integer of
// magnitude 2^53 or more and words two refusals as encoding/json does:
// arguments that are not a JSON object, and a number no float64 holds,
// whose refusal no longer names the argument.
func TestApplyInputSchemaMatchesSDK(t *testing.T) {
	schema := &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"count": {Type: "integer", Minimum: jsonschema.Ptr(1.0)},
			"ratio": {Type: "number", Maximum: jsonschema.Ptr(10.0)},
			"size":  {Enum: []any{1.0, 2.0}},
			"name":  {Type: "string"},
			"page":  {Type: "integer", Default: json.RawMessage("1")},
		},
		AdditionalProperties: &jsonschema.Schema{Not: &jsonschema.Schema{}},
	}
	resolved, err := schema.Resolve(&jsonschema.ResolveOptions{ValidateDefaults: true})
	require.NoError(t, err)

	sdk := mcp.NewServer(&mcp.Implementation{Name: "sdk"}, nil)
	mcp.AddTool(sdk, &mcp.Tool{Name: "typed", InputSchema: schema}, func(ctx context.Context, req *mcp.CallToolRequest, in map[string]any) (*mcp.CallToolResult, any, error) {
		data, err := json.Marshal(in)
		if err != nil {
			return nil, nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(data)}}}, nil, nil
	})
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	_, err = sdk.Connect(context.Background(), serverTransport, nil)
	require.NoError(t, err)
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(context.Background(), clientTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		session.Close()
	})

	cases := []struct {
		args string
		// exact is what applyInputSchema returns where it differs from the
		// SDK's input.
		exact string
		// sdk and ours are set where both refuse the arguments in other
		// words: the SDK's decoder is not encoding/json.
		sdk  string
		ours string
	}{
		{args: `{"count": 3, "ratio": 1.5, "size": 2, "name": "a"}`},
		{args: `{}`},
		{args: `{"count": 3.0, "size": 2.0}`},
		{args: `{"count": ` + bigInt + `}`, exact: `{"count":` + bigInt + `,"page":1}`},
		{args: `{"count": 1.5}`},
		{args: `{"count": 0}`},
		{args: `{"ratio": 11}`},
		{args: `{"size": 3}`},
		{args: `{"name": 5}`},
		{args: `{"name": "a", "extra": 1}`},
		{
			args: `{"ratio": 1e400}`,
			sdk:  `json: cannot unmarshal "1e400" into Go struct field map[string]interface {}.ratio of type float64`,
			ours: `json: cannot unmarshal number 1e400 into Go value of type float64`,
		},
		{args: `[]`, sdk: `json: cannot unmarshal "[]" into Go value of type map[string]interface {}`, ours: `json: cannot unmarshal array into Go value of type map[string]interface {}`},
		{args: `"x"`, sdk: `json: cannot unmarshal "\"x\"" into Go value of type map[string]interface {}`, ours: `json: cannot unmarshal string into Go value of type map[string]interface {}`},
		{args: `5`, sdk: `json: cannot unmarshal "5" into Go value of type map[string]interface {}`, ours: `json: cannot unmarshal number into Go value of type map[string]interface {}`},
		{args: `true`, sdk: `json: cannot unmarshal "true" into Go value of type map[string]interface {}`, ours: `json: cannot unmarshal bool into Go value of type map[string]interface {}`},
	}
	for _, tc := range cases {
		res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "typed", Arguments: json.RawMessage(tc.args)})
		require.NoError(t, err, tc.args)
		require.Len(t, res.Content, 1)
		want := res.Content[0].(*mcp.TextContent).Text

		input, invalid := applyInputSchema(json.RawMessage(tc.args), resolved)
		if tc.sdk != "" {
			assert.True(t, res.IsError, tc.args)
			assert.Equal(t, `validating "arguments": unmarshaling arguments: `+tc.sdk, want, tc.args)
			assert.Equal(t, `validating "arguments": unmarshaling arguments: `+tc.ours, invalid, tc.args)
			continue
		}
		if res.IsError {
			assert.Equal(t, want, invalid, tc.args)
			continue
		}
		require.Empty(t, invalid, tc.args)
		if tc.exact == "" {
			assert.Equal(t, want, string(input), tc.args)
			continue
		}
		assert.Equal(t, tc.exact, string(input), tc.args)
	}
}

// TestApplyInputSchemaIntegerInput: a typed input with an int field takes
// 1.0 and 1e3, as the SDK's typed tools take them.
func TestApplyInputSchemaIntegerInput(t *testing.T) {
	type integerInput struct {
		N int `json:"n"`
		M int `json:"m"`
	}
	schema, err := jsonschema.For[integerInput](nil)
	require.NoError(t, err)
	resolved, err := schema.Resolve(&jsonschema.ResolveOptions{ValidateDefaults: true})
	require.NoError(t, err)

	input, invalid := applyInputSchema(json.RawMessage(`{"n": 1.0, "m": 1e3}`), resolved)
	require.Empty(t, invalid)
	var in integerInput
	require.NoError(t, unmarshalExact(input, &in))
	assert.Equal(t, integerInput{N: 1, M: 1000}, in)
}

// The SDK's typed tools panic on these arguments, so there is nothing to
// compare with.
func TestApplyInputSchemaNullArguments(t *testing.T) {
	schema := &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"name": {Type: "string"},
			"page": {Type: "integer", Default: json.RawMessage("1")},
		},
	}
	resolved, err := schema.Resolve(&jsonschema.ResolveOptions{ValidateDefaults: true})
	require.NoError(t, err)

	input, invalid := applyInputSchema(json.RawMessage(`null`), resolved)
	require.Empty(t, invalid)
	assert.JSONEq(t, `{"page": 1}`, string(input))

	schema.Required = []string{"name"}
	resolved, err = schema.Resolve(&jsonschema.ResolveOptions{ValidateDefaults: true})
	require.NoError(t, err)
	_, invalid = applyInputSchema(json.RawMessage(`null`), resolved)
	_, want := applyInputSchema(json.RawMessage(`{}`), resolved)
	require.NotEmpty(t, want)
	assert.Equal(t, want, invalid)
}

// countingHandler counts the requests that reach it.
type countingHandler struct {
	n atomic.Int64
}

func (h *countingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.n.Add(1)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte("{}"))
}

func TestCallRefusesEmptyPathArgument(t *testing.T) {
	handler := &countingHandler{}
	srv, err := New(Config{Spec: []byte(numbersSpec), Handler: handler})
	require.NoError(t, err)

	empty := map[string]any{
		"empty string":          "",
		"empty array":           []any{},
		"array of nulls":        []any{nil, nil},
		"array of empty string": []any{""},
		"empty Go slice":        []string{},
	}
	for name, value := range empty {
		_, err := srv.Call(context.Background(), nil, "numbers.send", map[string]any{"id": value})
		assert.Equal(t, ez.EINVALID, ez.ErrorCode(err), name)
		assert.Equal(t, "mcp: numbers.send requires a non-empty path argument id", ez.ErrorMessage(err), name)
	}
	assert.Zero(t, handler.n.Load(), "no request reaches the handler")

	_, err = srv.Call(context.Background(), nil, "numbers.send", map[string]any{"id": 0})
	require.NoError(t, err)
	assert.Equal(t, int64(1), handler.n.Load())
}

func TestCallRefusesDotPathSegment(t *testing.T) {
	handler := &countingHandler{}
	srv, err := New(Config{Spec: []byte(numbersSpec), Handler: handler})
	require.NoError(t, err)

	dots := map[string]any{
		"dot":         ".",
		"two dots":    "..",
		"array of ..": []any{".."},
	}
	for name, value := range dots {
		_, err := srv.Call(context.Background(), nil, "numbers.send", map[string]any{"id": value})
		assert.Equal(t, ez.EINVALID, ez.ErrorCode(err), name)
		assert.Equal(t, "mcp: numbers.send path arguments must not make a path segment of . or ..", ez.ErrorMessage(err), name)
	}
	assert.Zero(t, handler.n.Load(), "no request reaches the handler")

	for _, value := range []any{"...", ".a", "a..", "a.b", "../a", []any{".", "."}} {
		_, err = srv.Call(context.Background(), nil, "numbers.send", map[string]any{"id": value})
		require.NoError(t, err, value)
	}
	assert.Equal(t, int64(6), handler.n.Load())
}

func TestToolsRefuseEmptyPathArgument(t *testing.T) {
	cfg := promotedConfig(t)
	cfg.Script.Enabled = true
	s := newShopServer(t, cfg)
	session := connect(t, s, "key:secret")
	describeGetOrder := s.Catalog().renderDescribe([]string{"orders.get_order"})
	describeAddLine := s.Catalog().renderDescribe([]string{"orders.add_line"})

	text, failed := callTool(t, session, "call_function", map[string]any{"name": "orders.get_order", "args": map[string]any{"order_id": ""}})
	assert.True(t, failed)
	assert.Equal(t, "error invalid: orders.get_order requires a non-empty path argument order_id\n\n"+describeGetOrder, text)

	text, failed, err := s.CallTool(context.Background(), shopHeader(), "call_function", map[string]any{"name": "orders.get_order", "args": map[string]any{"order_id": []any{}}})
	require.NoError(t, err)
	assert.True(t, failed)
	assert.Equal(t, "error invalid: orders.get_order requires a non-empty path argument order_id\n\n"+describeGetOrder, text)

	text, failed = callTool(t, session, "orders-add_line", map[string]any{"order_id": "", "sku": "A1", "quantity": 1})
	assert.True(t, failed)
	assert.Equal(t, "error invalid: orders.add_line requires a non-empty path argument order_id\n\n"+describeAddLine, text)

	text, failed = callTool(t, session, "run_script", map[string]any{"code": `try { api.orders.get_order({order_id: ""}) } catch (e) { return e.code + " " + e.message }`})
	assert.False(t, failed, text)
	assert.Equal(t, `"invalid orders.get_order: invalid: orders.get_order requires a non-empty path argument order_id"`, text)
}

func TestToolsRefuseDotPathSegment(t *testing.T) {
	cfg := promotedConfig(t)
	cfg.Script.Enabled = true
	s := newShopServer(t, cfg)
	session := connect(t, s, "key:secret")
	describeGetOrder := s.Catalog().renderDescribe([]string{"orders.get_order"})
	describeAddLine := s.Catalog().renderDescribe([]string{"orders.add_line"})

	text, failed := callTool(t, session, "call_function", map[string]any{"name": "orders.get_order", "args": map[string]any{"order_id": ".."}})
	assert.True(t, failed)
	assert.Equal(t, "error invalid: orders.get_order path arguments must not make a path segment of . or ..\n\n"+describeGetOrder, text)

	text, failed = callTool(t, session, "orders-add_line", map[string]any{"order_id": "..", "sku": "A1", "quantity": 1})
	assert.True(t, failed)
	assert.Equal(t, "error invalid: orders.add_line path arguments must not make a path segment of . or ..\n\n"+describeAddLine, text)

	text, failed = callTool(t, session, "run_script", map[string]any{"code": `try { api.orders.get_order({order_id: "."}) } catch (e) { return e.code + " " + e.message }`})
	assert.False(t, failed, text)
	assert.Equal(t, `"invalid orders.get_order: invalid: orders.get_order path arguments must not make a path segment of . or .."`, text)
}
