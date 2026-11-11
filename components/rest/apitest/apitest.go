// Package apitest checks recorded responses against the contract of an
// api.Registry: the strict OpenAPI 3.1 document, where every response object
// component has additionalProperties:false so undocumented fields fail.
// Non-JSON 200 responses (api.Blob) are checked against the operation's
// declared media types. The body of a HEAD operation's response is not
// checked: it has none.
package apitest

import (
	"bytes"
	"mime"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/vanclief/ez"

	"github.com/vanclief/compose/components/rest/api"
	"github.com/vanclief/compose/components/rest/api/validate"
)

// Contract validates responses of a registry's operations.
type Contract struct {
	responses map[string]*validate.Schema // operations that produce JSON
	produces  map[string][]string
	methods   map[string]string
	errors    *validate.Schema // the default response every operation shares
}

// New compiles one validator per operation's application/json 200 response
// (hidden operations included) and one for the error envelope.
func New(t testing.TB, reg *api.Registry) *Contract {
	t.Helper()

	doc := reg.StrictSpec()

	compiler, err := validate.NewCompiler(doc)
	if err != nil {
		t.Fatalf("apitest: load strict spec: %s", ez.ErrorMessage(err))
	}

	c := &Contract{responses: map[string]*validate.Schema{}, produces: map[string][]string{}, methods: map[string]string{}}

	for _, op := range reg.Operations() {
		responses := "spec.json#/paths/" + pointerEscape(op.SpecPath) + "/" + strings.ToLower(op.Method) + "/responses/"

		if c.errors == nil {
			c.errors, err = compiler.Compile("contract:error", wrap(responses+"default/content/application~1json/schema"))
			if err != nil {
				t.Fatalf("apitest: compile error response: %s", ez.ErrorMessage(err))
			}
		}

		c.produces[op.OperationID] = op.Produces
		c.methods[op.OperationID] = op.Method
		if !slices.Contains(op.Produces, echo.MIMEApplicationJSON) {
			continue
		}

		c.responses[op.OperationID], err = compiler.Compile("contract:"+op.OperationID, wrap(responses+"200/content/application~1json/schema"))
		if err != nil {
			t.Fatalf("apitest: compile %s response: %s", op.OperationID, ez.ErrorMessage(err))
		}
	}

	return c
}

// AssertResponse validates a response body against the operation's 200
// schema (status 200) or the error envelope (any other status), and reports
// the failing fields with t.Errorf (the first validate.MaxFields of them and
// a count of the rest), that an object repeats a key, or that the body is
// empty. The body of a HEAD operation's response is not
// checked: rest.ErrorHandler answers a HEAD request with the status alone.
func (c *Contract) AssertResponse(t testing.TB, operationID string, status int, body []byte) {
	t.Helper()

	_, known := c.produces[operationID]
	if !known {
		t.Errorf("apitest: unknown operation %q", operationID)
		return
	}
	if c.methods[operationID] == http.MethodHead {
		return
	}

	schema := c.errors
	if status == http.StatusOK {
		var ok bool
		schema, ok = c.responses[operationID]
		if !ok {
			t.Errorf("apitest: %s does not produce application/json", operationID)
			return
		}
	}

	// DecodeBytes reads an empty body as null, which a nullable Res allows.
	if len(bytes.TrimSpace(body)) == 0 {
		t.Errorf("apitest: %s %d response has no body", operationID, status)
		return
	}
	decoded, err := validate.DecodeBytes(body)
	if err != nil {
		t.Errorf("apitest: %s %d response is not JSON: %s", operationID, status, ez.ErrorMessage(err))
		return
	}
	// The schema sees only a repeated key's last value, and clients' parsers
	// disagree on which one they keep.
	err = validate.RejectRepeatedKeys(body)
	if err != nil {
		t.Errorf("apitest: %s %d response does not match the contract: %s", operationID, status, ez.ErrorMessage(err))
		return
	}

	err = schema.Validate(map[string]any{"response": decoded})
	if err != nil {
		t.Errorf("apitest: %s %d response does not match the contract: %s", operationID, status, ez.ErrorMessage(err))
	}
}

// Do serves req on e, asserts the response matches the contract and returns
// the recorder. A 200 response that is not application/json must carry one
// of the operation's declared media types.
func (c *Contract) Do(t testing.TB, e *echo.Echo, operationID string, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	// The parse error is dropped: a Content-Type that does not parse leaves
	// mediaType empty, which assertMediaType reports as an undeclared type.
	mediaType, _, _ := mime.ParseMediaType(rec.Header().Get(echo.HeaderContentType))
	if rec.Code == http.StatusOK && mediaType != echo.MIMEApplicationJSON {
		c.assertMediaType(t, operationID, mediaType)
		return rec
	}

	c.AssertResponse(t, operationID, rec.Code, rec.Body.Bytes())
	return rec
}

func (c *Contract) assertMediaType(t testing.TB, operationID, mediaType string) {
	t.Helper()

	produces, ok := c.produces[operationID]
	if !ok {
		t.Errorf("apitest: unknown operation %q", operationID)
		return
	}
	if !slices.Contains(produces, mediaType) {
		t.Errorf("apitest: %s 200 response has undeclared content type %q (declared: %s)", operationID, mediaType, strings.Join(produces, ", "))
	}
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
