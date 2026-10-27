package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vanclief/ez"

	"github.com/vanclief/compose/components/rest/reference"
)

// promotedConfig is the shop config with an operation, a destructive
// operation and a composite promoted.
func promotedConfig(t *testing.T) Config {
	t.Helper()
	cfg := shopConfig(t)
	cfg.Tools = []string{"orders.add_line", "orders.summary", "orders.cancel_order"}
	return cfg
}

// listedTools returns the tools/list entries by name.
func listedTools(t *testing.T, session *mcp.ClientSession) map[string]*mcp.Tool {
	t.Helper()
	res, err := session.ListTools(context.Background(), nil)
	require.NoError(t, err)
	tools := map[string]*mcp.Tool{}
	for _, tool := range res.Tools {
		tools[tool.Name] = tool
	}
	return tools
}

// schemaJSON encodes a schema as the client received it.
func schemaJSON(t *testing.T, schema any) string {
	t.Helper()
	data, err := json.Marshal(schema)
	require.NoError(t, err)
	return string(data)
}

func TestServerPromotedToolsList(t *testing.T) {
	s := newShopServer(t, promotedConfig(t))
	session := connect(t, s, "key:secret")

	assert.Equal(t, []string{
		"call_function", "describe_function", "explain_term",
		"orders-add_line", "orders-cancel_order", "orders-summary",
		"search_functions", "whoami",
	}, toolNames(t, session))
	tools := listedTools(t, session)

	addLine := tools["orders-add_line"]
	require.NotNil(t, addLine)
	assert.Equal(t, "Add a line to an order\nReturns Order", addLine.Description)
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
	}`, schemaJSON(t, addLine.InputSchema))
	// destructiveHint is left unset, the MCP default, for a write that is not
	// a DELETE: false would claim only additive updates.
	assert.Equal(t, &mcp.ToolAnnotations{OpenWorldHint: boolPtr(false)}, addLine.Annotations)

	cancel := tools["orders-cancel_order"]
	require.NotNil(t, cancel)
	assert.Equal(t, "Cancel an order", cancel.Description)
	assert.Equal(t, &mcp.ToolAnnotations{DestructiveHint: boolPtr(true), OpenWorldHint: boolPtr(false)}, cancel.Annotations)

	summary := tools["orders-summary"]
	require.NotNil(t, summary)
	assert.Equal(t, "Summarize an order\nReads the order and counts two catalog items.\nReturns OrderSummary", summary.Description)
	assert.JSONEq(t, `{
		"type": "object",
		"properties": {"order_id": {"type": "integer", "description": "Order to summarize"}},
		"required": ["order_id"],
		"additionalProperties": false
	}`, schemaJSON(t, summary.InputSchema))
	assert.Equal(t, &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: boolPtr(false)}, summary.Annotations)
}

func TestServerPromotedToolsCall(t *testing.T) {
	s := newShopServer(t, promotedConfig(t))
	session := connect(t, s, "key:secret")

	text, failed := callTool(t, session, "orders-add_line", map[string]any{"order_id": 7, "sku": "A1", "quantity": 2})
	require.False(t, failed, text)
	var got shopEcho
	require.NoError(t, json.Unmarshal([]byte(text), &got), text)
	assert.Equal(t, "POST", got.Method)
	assert.Equal(t, "secret", got.Secret)
	assert.Equal(t, map[string]string{"order_id": "7"}, got.Path)
	assert.Equal(t, map[string]any{"sku": "A1", "quantity": float64(2)}, got.Body)

	text, failed = callTool(t, session, "orders-summary", map[string]any{"order_id": 7})
	assert.False(t, failed, text)
	assert.JSONEq(t, `{"order_id":7,"key":"key","items":2}`, text)

	// Invalid arguments get the error line and the description, as
	// call_function gives them.
	describeAddLine := s.Catalog().renderDescribe([]string{"orders.add_line"})
	text, failed = callTool(t, session, "orders-add_line", map[string]any{"order_id": 7, "quantity": 1})
	assert.True(t, failed)
	assert.Equal(t, "error invalid: sku is required\n\n"+describeAddLine, text)

	text, failed = callTool(t, session, "orders-add_line", map[string]any{"order_id": 7, "sku": "A1", "color": "red"})
	assert.True(t, failed)
	assert.Equal(t, "error invalid: unknown arguments color for orders.add_line; valid: order_id, sku, quantity, note\n\n"+describeAddLine, text)

	text, failed = callTool(t, session, "orders-add_line", nil)
	assert.True(t, failed)
	assert.Equal(t, "error invalid: orders.add_line requires the path argument order_id\n\n"+describeAddLine, text)

	text, failed = callTool(t, session, "orders-cancel_order", map[string]any{"order_id": 7})
	assert.True(t, failed)
	assert.Equal(t, "error conflict: order already shipped", text)

	// The credentials of the request still apply.
	text, failed = callTool(t, connect(t, s, "nocolon"), "orders-summary", map[string]any{"order_id": 7})
	assert.True(t, failed)
	assert.Equal(t, "error invalid: bearer token must be key:secret", text)
}

func TestServerPromotedToolsNewErrors(t *testing.T) {
	names := map[string][]string{
		"unknown":   {"orders.nope"},
		"duplicate": {"orders.add_line", "orders.add_line"},
		// Fixed tool names are never catalog names, which are module.function.
		"fixed": {"call_function"},
	}
	for name, tools := range names {
		t.Run(name, func(t *testing.T) {
			cfg := shopConfig(t)
			cfg.Tools = tools
			_, err := New(cfg)
			assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
			assert.Contains(t, ez.ErrorMessage(err), tools[0])
		})
	}
}

func TestServerPromotedToolsInstructions(t *testing.T) {
	s := newShopServer(t, promotedConfig(t))
	assert.Contains(t, s.Instructions(), " These functions are also tools you can call directly: orders.add_line as orders-add_line, orders.summary as orders-summary, "+
		"orders.cancel_order as orders-cancel_order. "+
		"Everything else is reached through search_functions and call_function.")

	assert.NotContains(t, newShopServer(t, shopConfig(t)).Instructions(), "also tools")
}

func TestServerPromotedToolsSnapshot(t *testing.T) {
	cfg := promotedConfig(t)
	cfg.Script.Enabled = true
	s := newShopServer(t, cfg)

	snap, err := s.Snapshot()
	require.NoError(t, err)
	golden(t, "snapshot_promoted", string(snap))

	// The reference page shows the promoted tools with the fixed ones.
	html, err := s.Reference()
	require.NoError(t, err)
	assert.Contains(t, string(html), `id="tool-orders-add_line"`)
}

func TestServerPromotedToolsNameLimit(t *testing.T) {
	// The tool name is 64 characters with a 31-character module, one over
	// with a 32-character one.
	spec := func(module string) string {
		return `{"openapi": "3.1.0", "info": {"title": "Long", "version": "1"}, "paths": {
			"/things": {"get": {"tags": ["` + module + `"], "operationId": "` + strings.Repeat("f", 32) + `",
				"responses": {"200": {"description": "OK"}}}}
		}}`
	}
	cfg := Config{Spec: []byte(spec(strings.Repeat("m", 31))), Handler: http.NotFoundHandler()}
	cfg.Tools = []string{strings.Repeat("m", 31) + "." + strings.Repeat("f", 32)}
	_, err := New(cfg)
	require.NoError(t, err)

	cfg.Spec = []byte(spec(strings.Repeat("m", 32)))
	cfg.Tools = []string{strings.Repeat("m", 32) + "." + strings.Repeat("f", 32)}
	_, err = New(cfg)
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	assert.Contains(t, ez.ErrorMessage(err), `whose tool name "`+strings.Repeat("m", 32)+"-"+strings.Repeat("f", 32)+`" breaks claude.ai's rule`)
}

