// Package apitest checks recorded responses against the contract of an
// api.Registry: the strict OpenAPI 3.1 document, where every response object
// component has additionalProperties:false so undocumented fields fail.
package apitest

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/vanclief/ez"

	"github.com/vanclief/compose/components/rest/api"
	"github.com/vanclief/compose/components/rest/api/validate"
)

// errorResponseRef is the component every operation's default response uses.
const errorResponseRef = "spec.json#/components/schemas/handler.ErrorResponse"

// Contract validates responses of a registry's operations.
type Contract struct {
	responses map[string]*validate.Schema
	errors    *validate.Schema
}

// New compiles one validator per operation's 200 response (hidden and legacy
// operations included) and one for the error envelope.
func New(t testing.TB, reg *api.Registry) *Contract {
	t.Helper()

	doc, err := reg.StrictSpec()
	if err != nil {
		t.Fatalf("apitest: strict spec: %s", ez.ErrorMessage(err))
	}

	compiler, err := validate.NewCompiler(doc)
	if err != nil {
		t.Fatalf("apitest: load strict spec: %s", ez.ErrorMessage(err))
	}

	c := &Contract{responses: map[string]*validate.Schema{}}

	c.errors, err = compiler.Compile("contract:error", wrap(errorResponseRef))
	if err != nil {
		t.Fatalf("apitest: compile error response: %s", ez.ErrorMessage(err))
	}

	for _, op := range reg.Operations() {
		ref := "spec.json#/paths/" + pointerEscape(op.SpecPath) + "/" + strings.ToLower(op.Method) +
			"/responses/200/content/application~1json/schema"

		c.responses[op.OperationID], err = compiler.Compile("contract:"+op.OperationID, wrap(ref))
		if err != nil {
			t.Fatalf("apitest: compile %s response: %s", op.OperationID, ez.ErrorMessage(err))
		}
	}

	return c
}

// AssertResponse validates a response body against the operation's 200
// schema (status 200) or the error envelope (any other status), and reports
// every failing field with t.Errorf.
func (c *Contract) AssertResponse(t testing.TB, operationID string, status int, body []byte) {
	t.Helper()

	schema := c.errors
	if status == http.StatusOK {
		var ok bool
		schema, ok = c.responses[operationID]
		if !ok {
			t.Errorf("apitest: unknown operation %q", operationID)
			return
		}
	}

	decoded, err := validate.DecodeBytes(body)
	if err != nil {
		t.Errorf("apitest: %s %d response is not JSON: %s", operationID, status, ez.ErrorMessage(err))
		return
	}

	err = schema.Validate(map[string]any{"response": decoded})
	if err != nil {
		t.Errorf("apitest: %s %d response does not match the contract: %s", operationID, status, ez.ErrorMessage(err))
	}
}

// Do serves req on e, asserts the response matches the contract and returns
// the recorder.
func (c *Contract) Do(t testing.TB, e *echo.Echo, operationID string, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	c.AssertResponse(t, operationID, rec.Code, rec.Body.Bytes())
	return rec
}

// wrap validates {"response": <body>} so field errors read "response.x ...".
func wrap(ref string) map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{"response": map[string]any{"$ref": ref}},
		"required":   []string{"response"},
	}
}

// pointerEscape escapes a JSON pointer token (RFC 6901), then the characters
// a URI fragment cannot hold.
func pointerEscape(token string) string {
	token = strings.ReplaceAll(token, "~", "~0")
	token = strings.ReplaceAll(token, "/", "~1")
	token = strings.ReplaceAll(token, "{", "%7B")
	return strings.ReplaceAll(token, "}", "%7D")
}
