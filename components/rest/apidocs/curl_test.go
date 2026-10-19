package apidocs

import (
	"testing"

	"github.com/stretchr/testify/require"
)

const curlSpec = `
openapi: 3.0.3
info:
  title: Shop
  version: "1"
servers:
  - url: https://api.example.com/
components:
  parameters:
    OrderID:
      in: path
      name: order_id
      required: true
      schema:
        type: string
        format: uuid
  securitySchemes:
    Bearer:
      type: http
      scheme: bearer
    Basic:
      type: http
      scheme: basic
    QueryKey:
      type: apiKey
      in: query
      name: api-key
security:
  - Bearer: []
paths:
  /orders/{order_id}/items/{item_id}:
    parameters:
      - $ref: '#/components/parameters/OrderID'
      - in: path
        name: item_id
        required: true
        schema:
          type: integer
      - in: query
        name: expand
        required: true
        schema:
          type: string
          example: from path item
    get:
      parameters:
        - in: query
          name: expand
          required: true
          schema:
            type: array
            items:
              type: string
              enum: [lines, totals]
        - in: query
          name: q
          required: true
          example: a b&c
        - in: query
          name: page
          schema:
            type: integer
        - in: header
          name: X-Tenant
          required: true
          schema:
            type: string
        - in: header
          name: Accept
          required: true
          schema:
            type: string
    post:
      security:
        - Basic: []
          QueryKey: []
      requestBody:
        content:
          application/json:
            example:
              note: "it's here"
  /uploads:
    put:
      security: []
      requestBody:
        content:
          application/octet-stream:
            schema:
              type: string
              format: binary
`

func curlFor(t *testing.T, doc *Document, method, path, server string) string {
	t.Helper()
	item, ok := doc.Paths.Get(path)
	require.True(t, ok, path)
	for _, m := range item.Methods() {
		if m.Method == method {
			return doc.Curl(method, path, item, m.Operation, server)
		}
	}
	t.Fatalf("no %s %s", method, path)
	return ""
}

func TestCurlParametersAndBearer(t *testing.T) {
	doc := parseSpec(t, curlSpec)

	got := curlFor(t, doc, "GET", "/orders/{order_id}/items/{item_id}", "")
	require.Equal(t, `curl 'https://api.example.com/orders/123e4567-e89b-12d3-a456-426614174000/items/0?expand=lines&q=a+b%26c' \
  -H "Authorization: Bearer $TOKEN" \
  -H 'X-Tenant: string'`, got)
}

func TestCurlBasicQueryKeyAndBody(t *testing.T) {
	doc := parseSpec(t, curlSpec)

	got := curlFor(t, doc, "POST", "/orders/{order_id}/items/{item_id}", "http://localhost:8080")
	// Credentials are double-quoted so the shell expands them; everything
	// else stays single-quoted.
	require.Equal(t, `curl -X POST 'http://localhost:8080/orders/123e4567-e89b-12d3-a456-426614174000/items/0?expand=from+path+item&api-key='"$API_KEY" \
  -u "$BASIC_USER:$BASIC_PASSWORD" \
  -H 'Content-Type: application/json' \
  -d '{
  "note": "it'\''s here"
}'`, got)
}

func TestCurlBinaryBodyAndPublicOperation(t *testing.T) {
	doc := parseSpec(t, curlSpec)

	got := curlFor(t, doc, "PUT", "/uploads", "")
	require.Equal(t, `curl -X PUT 'https://api.example.com/uploads' \
  -H 'Content-Type: application/octet-stream' \
  --data-binary '@file'`, got)
}

func TestCurlWithoutServerOrOperation(t *testing.T) {
	doc := parseSpec(t, curlSpec)
	doc.Servers = nil

	require.Equal(t, `curl '/health'`, doc.Curl("get", "/health", nil, nil, ""))
}

