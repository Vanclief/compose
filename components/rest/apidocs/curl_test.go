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
  -H 'Authorization: Bearer $TOKEN' \
  -H 'X-Tenant: string'`, got)
}

func TestCurlBasicQueryKeyAndBody(t *testing.T) {
	doc := parseSpec(t, curlSpec)

	got := curlFor(t, doc, "POST", "/orders/{order_id}/items/{item_id}", "http://localhost:8080")
	require.Equal(t, `curl -X POST 'http://localhost:8080/orders/123e4567-e89b-12d3-a456-426614174000/items/0?expand=from+path+item&api-key=$API_KEY' \
  -u '$USER:$PASSWORD' \
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
  -H 'API-KEY: $API_KEY' \
  -H 'API-SECRET: $API_SECRET'`, codes)

	create := curlFor(t, doc, "POST", "/complexes/{complex_id}/access-codes", "")
	require.Contains(t, create, `-H 'API-KEY: $API_KEY'`)
	require.Contains(t, create, `-H 'Content-Type: application/json'`)
	require.Contains(t, create, `-d '{`)
	require.NotContains(t, create, "{complex_id}")
}
