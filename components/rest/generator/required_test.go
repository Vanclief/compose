package generator

import (
	"reflect"
	"strings"
	"testing"
)

// requiredFixture has a model shared by a request body and a response, a model only
// responses carry, a module response wrapper and request types.
var requiredFixture = map[string]string{
	"go.mod": "module example.com/app\n\ngo 1.22\n",
	"api/routes.go": `package api

func addRoutes(e *echo.Echo, h *Handler) {
	e.POST("/invoices", h.CreateInvoice)
	e.GET("/invoices/:id", h.GetInvoice)
}
`,
	"api/handler.go": `package api

import (
	"github.com/labstack/echo/v4"

	"example.com/app/modules/invoices"
)

type Handler struct{}

func (h *Handler) CreateInvoice(c echo.Context) error {
	return h.BindedJSONResponse(c, "CreateInvoice", nil, &invoices.CreateRequest{})
}

func (h *Handler) GetInvoice(c echo.Context) error {
	return h.JSONResponse(c, "GetInvoice", nil, &invoices.GetRequest{})
}
`,
	"models/finance/finance.go": `package finance

type Adjustment struct {
	Percentage int64 ` + "`json:\"percentage\"`" + `
	Amount     int64 ` + "`json:\"amount\"`" + `
}

type IncomeToInvoice struct {
	AppliedAmount  int64 ` + "`json:\"applied_amount\"`" + `
	AssignmentDate int64 ` + "`json:\"assignment_date\"`" + `
}

type Invoice struct {
	Total      int64             ` + "`json:\"total\"`" + `
	Adjustment Adjustment        ` + "`json:\"adjustment\"`" + `
	Items      []IncomeToInvoice ` + "`json:\"items\"`" + `
}
`,
	"modules/invoices/invoices.go": `package invoices

import (
	"context"

	"example.com/app/models/finance"
)

type CreateRequest struct {
	Name       string                    ` + "`json:\"name\"`" + `
	Total      int64                     ` + "`json:\"total\"`" + `
	Adjustment *finance.Adjustment       ` + "`json:\"adjustment,omitempty\"`" + `
	Items      []finance.IncomeToInvoice ` + "`json:\"incomes_to_invoices\"`" + `
}

func Create(ctx context.Context, request *CreateRequest) (*finance.Invoice, error) { return nil, nil }

type GetRequest struct {
	ID int64 ` + "`json:\"id\"`" + `
}

type GetResponse struct {
	Count  int64           ` + "`json:\"count\"`" + `
	Hidden string          ` + "`json:\"hidden,omitempty\"`" + `
	Latest finance.Invoice ` + "`json:\"latest\"`" + `
}

func Get(ctx context.Context, request *GetRequest) (*GetResponse, error) { return nil, nil }
`,
}