func TestCatalogToolDescriptionDeprecated(t *testing.T) {
	c := smallCatalog(t)
	f := mustFunction(t, c, "packages.update_package")
	assert.Equal(t, "Deprecated.\nUpdate a package", c.toolDescription(f))

	// The "METHOD /path" summary the catalog falls back to is left out, as
	// describe_function leaves it out.
	bare := &Function{Summary: "GET /things", Method: "GET", Path: "/things", Description: "Lists the things."}
	assert.Equal(t, "Lists the things.", c.toolDescription(bare))
}

func TestServerPromotedToolsPropertyName(t *testing.T) {
	spec := `{"openapi": "3.1.0", "info": {"title": "Filters", "version": "1"}, "paths": {
		"/things": {"get": {"operationId": "things.list",
			"parameters": [{"name": "filter[status]", "in": "query", "schema": {"type": "string"}}],
			"responses": {"200": {"description": "OK"}}}}
	}}`
	cfg := Config{Spec: []byte(spec), Handler: http.NotFoundHandler()}
	_, err := New(cfg)
	require.NoError(t, err)

	cfg.Tools = []string{"things.list"}
	_, err = New(cfg)
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	assert.Contains(t, ez.ErrorMessage(err), `whose argument "filter[status]" breaks the Claude API's rule`)
}