func TestCurlEmma(t *testing.T) {
	doc := loadEmma(t, "emma.yaml")

	// Login is public (security: []), so it carries no credentials.
	login := curlFor(t, doc, "POST", "/auth/login", "")
	require.Contains(t, login, `curl -X POST 'https://api.holaemma.app/auth/login' \`)
	require.Contains(t, login, `-H 'Content-Type: application/json'`)
	require.Contains(t, login, `-d '{`)
	require.Contains(t, login, `"destinatary": "string"`)
	require.NotContains(t, login, "API-KEY")

	codes := curlFor(t, doc, "GET", "/complexes/{complex_id}/access-codes", "")
	require.Equal(t, `curl 'https://api.holaemma.app/complexes/0/access-codes' \
  -H "API-KEY: $API_KEY" \
  -H "API-SECRET: $API_SECRET"`, codes)

	create := curlFor(t, doc, "POST", "/complexes/{complex_id}/access-codes", "")
	require.Contains(t, create, `-H "API-KEY: $API_KEY"`)
	require.Contains(t, create, `-H 'Content-Type: application/json'`)
	require.Contains(t, create, `-d '{`)
	require.NotContains(t, create, "{complex_id}")
}

func TestCurlCredentialsExpandInAShell(t *testing.T) {
	doc := parseSpec(t, curlSpec)

	got := curlFor(t, doc, "POST", "/orders/{order_id}/items/{item_id}", "")
	require.NotContains(t, got, `'$`, "a single-quoted placeholder never expands")
	require.Equal(t, `"API-KEY: $API_KEY"`, shellPlaceholder("API-KEY: ", "API_KEY"))
	require.Equal(t, `"we\"ird \$x\\ \`+"`"+`: $K"`, shellPlaceholder(`we"ird $x\ `+"`"+`: `, "K"))
	require.Equal(t, `'https://h/p?a=1&k='"$K"'&q='"$Q"`, quoteTarget("https://h/p?a=1", true, []queryPlaceholder{{"k", "K"}, {"q", "Q"}}))
	require.Equal(t, `'https://h/p?k='"$K"`, quoteTarget("https://h/p", false, []queryPlaceholder{{"k", "K"}}))
}

const curlServersSpec = `
openapi: 3.0.3
info: {title: t, version: "1"}
servers:
  - url: http://localhost:{port}/{base}
    variables:
      port: {default: "8080", enum: ["8080", "9090"]}
      base: {default: v1}
paths:
  /doc:
    get:
      responses: {"200": {description: ok}}
  /own:
    get:
      servers:
        - url: https://{region}.example.com
          variables:
            region: {default: eu}
      responses: {"200": {description: ok}}
`

func TestCurlServerPrecedenceAndVariables(t *testing.T) {
	doc := parseSpec(t, curlServersSpec)

	// Document servers expand their variables to the defaults.
	require.Equal(t, `curl 'http://localhost:8080/v1/doc'`, curlFor(t, doc, "GET", "/doc", ""))

	// Operation servers win over document servers when there is no override.
	require.Equal(t, `curl 'https://eu.example.com/own'`, curlFor(t, doc, "GET", "/own", ""))

	// An explicit server argument wins over both.
	require.Equal(t, `curl 'http://127.0.0.1:3000/own'`, curlFor(t, doc, "GET", "/own", "http://127.0.0.1:3000"))

	// An override naming a declared server is expanded with its variables.
	require.Equal(t, `curl 'http://localhost:8080/v1/doc'`, curlFor(t, doc, "GET", "/doc", "http://localhost:{port}/{base}"))

	require.Equal(t, "http://localhost:8080", Server{
		URL:       "http://localhost:{port}",
		Variables: map[string]ServerVariable{"port": {Default: "8080"}},
	}.Expand())
}

const curlArraySpec = `
openapi: 3.0.3
info: {title: t, version: "1"}
paths:
  /items/{ids}:
    get:
      parameters:
        - {in: path, name: ids, required: true, schema: {type: array, items: {type: string}}, example: [a, b]}
        - {in: query, name: tags, required: true, schema: {type: array, items: {type: string}}, example: [a, b]}
        - {in: query, name: csv, required: true, explode: false, example: [a, b]}
        - {in: query, name: pipes, required: true, style: pipeDelimited, example: [a, b]}
        - {in: query, name: spaces, required: true, style: spaceDelimited, example: [a, b]}
        - {in: query, name: none, required: true, example: []}
        - {in: header, name: X-Ids, required: true, example: [1, 2]}
      responses: {"200": {description: ok}}
`

func TestCurlArrayParameterStyles(t *testing.T) {
	doc := parseSpec(t, curlArraySpec)

	got := curlFor(t, doc, "GET", "/items/{ids}", "")
	require.Equal(t, `curl '/items/a%2Cb?tags=a&tags=b&csv=a%2Cb&pipes=a%7Cb&spaces=a+b' \
  -H 'X-Ids: 1,2'`, got)
}

func TestParamValues(t *testing.T) {
	yes := true
	no := false
	values := []any{"a", float64(2), true}

	cases := map[string]struct {
		param *Parameter
		value any
		want  []string
	}{
		"query explodes by default":  {&Parameter{In: "query"}, values, []string{"a", "2", "true"}},
		"cookie explodes by default": {&Parameter{In: "cookie"}, values, []string{"a", "2", "true"}},
		"query explode false":        {&Parameter{In: "query", Explode: &no}, values, []string{"a,2,true"}},
		"path joins":                 {&Parameter{In: "path"}, values, []string{"a,2,true"}},
		"header joins":               {&Parameter{In: "header"}, values, []string{"a,2,true"}},
		"simple explode still joins": {&Parameter{In: "path", Style: "simple", Explode: &yes}, values, []string{"a,2,true"}},
		"pipeDelimited":              {&Parameter{In: "query", Style: "pipeDelimited"}, values, []string{"a|2|true"}},
		"spaceDelimited":             {&Parameter{In: "query", Style: "spaceDelimited"}, values, []string{"a 2 true"}},
		"pipeDelimited explode":      {&Parameter{In: "query", Style: "pipeDelimited", Explode: &yes}, values, []string{"a", "2", "true"}},
		"form explicit":              {&Parameter{In: "header", Style: "form"}, values, []string{"a", "2", "true"}},
		"scalar":                     {&Parameter{In: "query"}, float64(1000000), []string{"1000000"}},
		"nil":                        {&Parameter{In: "query"}, nil, []string{""}},
		"empty exploded array":       {&Parameter{In: "query"}, []any{}, []string{}},
		"empty joined array":         {&Parameter{In: "path"}, []any{}, []string{""}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, ParamValues(tc.param, tc.value))
		})
	}
}

func TestParamString(t *testing.T) {
	ordered := &Ordered[any]{}
	ordered.Set("b", 1)
	ordered.Set("a", "<x>")

	require.Equal(t, "", ParamString(nil))
	require.Equal(t, "text", ParamString("text"))
	require.Equal(t, "true", ParamString(true))
	require.Equal(t, "1000000", ParamString(float64(1e6)))
	require.Equal(t, "1.5", ParamString(1.5))
	require.Equal(t, "42", ParamString(int64(42)))
	require.Equal(t, "a,1,false", ParamString([]any{"a", float64(1), false}))
	require.Equal(t, `{"b":1,"a":"<x>"}`, ParamString(ordered))
	require.Equal(t, `{"a":1,"b":"x"}`, ParamString(map[string]any{"b": "x", "a": 1}))
	require.Equal(t, `{"1":true}`, ParamString(map[any]any{1: true}))
}

const curlFormSpec = `
openapi: 3.0.3
info: {title: t, version: "1"}
components:
  schemas:
    Upload:
      type: object
      properties:
        id: {type: string, readOnly: true}
        name: {type: string, example: "it's"}
        file: {type: string, format: binary}
        attachments: {type: array, items: {type: string, format: binary}}
        tags: {type: array, items: {type: string}, example: [a, b]}
        meta: {type: object, properties: {n: {type: integer}}}
paths:
  /upload:
    post:
      requestBody:
        content:
          multipart/form-data:
            schema: {$ref: '#/components/schemas/Upload'}
      responses: {"200": {description: ok}}
  /login:
    post:
      requestBody:
        content:
          application/x-www-form-urlencoded:
            schema:
              type: object
              properties:
                user: {type: string, example: a b}
                scope: {type: array, items: {type: string}, example: [read, write]}
      responses: {"200": {description: ok}}
  /explicit:
    post:
      requestBody:
        content:
          application/x-www-form-urlencoded:
            example: {z: 1, a: x}
      responses: {"200": {description: ok}}
  /text:
    post:
      requestBody:
        content:
          text/plain:
            schema: {type: string}
      responses: {"200": {description: ok}}
`

func TestCurlMultipartBody(t *testing.T) {
	doc := parseSpec(t, curlFormSpec)

	got := curlFor(t, doc, "POST", "/upload", "")
	require.Equal(t, `curl -X POST '/upload' \
  -F 'name=it'\''s' \
  -F 'file=@path' \
  -F 'attachments=@path' \
  -F 'tags=a' \
  -F 'tags=b' \
  -F 'meta={"n":0}'`, got)
	require.NotContains(t, got, "Content-Type", "curl -F sets the multipart boundary itself")
}

func TestCurlFormURLEncodedBody(t *testing.T) {
	doc := parseSpec(t, curlFormSpec)

	require.Equal(t, `curl -X POST '/login' \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  --data-urlencode 'user=a b' \
  --data-urlencode 'scope=read' \
  --data-urlencode 'scope=write'`, curlFor(t, doc, "POST", "/login", ""))

	// Explicit examples decoded as plain maps are emitted in sorted key order.
	require.Equal(t, `curl -X POST '/explicit' \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  --data-urlencode 'a=x' \
  --data-urlencode 'z=1'`, curlFor(t, doc, "POST", "/explicit", ""))

	require.Equal(t, `curl -X POST '/text' \
  -H 'Content-Type: text/plain' \
  --data-binary '@file'`, curlFor(t, doc, "POST", "/text", ""))
}
