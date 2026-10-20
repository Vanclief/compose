package generator

import (
	"reflect"
	"testing"
)

// documentedFixture is nestedFixture with a commented bun model and a second namespace,
// gadgets, sharing the shared module with widgets.
func documentedFixture() map[string]string {
	files := map[string]string{}
	for name, contents := range nestedFixture {
		files[name] = contents
	}

	files["api/handlers.go"] = `package api

import (
	"example.com/app/api/handler"
	"example.com/app/api/handler/gadgets"
	"example.com/app/api/handler/widgets"
)

type Handlers struct {
	*handler.Base
	Widgets *widgets.Handler
	Gadgets *gadgets.Handler
}
`
	files["api/routes_gadgets.go"] = `package api

func addGadgetRoutes(e *echo.Echo, h *Handlers) {
	gadgets := e.Group("/gadgets", requireAuth)
	gadgets.GET("/search", h.Gadgets.Search)
	gadgets.GET("/:id", h.Gadgets.GetGadget)
}
`
	files["api/handler/gadgets/gadgets.go"] = `package gadgets

import (
	"github.com/labstack/echo/v4"

	"example.com/app/api/handler"
	"example.com/app/models/gadget"
	"example.com/app/modules/shared"
)

type Handler struct{ *handler.Base }

func (h *Handler) Search(c echo.Context) error {
	return h.JSONResponse(c, "Gadgets.Search", nil, &shared.SearchRequest{})
}

// GetGadget returns one gadget.
//
// TODO: cache it.
func (h *Handler) GetGadget(c echo.Context) error {
	return h.JSONResponse(c, "Gadgets.GetGadget", nil, &gadget.GetRequest{})
}
`
	files["api/handler/gadgets/doc.go"] = `// Package gadgets serves the gadget catalog.
package gadgets
`
	files["api/routes_widgets.go"] = `package api

func addWidgetRoutes(e *echo.Echo, h *Handlers) {
	widgets := e.Group("/widgets", requireAuth)
	widgets.GET("", h.Widgets.ListWidgets)
	widgets.GET("/:id", h.Widgets.GetWidget)
	widgets.POST("", h.Widgets.CreateWidget)
	widgets.GET("/search", h.Widgets.Search)
}
`
	files["api/handler/widgets/search.go"] = `package widgets

import (
	"github.com/labstack/echo/v4"

	"example.com/app/modules/shared"
)

func (h *Handler) Search(c echo.Context) error {
	return h.JSONResponse(c, "Widgets.Search", nil, &shared.SearchRequest{})
}
`
	files["modules/shared/shared.go"] = `package shared

import "context"

type SearchRequest struct {
	Query string ` + "`json:\"query\"`" + `
}

type SearchResponse struct {
	Hits int64 ` + "`json:\"hits\"`" + `
}

// Search finds the catalog entries matching a query.
func Search(ctx context.Context, request *SearchRequest) (*SearchResponse, error) { return nil, nil }
`
	files["models/gadget/gadget.go"] = `package gadget

import "context"

type GetRequest struct {
	ID int64 ` + "`json:\"id\"`" + `
}

type Gadget struct {
	ID int64 ` + "`json:\"id\"`" + `
}

func Get(ctx context.Context, request *GetRequest) (*Gadget, error) { return nil, nil }
`
	files["models/widget/widget.go"] = `package widget

import (
	"github.com/uptrace/bun"
	"github.com/vanclief/compose/primitives/enums"
)

// Kind is how big a widget is.
type Kind string

const (
	// Pocket sizes
	SmallKind Kind = "small" // Fits in a pocket.
	// LargeKind needs both hands.
	LargeKind Kind = "large"
	// Odd one out
	OddKind Kind = "odd"
)

var kindSet = enums.Set([]Kind{SmallKind, LargeKind, OddKind})

// Widget is a thing the catalog sells.
//
// TODO: split the stock out.
type Widget struct {
	bun.BaseModel ` + "`bun:\"table:widgets,alias:w\"`" + `

	ID int64 ` + "`json:\"id\"`" + `
	// Name is shown to buyers.
	Name string ` + "`json:\"name\"`" + `
	Kind Kind   ` + "`json:\"kind\"`" + ` // Defaults to small.
	Note string ` + "`json:\"note,omitempty\"`" + `
}
`
	return files
}