func TestFunctionInputSchemaKeywords(t *testing.T) {
	doc := parseSpec(t, `{"openapi": "3.1.0", "info": {"title": "Bounds", "version": "1"}, "paths": {
		"/things": {"get": {
			"operationId": "things.list",
			"parameters": [
				{"name": "limit", "in": "query", "schema": {"type": "integer", "minimum": 1, "maximum": 100, "default": 20}},
				{"name": "price", "in": "query", "schema": {"type": "number", "minimum": 0, "exclusiveMinimum": true, "exclusiveMaximum": 1000, "multipleOf": 0.5}},
				{"name": "code", "in": "query", "schema": {"type": "string", "pattern": "^[A-Z]{3}$", "minLength": 3, "maxLength": 3}},
				{"name": "tags", "in": "query", "schema": {"type": "array", "items": {"type": "string"}, "minItems": 1, "maxItems": 5, "uniqueItems": true}},
				{"name": "extra", "in": "query", "schema": {"type": "object", "minProperties": 1, "maxProperties": 3}}
			],
			"responses": {"200": {"description": "OK"}}
		}}
	}}`)
	c, err := buildCatalog(doc, nil, nil)
	require.NoError(t, err)
	schema, err := mustFunction(t, c, "things.list").InputSchema()
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"type": "object",
		"properties": {
			"limit": {"type": "integer", "minimum": 1, "maximum": 100, "default": 20},
			"price": {"type": "number", "exclusiveMinimum": 0, "exclusiveMaximum": 1000, "multipleOf": 0.5},
			"code": {"type": "string", "pattern": "^[A-Z]{3}$", "minLength": 3, "maxLength": 3},
			"tags": {"type": "array", "items": {"type": "string"}, "minItems": 1, "maxItems": 5, "uniqueItems": true},
			"extra": {"type": "object", "minProperties": 1, "maxProperties": 3}
		},
		"additionalProperties": false
	}`, schemaJSON(t, schema))
}

func TestServerCall(t *testing.T) {
	s := newShopServer(t, shopConfig(t))
	header := http.Header{"Api-Key": {"key"}, "Api-Secret": {"secret"}}

	raw, err := s.Call(context.Background(), header, "orders.get_order", map[string]any{"order_id": 7})
	require.NoError(t, err)
	var got shopEcho
	require.NoError(t, json.Unmarshal(raw, &got), string(raw))
	assert.Equal(t, "GET", got.Method)
	assert.Equal(t, "key", got.Key)
	assert.Equal(t, "secret", got.Secret)
	assert.Equal(t, 7, got.ID)

	raw, err = s.Call(context.Background(), header, "orders.summary", map[string]any{"order_id": 7})
	require.NoError(t, err)
	assert.JSONEq(t, `{"order_id":7,"key":"key","items":2}`, string(raw))

	_, err = s.Call(context.Background(), header, "orders.get_order", nil)
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	assert.Contains(t, ez.ErrorMessage(err), "requires the path argument order_id")

	_, err = s.Call(context.Background(), header, "orders.nope", nil)
	assert.Equal(t, ez.ENOTFOUND, ez.ErrorCode(err))
}

// schemaSpec has an operation with path, query, header and body arguments,
// a nullable enum, a nested ref, a recursive type and readOnly properties.
const schemaSpec = `{"openapi": "3.1.0", "info": {"title": "Schema", "version": "1"}, "paths": {
	"/notes/{id}": {"post": {
		"operationId": "notes.update",
		"summary": "Update a note",
		"parameters": [
			{"name": "id", "in": "path", "required": true, "description": "Note to update", "schema": {"type": "integer"}},
			{"name": "dry_run", "in": "query", "schema": {"type": "boolean"}},
			{"name": "X-Trace", "in": "header", "schema": {"type": "string"}}
		],
		"requestBody": {"required": true, "content": {"application/json": {"schema": {"$ref": "#/components/schemas/NoteInput"}}}},
		"responses": {"200": {"description": "OK"}}
	}}
}, "components": {"schemas": {
	"NoteInput": {"type": "object", "required": ["id", "text"], "properties": {
		"id": {"type": "integer", "readOnly": true},
		"text": {"type": "string", "description": "Body of the note"},
		"color": {"type": ["string", "null"], "enum": ["red", "blue"]},
		"author": {"$ref": "#/components/schemas/Person"},
		"tags": {"type": "array", "items": {"type": "string"}}
	}},
	"Person": {"type": "object", "required": ["name"], "additionalProperties": false, "properties": {
		"name": {"type": "string"},
		"created": {"type": "string", "readOnly": true},
		"manager": {"$ref": "#/components/schemas/Person"}
	}}
}}}`

func TestFunctionInputSchema(t *testing.T) {
	doc, err := reference.Parse([]byte(schemaSpec))
	require.NoError(t, err)
	c, err := buildCatalog(doc, nil, nil)
	require.NoError(t, err)

	schema, err := mustFunction(t, c, "notes.update").InputSchema()
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"type": "object",
		"properties": {
			"id": {"type": "integer", "description": "Note to update"},
			"dry_run": {"type": "boolean"},
			"X-Trace": {"type": "string"},
			"text": {"type": "string", "description": "Body of the note"},
			"color": {"type": ["string", "null"], "enum": ["red", "blue"]},
			"author": {
				"type": "object",
				"properties": {"name": {"type": "string"}, "manager": {"type": "object"}},
				"required": ["name"],
				"additionalProperties": false
			},
			"tags": {"type": "array", "items": {"type": "string"}}
		},
		"required": ["id", "text"],
		"additionalProperties": false
	}`, schemaJSON(t, schema))

	// Properties keep the argument order.
	data, err := json.Marshal(schema)
	require.NoError(t, err)
	assert.Regexp(t, `"id".*"dry_run".*"X-Trace".*"text".*"color".*"author".*"tags"`, string(data))

	// The enum is not widened: a nullable type with an enum that leaves
	// null out still refuses null, as the document does.
	resolved, err := schema.Resolve(nil)
	require.NoError(t, err)
	assert.NoError(t, resolved.Validate(map[string]any{"id": 1, "text": "x", "color": "red"}))
	assert.Error(t, resolved.Validate(map[string]any{"id": 1, "text": "x", "color": nil}))

	// A function without arguments takes an empty object and nothing else.
	shop := newShopServer(t, shopConfig(t))
	me, ok := shop.Catalog().Function("accounts.me")
	require.True(t, ok)
	schema, err = me.InputSchema()
	require.NoError(t, err)
	assert.JSONEq(t, `{"type": "object", "additionalProperties": false}`, schemaJSON(t, schema))
}