func TestRequestSideRequired(t *testing.T) {
	root := writeFixture(t, requiredFixture)
	cfg := Config{
		Root:                  root,
		RoutesGlob:            "api/routes.go",
		HandlerGlob:           "api/*.go",
		RequiredPackagePrefix: "/models/",
		Schemas: func(schemas map[string]map[string]any) {
			// The server stamps assignment_date: readOnly keeps it required on both sides.
			properties := schemas["models_finance_IncomeToInvoice"]["properties"].(map[string]any)
			properties["assignment_date"] = map[string]any{"type": "integer", "format": "int64", "readOnly": true}
			// A hook's own required list is the hook's.
			schemas["modules_invoices_CreateRequest"]["required"] = []any{"name"}
		},
	}

	spec, err := Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	schemas := componentSchemas(spec.Document)

	cases := map[string]any{
		// Response-only model and module wrapper: always-serialized fields are required.
		"models_finance_Invoice":       []string{"total", "adjustment"},
		"modules_invoices_GetResponse": []string{"count", "latest"},
		// Shared models keep their list for responses; their request copies drop it.
		"models_finance_Adjustment":           []string{"percentage", "amount"},
		"models_finance_AdjustmentInput":      nil,
		"models_finance_IncomeToInvoice":      []string{"applied_amount", "assignment_date"},
		"models_finance_IncomeToInvoiceInput": []string{"assignment_date"},
		// Request-only types require nothing the rule derived; a hook's list stays.
		"modules_invoices_GetRequest":    nil,
		"modules_invoices_CreateRequest": []any{"name"},
	}
	for name, want := range cases {
		schema, ok := schemas[name]
		if !ok {
			t.Fatalf("expected component %s, got %v", name, sortedKeys(schemas))
		}
		got, has := schema["required"]
		if want == nil {
			if has {
				t.Errorf("%s: expected no required list, got %v", name, got)
			}
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: expected required %v, got %v", name, want, got)
		}
	}

	create := schemas["modules_invoices_CreateRequest"]["properties"].(map[string]any)
	adjustment := create["adjustment"].(map[string]any)["allOf"].([]any)[0]
	if !reflect.DeepEqual(adjustment, map[string]any{"$ref": "#/components/schemas/models_finance_AdjustmentInput"}) {
		t.Fatalf("expected the request to reference the request copy, got %v", adjustment)
	}
	items := create["incomes_to_invoices"].(map[string]any)["items"]
	if !reflect.DeepEqual(items, map[string]any{"$ref": "#/components/schemas/models_finance_IncomeToInvoiceInput"}) {
		t.Fatalf("expected the request items to reference the request copy, got %v", items)
	}
	invoice := schemas["models_finance_Invoice"]["properties"].(map[string]any)
	if !reflect.DeepEqual(invoice["adjustment"], map[string]any{"$ref": "#/components/schemas/models_finance_Adjustment"}) {
		t.Fatalf("expected the response to keep the shared component, got %v", invoice["adjustment"])
	}

	post := operation(t, spec.Document, "/invoices", "post")
	body := post["requestBody"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"]
	if !reflect.DeepEqual(body, map[string]any{"$ref": "#/components/schemas/modules_invoices_CreateRequest"}) {
		t.Fatalf("expected the request body, got %v", body)
	}
}

func TestRequestVariantCollisionFails(t *testing.T) {
	root := writeFixture(t, requiredFixture)
	cfg := Config{
		Root:                  root,
		RoutesGlob:            "api/routes.go",
		HandlerGlob:           "api/*.go",
		RequiredPackagePrefix: "/models/",
		Schemas: func(schemas map[string]map[string]any) {
			schemas["models_finance_AdjustmentInput"] = map[string]any{"type": "object"}
		},
	}

	_, err := Generate(cfg)
	if err == nil {
		t.Fatal("expected a request variant colliding with a component to fail")
	}
}

func TestCursorResponseRequiresHasNextPage(t *testing.T) {
	root := writeFixture(t, nestedFixture)

	spec, err := Generate(nestedFixtureConfig(root))
	if err != nil {
		t.Fatal(err)
	}

	cursor := componentSchemas(spec.Document)["github_com_vanclief_compose_drivers_databases_relational_postgres_pagination_CursorResponse"]
	if !reflect.DeepEqual(cursor["required"], []string{"has_next_page"}) {
		t.Fatalf("expected has_next_page required, got %v", cursor["required"])
	}
}

// A Schemas hook that extends a derived required list owns it, even when append reuses the
// list's backing array.
func TestHookExtendedRequiredListIsTheHooks(t *testing.T) {
	files := map[string]string{}
	for name, contents := range requiredFixture {
		files[name] = contents
	}
	files["modules/invoices/invoices.go"] = strings.Replace(requiredFixture["modules/invoices/invoices.go"],
		"	Name       string                    `json:\"name\"`\n",
		"	Name       string                    `json:\"name\"`\n	Note       string                    `json:\"note\"`\n", 1)
	root := writeFixture(t, files)

	spareCapacity := false
	cfg := Config{
		Root:        root,
		RoutesGlob:  "api/routes.go",
		HandlerGlob: "api/*.go",
		Schemas: func(schemas map[string]map[string]any) {
			create := schemas["modules_invoices_CreateRequest"]
			required := create["required"].([]string)
			spareCapacity = cap(required) > len(required)
			create["required"] = append(required, "adjustment")
		},
	}

	spec, err := Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !spareCapacity {
		t.Fatal("expected the derived list to have spare capacity, so append reuses its backing array")
	}

	got := componentSchemas(spec.Document)["modules_invoices_CreateRequest"]["required"]
	want := []string{"name", "note", "total", "adjustment"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expected the hook's required list %v, got %v", want, got)
	}
}

func TestIsDerivedOnlyRecognisesTheRecordedList(t *testing.T) {
	builder := &schemaBuilder{}

	list := make([]string, 0, 8)
	list = append(list, "a", "b", "c")
	builder.markDerived(list)
	single := make([]string, 0, 4)
	single = append(single, "only")
	builder.markDerived(single)

	cases := map[string]struct {
		list []string
		want bool
	}{
		"recorded":           {list, true},
		"recorded single":    {single, true},
		"extended":           {append(list, "d"), false},
		"extended single":    {append(single, "more"), false},
		"truncated":          {list[:2], false},
		"truncated to first": {list[:1], false},
		"without first":      {list[1:], false},
		"last only":          {list[2:], false},
		"copied":             {append([]string(nil), list...), false},
	}
	for name, c := range cases {
		got := builder.isDerived(c.list)
		if got != c.want {
			t.Errorf("%s: isDerived(%v) = %v, want %v", name, c.list, got, c.want)
		}
	}
}