func generateDocumented(t *testing.T, configure func(*Config)) map[string]map[string]any {
	t.Helper()

	return componentSchemas(generateDocumentedSpec(t, configure).Document)
}

func generateDocumentedSpec(t *testing.T, configure func(*Config)) *Spec {
	t.Helper()

	root := writeFixture(t, documentedFixture())
	cfg := nestedFixtureConfig(root)
	if configure != nil {
		configure(&cfg)
	}
	spec, err := Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}

	return spec
}

func TestOperationAndTagDescriptions(t *testing.T) {
	describe := func(cfg *Config) {
		cfg.DocComments = true
		cfg.TagDescriptions = map[string]string{"Widgets": "Widgets for sale."}
	}
	document := generateDocumentedSpec(t, describe).Document

	// The handler's doc comment, without maintainer notes; else the module function's.
	cases := map[string]string{
		"/gadgets/{id}":   "GetGadget returns one gadget.",
		"/gadgets/search": "Search finds the catalog entries matching a query.",
		"/widgets/{id}":   "",
	}
	for path, want := range cases {
		op := operation(t, document, path, "get")
		got, _ := op["description"].(string)
		if got != want {
			t.Errorf("%s: expected description %q, got %q", path, want, got)
		}
	}

	descriptions := map[string]any{}
	for _, raw := range document["tags"].([]any) {
		tag := raw.(map[string]any)
		descriptions[tag["name"].(string)] = tag["description"]
	}
	if descriptions["Gadgets"] != "Package gadgets serves the gadget catalog." || descriptions["Widgets"] != "Widgets for sale." {
		t.Fatalf("expected the package doc and the configured description, got %v", descriptions)
	}

	plain := generateDocumentedSpec(t, nil).Document
	_, described := operation(t, plain, "/gadgets/{id}", "get")["description"]
	if described {
		t.Fatal("expected no operation description without DocComments")
	}
}

func TestBunModelTable(t *testing.T) {
	schemas := generateDocumented(t, nil)

	widget := schemas["models_widget_Widget"]
	if widget["x-table"] != "widgets" {
		t.Fatalf("expected the BaseModel tag's table, got %v", widget["x-table"])
	}
	_, hasAllOf := widget["allOf"]
	if hasAllOf || widget["type"] != "object" {
		t.Fatalf("expected BaseModel left out of the schema, got %v", widget)
	}
	_, ok := schemas["github_com_uptrace_bun_BaseModel"]
	if ok {
		t.Fatal("expected no BaseModel component")
	}
	_, ok = schemas["models_gadget_Gadget"]["x-table"]
	if ok {
		t.Fatal("expected no table on a model without BaseModel")
	}
}

func TestDocCommentsOff(t *testing.T) {
	schemas := generateDocumented(t, nil)

	for name, schema := range schemas {
		_, described := schema["description"]
		_, enumDescribed := schema["x-enum-descriptions"]
		if (described && name != "ErrorResponse" && name != "StandardError") || enumDescribed {
			t.Fatalf("expected no descriptions without DocComments, %s has %v", name, schema)
		}
	}
}

func TestDocComments(t *testing.T) {
	schemas := generateDocumented(t, func(cfg *Config) {
		cfg.DocComments = true
	})

	widget := schemas["models_widget_Widget"]
	if widget["description"] != "Widget is a thing the catalog sells." {
		t.Fatalf("expected the type doc without its TODO, got %q", widget["description"])
	}
	properties := widget["properties"].(map[string]any)
	name := properties["name"].(map[string]any)
	if name["description"] != "Name is shown to buyers." {
		t.Fatalf("expected the field doc, got %v", name)
	}
	kind := properties["kind"].(map[string]any)
	want := map[string]any{
		"allOf":       []any{map[string]any{"$ref": "#/components/schemas/models_widget_Kind"}},
		"description": "Defaults to small.",
	}
	if !reflect.DeepEqual(kind, want) {
		t.Fatalf("expected the line comment beside the $ref wrapped in allOf, got %v", kind)
	}
	_, ok := properties["id"].(map[string]any)["description"]
	if ok {
		t.Fatal("expected an undocumented field undescribed")
	}

	enum := schemas["models_widget_Kind"]
	if enum["description"] != "Kind is how big a widget is." {
		t.Fatalf("expected the enum's type doc, got %q", enum["description"])
	}
	wantDescs := []any{"Fits in a pocket.", "LargeKind needs both hands.", ""}
	if !reflect.DeepEqual(enum["x-enum-descriptions"], wantDescs) {
		t.Fatalf("expected line comments and name-led docs, not group headings, got %v", enum["x-enum-descriptions"])
	}
	wantByValue := map[string]any{"small": "Fits in a pocket.", "large": "LargeKind needs both hands."}
	if !reflect.DeepEqual(enum["x-enumDescriptions"], wantByValue) {
		t.Fatalf("expected the documented values keyed by value, got %v", enum["x-enumDescriptions"])
	}
	if !reflect.DeepEqual(enum["x-enum-varnames"], []any{"SmallKind", "LargeKind", "OddKind"}) {
		t.Fatalf("expected the constant names aligned with enum, got %v", enum["x-enum-varnames"])
	}
}