// sameNameSpec has a path and a query parameter both named id, and an
// optional query and a required header parameter both named q.
const sameNameSpec = `{"openapi": "3.1.0", "info": {"title": "Same", "version": "1"}, "paths": {
	"/items/{id}": {"get": {
		"operationId": "items.get",
		"parameters": [
			{"name": "id", "in": "query", "schema": {"type": "string"}},
			{"name": "id", "in": "path", "required": true, "schema": {"type": "integer"}}
		],
		"responses": {"200": {"description": "OK"}}
	}},
	"/items": {"get": {
		"operationId": "items.find",
		"parameters": [
			{"name": "q", "in": "query", "schema": {"type": "string"}},
			{"name": "q", "in": "header", "required": true, "schema": {"type": "string"}}
		],
		"responses": {"200": {"description": "OK"}}
	}}
}}`

func TestServerPromotedToolsSameArgName(t *testing.T) {
	s, err := New(Config{
		Spec:    []byte(sameNameSpec),
		Handler: http.NotFoundHandler(),
		Tools:   []string{"items.get", "items.find"},
	})
	require.NoError(t, err)

	tools := listedTools(t, connect(t, s, ""))
	require.NotNil(t, tools["items-get"])
	assert.JSONEq(t, `{
		"type": "object",
		"properties": {"id": {"type": "integer"}},
		"required": ["id"],
		"additionalProperties": false
	}`, schemaJSON(t, tools["items-get"].InputSchema))

	// The second q is the required one.
	require.NotNil(t, tools["items-find"])
	assert.JSONEq(t, `{
		"type": "object",
		"properties": {"q": {"type": "string"}},
		"required": ["q"],
		"additionalProperties": false
	}`, schemaJSON(t, tools["items-find"].InputSchema))
}

// chainSpec returns a document whose body schema S0 references S1 twice, S1
// references S2 twice, and so on down to a string at Sdepth, so the inlined
// schema has 2^depth leaves.
func chainSpec(depth int) string {
	schemas := []string{}
	for i := 0; i < depth; i++ {
		next := fmt.Sprintf(`{"$ref": "#/components/schemas/S%d"}`, i+1)
		schemas = append(schemas, fmt.Sprintf(`"S%d": {"type": "object", "properties": {"a": %s, "b": %s}}`, i, next, next))
	}
	schemas = append(schemas, fmt.Sprintf(`"S%d": {"type": "string"}`, depth))
	return `{"openapi": "3.1.0", "info": {"title": "Chain", "version": "1"}, "paths": {
		"/chains": {"post": {
			"operationId": "chains.create",
			"requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/S0"}}}},
			"responses": {"200": {"description": "OK"}}
		}}
	}, "components": {"schemas": {` + strings.Join(schemas, ", ") + `}}}`
}

