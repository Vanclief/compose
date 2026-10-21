package generator

import (
	"reflect"
	"strings"
	"testing"
)

// aliasChainFixture routes POST /invoices to CreateInvoiceV3, which only returns
// h.CreateInvoiceV2(c), which only returns h.CreateInvoice(c): a chain of handler aliases.
func aliasChainFixture() map[string]string {
	files := map[string]string{}
	for name, contents := range requiredFixture {
		files[name] = contents
	}
	files["api/routes.go"] = `package api

func addRoutes(e *echo.Echo, h *Handler) {
	e.POST("/invoices", h.CreateInvoiceV3)
}
`
	files["api/handler.go"] = `package api

import (
	"github.com/labstack/echo/v4"

	"example.com/app/modules/invoices"
)

type Handler struct{}

// CreateInvoice issues an invoice.
func (h *Handler) CreateInvoice(c echo.Context) error {
	return h.BindedJSONResponse(c, "CreateInvoice", nil, &invoices.CreateRequest{})
}

func (h *Handler) CreateInvoiceV2(c echo.Context) error {
	return h.CreateInvoice(c)
}

func (h *Handler) CreateInvoiceV3(c echo.Context) error {
	return h.CreateInvoiceV2(c)
}
`

	return files
}

func TestHandlerAliasChainsResolveToTheirTarget(t *testing.T) {
	root := writeFixture(t, aliasChainFixture())
	cfg := Config{
		Root:        root,
		RoutesGlob:  "api/routes.go",
		HandlerGlob: "api/*.go",
		DocComments: true,
	}
	wantBody := map[string]any{"$ref": "#/components/schemas/modules_invoices_CreateRequest"}

	// The alias map iterates in random order; a chain must resolve the same way every time.
	for run := 0; run < 40; run++ {
		spec, err := Generate(cfg)
		if err != nil {
			t.Fatal(err)
		}

		post := operation(t, spec.Document, "/invoices", "post")
		requestBody, ok := post["requestBody"].(map[string]any)
		if !ok {
			t.Fatalf("run %d: expected the chained alias to inherit the request body, got %v", run, post)
		}
		body := requestBody["content"].(map[string]any)["application/json"].(map[string]any)["schema"]
		if !reflect.DeepEqual(body, wantBody) {
			t.Fatalf("run %d: expected request body %v, got %v", run, wantBody, body)
		}
		if post["description"] != "CreateInvoice issues an invoice." {
			t.Fatalf("run %d: expected the chained alias to inherit the doc comment, got %v", run, post["description"])
		}
	}
}

func TestFillAliasesTakesTheDeepestValueDownTheChain(t *testing.T) {
	aliases := map[string]string{
		// A wraps B wraps C wraps D; D's body is one compose response call, which reads as an
		// alias of a method that is not a handler.
		"A": "B",
		"B": "C",
		"C": "D",
		"D": "BindedJSONResponse",
		// Loops end where they close.
		"Self": "Self",
		"X":    "Y",
		"Y":    "X",
	}
	want := map[string]string{"A": "d", "B": "d", "C": "d", "D": "d", "X": "x", "Y": "x"}

	// The alias map iterates in random order; the result must not depend on it.
	for run := 0; run < 100; run++ {
		out := map[string]string{"B": "b", "D": "d", "X": "x"}
		fillAliases(out, aliases)
		if !reflect.DeepEqual(out, want) {
			t.Fatalf("run %d: expected %v, got %v", run, want, out)
		}
	}
}

func TestAliasChain(t *testing.T) {
	aliases := map[string]string{"A": "B", "B": "C", "X": "Y", "Y": "X", "Self": "Self"}
	cases := map[string][]string{
		"A":    {"B", "C"},
		"B":    {"C"},
		"C":    nil,
		"X":    {"Y"},
		"Self": nil,
	}
	for alias, want := range cases {
		got := aliasChain(aliases, alias)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("aliasChain(%s) = %v, want %v", alias, got, want)
		}
	}
}

// A handler package is matched to its Handlers field by the name in its package clause, not by
// the last element of its import path (handler/v2 holding package widgets).
func TestHandlerPackageNamespaceUsesPackageClause(t *testing.T) {
	files := map[string]string{}
	for name, contents := range nestedFixture {
		files[name] = contents
	}
	files["api/handlers.go"] = strings.Replace(files["api/handlers.go"], `"example.com/app/api/handler/widgets"`, `"example.com/app/api/handler/v2"`, 1)
	files["api/handler/v2/widgets.go"] = files["api/handler/widgets/widgets.go"]
	delete(files, "api/handler/widgets/widgets.go")
	root := writeFixture(t, files)

	spec, err := Generate(nestedFixtureConfig(root))
	if err != nil {
		t.Fatal(err)
	}

	paths := spec.Document["paths"].(map[string]any)
	_, ok := paths["/widgets/{id}"]
	if !ok {
		t.Fatalf("expected the v2 handler package's routes, got %v", sortedKeys(paths))
	}
}