func TestSchemaTags(t *testing.T) {
	schemas := generateDocumented(t, nil)

	cases := map[string][]string{
		// A module's requests and responses: the namespace its handlers belong to.
		"modules_catalog_ListRequest":  {"Widgets"},
		"modules_catalog_ListResponse": {"Widgets"},
		// A model package named like a namespace.
		"models_widget_Widget": {"Widgets"},
		"models_widget_Kind":   {"Widgets"},
		// A request type's package wins over its name.
		"models_gadget_GetRequest": {"Gadgets"},
		// Used by two namespaces: ambiguous.
		"modules_shared_SearchRequest": nil,
		// Outside the module.
		"github_com_vanclief_compose_drivers_databases_relational_postgres_pagination_CursorResponse": nil,
		"ErrorResponse": nil,
	}
	for name, want := range cases {
		schema, ok := schemas[name]
		if !ok {
			t.Fatalf("expected schema %s, got %v", name, sortedKeys(schemas))
		}
		got, _ := schema["x-tags"].([]string)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: expected x-tags %v, got %v", name, want, schema["x-tags"])
		}
	}
}

func TestSchemaTagsHook(t *testing.T) {
	var seen []string
	schemas := generateDocumented(t, func(cfg *Config) {
		cfg.SchemaTags = func(name string, pkgPath string, derived []string) []string {
			if name == "modules_shared_SearchRequest" {
				seen = derived
				return []string{"Search"}
			}
			return nil
		}
	})

	if !reflect.DeepEqual(seen, []string{"Gadgets", "Widgets"}) {
		t.Fatalf("expected the hook to see both derived tags, got %v", seen)
	}
	if !reflect.DeepEqual(schemas["modules_shared_SearchRequest"]["x-tags"], []string{"Search"}) {
		t.Fatalf("expected the hook's tags, got %v", schemas["modules_shared_SearchRequest"]["x-tags"])
	}
	_, ok := schemas["models_widget_Widget"]["x-tags"]
	if ok {
		t.Fatal("expected the hook returning nil to drop x-tags")
	}
}

func TestSameName(t *testing.T) {
	cases := []struct {
		pkg, namespace string
		want           bool
	}{
		{"accesscontrol", "accessControl", true},
		{"widget", "widgets", true},
		{"amenity", "amenities", true},
		{"access_control", "accessControl", true},
		{"address", "address", true},
		{"widget", "gadgets", false},
		{"", "widgets", false},
	}
	for _, c := range cases {
		got := sameName(c.pkg, c.namespace)
		if got != c.want {
			t.Fatalf("sameName(%q, %q) = %v, want %v", c.pkg, c.namespace, got, c.want)
		}
	}
}

// A component a Schemas hook made a bare $ref alias is wrapped in allOf before it gets x-tags:
// OpenAPI 3.0 ignores the siblings of a $ref.
func TestSetSchemaTagsWrapsReferences(t *testing.T) {
	alias := map[string]any{"$ref": "#/components/schemas/Other"}
	setSchemaTags(alias, []string{"Widgets"})

	want := map[string]any{
		"allOf":  []any{map[string]any{"$ref": "#/components/schemas/Other"}},
		"x-tags": []string{"Widgets"},
	}
	if !reflect.DeepEqual(alias, want) {
		t.Fatalf("expected %v, got %v", want, alias)
	}
}