func TestServerPromotedToolsSchemaLimit(t *testing.T) {
	cfg := Config{Spec: []byte(chainSpec(3)), Handler: http.NotFoundHandler(), Tools: []string{"chains.create"}}
	_, err := New(cfg)
	require.NoError(t, err)

	for _, depth := range []int{10, 20} {
		cfg.Spec = []byte(chainSpec(depth))
		start := time.Now()
		_, err = New(cfg)
		assert.Equal(t, ez.EINVALID, ez.ErrorCode(err), "depth %d", depth)
		assert.Contains(t, ez.ErrorMessage(err), "the input schema of chains.create has more than 1000 nodes")
		// The budget bounds the work, not only the output: 2^20 leaves are
		// never expanded.
		assert.Less(t, time.Since(start), 2*time.Second, "depth %d", depth)
	}
}

func TestFunctionInputSchemaAllOfCycle(t *testing.T) {
	// The inline allOf flattens to a new copy on every visit and has no
	// component name, so only the resolved schema ends the recursion. The
	// argument is the copy the catalog flattened, so child expands twice.
	doc := parseSpec(t, `{"openapi": "3.1.0", "info": {"title": "Tree", "version": "1"}, "paths": {
		"/nodes": {"post": {
			"operationId": "nodes.create",
			"requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Node"}}}},
			"responses": {"200": {"description": "OK"}}
		}}
	}, "components": {"schemas": {
		"Node": {"type": "object", "properties": {
			"child": {"allOf": [{"$ref": "#/components/schemas/Node"}, {"properties": {"weight": {"type": "integer"}}}]}
		}}
	}}}`)
	c, err := buildCatalog(doc, nil, nil)
	require.NoError(t, err)
	schema, err := mustFunction(t, c, "nodes.create").InputSchema()
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"type": "object",
		"properties": {"child": {"type": "object", "properties": {
			"child": {"type": "object", "properties": {"child": {"type": "object"}, "weight": {"type": "integer"}}},
			"weight": {"type": "integer"}
		}}},
		"additionalProperties": false
	}`, schemaJSON(t, schema))
}

func TestFunctionInputSchemaNullableRefs(t *testing.T) {
	doc := parseSpec(t, `{"openapi": "3.1.0", "info": {"title": "Null", "version": "1"}, "paths": {
		"/people": {"post": {
			"operationId": "people.create",
			"requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Person"}}}},
			"responses": {"200": {"description": "OK"}}
		}}
	}, "components": {"schemas": {
		"Address": {"type": "object", "properties": {"street": {"type": "string"}}},
		"Person": {"type": "object", "properties": {
			"home": {"anyOf": [{"$ref": "#/components/schemas/Address"}, {"type": "null"}]},
			"work": {"type": "object", "nullable": true, "allOf": [{"$ref": "#/components/schemas/Address"}]},
			"either": {"anyOf": [{"$ref": "#/components/schemas/Address"}, {"type": "string"}, {"type": "null"}]},
			"nested": {"type": "object", "properties": {
				"home": {"anyOf": [{"$ref": "#/components/schemas/Address"}, {"type": "null"}], "description": "Where they live"},
				"work": {"type": "object", "nullable": true, "allOf": [{"$ref": "#/components/schemas/Address"}]}
			}}
		}}
	}}}`)
	c, err := buildCatalog(doc, nil, nil)
	require.NoError(t, err)
	f := mustFunction(t, c, "people.create")
	schema, err := f.InputSchema()
	require.NoError(t, err)

	address := `{"type": ["object", "null"], "properties": {"street": {"type": "string"}}}`
	assert.JSONEq(t, `{
		"type": "object",
		"properties": {
			"home": `+address+`,
			"work": `+address+`,
			"either": {"anyOf": [
				{"anyOf": [{"type": "object", "properties": {"street": {"type": "string"}}}, {"type": "string"}]},
				{"type": "null"}
			]},
			"nested": {"type": "object", "properties": {
				"home": {"type": ["object", "null"], "properties": {"street": {"type": "string"}}, "description": "Where they live"},
				"work": `+address+`
			}}
		},
		"additionalProperties": false
	}`, schemaJSON(t, schema))

	// describe_function marks the top-level arguments nullable too.
	var types []string
	for _, a := range f.Args {
		types = append(types, c.typeName(a.Schema))
	}
	assert.Equal(t, []string{"Address?", "Address?", "Address|string?", "object"}, types)
}

func TestFunctionInputSchemaNullableParams(t *testing.T) {
	doc := parseSpec(t, `{"openapi": "3.1.0", "info": {"title": "Trees", "version": "1"}, "paths": {
		"/trees/{code}/{year}/{month}": {"get": {
			"operationId": "trees.find",
			"parameters": [
				{"name": "code", "in": "path", "required": true, "schema": {"anyOf": [{"$ref": "#/components/schemas/Code"}, {"type": "null"}]}},
				{"name": "year", "in": "path", "required": true, "schema": {"$ref": "#/components/schemas/Year"}},
				{"name": "month", "in": "path", "required": true, "schema": {"type": "integer", "nullable": true}},
				{"name": "tree", "in": "query", "schema": {"anyOf": [{"$ref": "#/components/schemas/Tree"}, {"type": "null"}]}},
				{"name": "color", "in": "query", "schema": {"anyOf": [{"$ref": "#/components/schemas/Color"}, {"type": "null"}]}}
			],
			"responses": {"200": {"description": "OK"}}
		}}
	}, "components": {"schemas": {
		"Code": {"type": "string"},
		"Year": {"type": "integer", "nullable": true, "enum": [2025, 2026]},
		"Tree": {"type": "object", "properties": {"kids": {"type": "array", "items": {"$ref": "#/components/schemas/Tree"}}}},
		"Color": {"type": "string", "enum": ["red", "blue"]}
	}}}`)
	c, err := buildCatalog(doc, nil, nil)
	require.NoError(t, err)
	f := mustFunction(t, c, "trees.find")

	// A path argument is never nullable, by ref, by nullable component or
	// inline, since dispatch refuses null for it; the component keeps its
	// name.
	var types []string
	for _, a := range f.Args {
		types = append(types, c.argType(a))
	}
	assert.Equal(t, []string{"Code", "Year", "integer", "Tree?", "Color?"}, types)
	assert.Contains(t, c.describeFunction("trees.find"), "year   Year     required")

	// The nullable Tree stops at its own ref with its type, as a nested
	// bare ref does. The null member of Color is valid whatever the enum
	// says, through the argument wrapper too.
	schema, err := f.InputSchema()
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"type": "object",
		"properties": {
			"code": {"type": "string"},
			"year": {"type": "integer", "enum": [2025, 2026]},
			"month": {"type": "integer"},
			"tree": {"type": ["object", "null"], "properties": {"kids": {"type": "array", "items": {"type": "object"}}}},
			"color": {"anyOf": [{"type": "string", "enum": ["red", "blue"]}, {"type": "null"}]}
		},
		"required": ["code", "year", "month"],
		"additionalProperties": false
	}`, schemaJSON(t, schema))
	resolved, err := schema.Resolve(nil)
	require.NoError(t, err)
	assert.NoError(t, resolved.Validate(map[string]any{"code": "a", "year": 2025, "month": 1, "color": nil}))
}

func TestFunctionInputSchemaRequiredWithoutProperty(t *testing.T) {
	// A oneOf member that only requires a property, or forbids it through
	// not, has no local property to carry the required name.
	doc := parseSpec(t, `{"openapi": "3.1.0", "info": {"title": "Categories", "version": "1"}, "paths": {
		"/categories": {"post": {
			"operationId": "categories.create",
			"requestBody": {"required": true, "content": {"application/json": {"schema": {"$ref": "#/components/schemas/CreateRequest"}}}},
			"responses": {"200": {"description": "OK"}}
		}}
	}, "components": {"schemas": {
		"CreateRequest": {"type": "object", "required": ["type", "name"],
			"properties": {"name": {"type": "string"}, "organization_id": {"type": "string"}, "type": {"type": "string"}},
			"oneOf": [
				{"properties": {"type": {"type": "string", "enum": ["organization"]}}, "required": ["organization_id"]},
				{"properties": {"type": {"type": "string", "enum": ["platform"]}}, "not": {"required": ["organization_id"]}}
			]}
	}}}`)
	c, err := buildCatalog(doc, nil, nil)
	require.NoError(t, err)
	schema, err := mustFunction(t, c, "categories.create").InputSchema()
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"type": "object",
		"properties": {"name": {"type": "string"}, "organization_id": {"type": "string"}, "type": {"type": "string"}},
		"required": ["name", "type"],
		"oneOf": [
			{"type": "object", "properties": {"type": {"type": "string", "enum": ["organization"]}}, "required": ["organization_id"]},
			{"type": "object", "properties": {"type": {"type": "string", "enum": ["platform"]}}, "not": {"required": ["organization_id"]}}
		]
	}`, schemaJSON(t, schema.Properties["body"]))

	resolved, err := schema.Resolve(nil)
	require.NoError(t, err)
	valid := func(body string) bool {
		var value map[string]any
		require.NoError(t, json.Unmarshal([]byte(body), &value))
		return resolved.Validate(map[string]any{"body": value}) == nil
	}
	assert.True(t, valid(`{"type": "platform", "name": "Maintenance"}`))
	assert.True(t, valid(`{"type": "organization", "name": "Billing", "organization_id": "org_1"}`))
	assert.False(t, valid(`{"type": "organization", "name": "Billing"}`))
	assert.False(t, valid(`{"type": "platform", "name": "Maintenance", "organization_id": "org_1"}`))
}

func TestFunctionInputSchemaRecursiveOneOf(t *testing.T) {
	// A recursive branch of a oneOf is cut to its type, so a leaf matches
	// one branch only instead of the string and an any-value stub.
	doc := parseSpec(t, `{"openapi": "3.1.0", "info": {"title": "Nodes", "version": "1"}, "paths": {
		"/nodes": {"post": {
			"operationId": "nodes.create",
			"requestBody": {"required": true, "content": {"application/json": {"schema": {
				"type": "object", "properties": {"root": {"$ref": "#/components/schemas/Node"}}
			}}}},
			"responses": {"200": {"description": "OK"}}
		}}
	}, "components": {"schemas": {
		"Node": {"type": "object", "properties": {"child": {"oneOf": [{"type": "string"}, {"$ref": "#/components/schemas/Node"}]}}}
	}}}`)
	c, err := buildCatalog(doc, nil, nil)
	require.NoError(t, err)
	schema, err := mustFunction(t, c, "nodes.create").InputSchema()
	require.NoError(t, err)
	assert.JSONEq(t, `{"type": "object", "properties": {"child": {"oneOf": [{"type": "string"}, {"type": "object"}]}}}`, schemaJSON(t, schema.Properties["root"]))
	resolved, err := schema.Resolve(nil)
	require.NoError(t, err)
	assert.NoError(t, resolved.Validate(map[string]any{"root": map[string]any{"child": "leaf"}}))
	assert.NoError(t, resolved.Validate(map[string]any{"root": map[string]any{"child": map[string]any{"child": "leaf"}}}))
	assert.Error(t, resolved.Validate(map[string]any{"root": map[string]any{"child": 1}}))
}

func TestFunctionInputSchemaBodyRules(t *testing.T) {
	// A body with a rule over its properties together is one argument, so
	// the rule reaches the schema.
	doc := parseSpec(t, `{"openapi": "3.1.0", "info": {"title": "Patch", "version": "1"}, "paths": {
		"/things/{id}": {"patch": {
			"operationId": "things.update",
			"parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "integer"}}],
			"requestBody": {"required": true, "content": {"application/json": {"schema": {
				"type": "object", "minProperties": 1, "properties": {"a": {"type": "string"}, "b": {"type": "string"}},
				"not": {"required": ["a", "b"]}
			}}}},
			"responses": {"200": {"description": "OK"}}
		}}
	}}`)
	c, err := buildCatalog(doc, nil, nil)
	require.NoError(t, err)
	f := mustFunction(t, c, "things.update")
	var names []string
	for _, a := range f.Args {
		names = append(names, a.Name)
	}
	assert.Equal(t, []string{"id", "body"}, names)
	schema, err := f.InputSchema()
	require.NoError(t, err)
	resolved, err := schema.Resolve(nil)
	require.NoError(t, err)
	assert.NoError(t, resolved.Validate(map[string]any{"id": 1, "body": map[string]any{"a": "x"}}))
	assert.Error(t, resolved.Validate(map[string]any{"id": 1, "body": map[string]any{}}))
	assert.Error(t, resolved.Validate(map[string]any{"id": 1, "body": map[string]any{"a": "x", "b": "y"}}))
}

func TestJSONSchemaNullAndConst(t *testing.T) {
	doc := parseSpec(t, `{"openapi": "3.1.0", "info": {"title": "Null", "version": "1"}, "paths": {}, "components": {"schemas": {
		"Null": {"type": "null"},
		"Fixed": {"const": "x"},
		"MaybeFixed": {"const": "x", "nullable": true},
		"TypedMaybeFixed": {"type": "string", "const": "x", "nullable": true},
		"MemberFixed": {"anyOf": [{"type": "string", "const": "x"}, {"type": "null"}]},
		"Color": {"type": ["string", "null"], "enum": ["red", null]},
		"Shade": {"type": ["string", "null"], "enum": ["red", "blue"]},
		"MemberShade": {"anyOf": [{"type": "string", "enum": ["red", "blue"]}, {"type": "null"}]},
		"MemberPlain": {"oneOf": [{"type": "string"}, {"type": "null"}]},
		"Cat": {"type": "object", "properties": {"meow": {"type": "boolean"}}},
		"Dog": {"type": "object", "properties": {"bark": {"type": "boolean"}}},
		"Pet": {"type": "object", "nullable": true, "oneOf": [{"$ref": "#/components/schemas/Cat"}, {"$ref": "#/components/schemas/Dog"}]},
		"Loose": {"oneOf": [{"type": ["string", "null"]}, {"type": "integer"}, {"type": "null"}]},
		"List": {"nullable": true, "items": {"type": "string"}},
		"NotString": {"not": {"type": "string"}},
		"Never": false
	}}}`)
	convert := func(name string) string {
		b := &schemaBuilder{doc: doc, visiting: map[any]bool{}}
		return schemaJSON(t, b.convert(doc.Components.Schemas.Map[name]))
	}
	assert.JSONEq(t, `{"type": "null"}`, convert("Null"))
	assert.JSONEq(t, `{"const": "x"}`, convert("Fixed"))
	assert.JSONEq(t, `{"anyOf": [{"const": "x"}, {"type": "null"}]}`, convert("MaybeFixed"))
	// Null from nullable or the type list leaves the const and enum deciding;
	// null from an explicit member is valid whatever they say.
	assert.JSONEq(t, `{"type": ["string", "null"], "const": "x"}`, convert("TypedMaybeFixed"))
	assert.JSONEq(t, `{"anyOf": [{"type": "string", "const": "x"}, {"type": "null"}]}`, convert("MemberFixed"))
	assert.JSONEq(t, `{"type": ["string", "null"], "enum": ["red", null]}`, convert("Color"))
	assert.JSONEq(t, `{"type": ["string", "null"], "enum": ["red", "blue"]}`, convert("Shade"))
	assert.JSONEq(t, `{"anyOf": [{"type": "string", "enum": ["red", "blue"]}, {"type": "null"}]}`, convert("MemberShade"))
	assert.JSONEq(t, `{"type": ["string", "null"]}`, convert("MemberPlain"))
	admitsNull := func(name string) bool {
		b := &schemaBuilder{doc: doc, visiting: map[any]bool{}}
		resolved, err := b.convert(doc.Components.Schemas.Map[name]).Resolve(nil)
		require.NoError(t, err)
		return resolved.Validate(nil) == nil
	}
	assert.False(t, admitsNull("TypedMaybeFixed"))
	assert.True(t, admitsNull("MemberFixed"))
	assert.True(t, admitsNull("Color"))
	assert.False(t, admitsNull("Shade"))
	assert.True(t, admitsNull("MemberShade"))
	assert.True(t, admitsNull("MemberPlain"))
	// A typed schema with alternatives admits null through an outer anyOf.
	assert.JSONEq(t, `{"anyOf": [
		{"type": "object", "oneOf": [
			{"type": "object", "properties": {"meow": {"type": "boolean"}}},
			{"type": "object", "properties": {"bark": {"type": "boolean"}}}
		]},
		{"type": "null"}
	]}`, convert("Pet"))
	// A null member appended to the oneOf would match twice with the
	// nullable string and fail; the outer anyOf does not.
	assert.JSONEq(t, `{"anyOf": [{"oneOf": [{"type": ["string", "null"]}, {"type": "integer"}]}, {"type": "null"}]}`, convert("Loose"))
	assert.JSONEq(t, `{"anyOf": [{"items": {"type": "string"}}, {"type": "null"}]}`, convert("List"))
	assert.JSONEq(t, `{"not": {"type": "string"}}`, convert("NotString"))
	assert.JSONEq(t, `false`, convert("Never"))

	// In OpenAPI 3.0 nullable without a type is any value, not null alone.
	doc = parseSpec(t, `{"openapi": "3.0.3", "info": {"title": "Any", "version": "1"}, "paths": {}, "components": {"schemas": {
		"Any": {"nullable": true, "description": "Any JSON value"}
	}}}`)
	assert.JSONEq(t, `{"description": "Any JSON value"}`, convert("Any"))
}
