package generator

import (
	"path"
	"reflect"
	"strings"
	"testing"
)

func TestNamespaceDerivation(t *testing.T) {
	tests := []struct {
		field string
		want  string
	}{
		{field: "Operations", want: "operations"},
		{field: "AccessControl", want: "accessControl"},
		{field: "Crm", want: "crm"},
	}

	for _, tt := range tests {
		t.Run(tt.field, func(t *testing.T) {
			got := lowerFirst(tt.field)
			if got != tt.want {
				t.Fatalf("expected namespace %q, got %q", tt.want, got)
			}
		})
	}
}

func TestOperationIDForRouteUsesNamespaceAndMethod(t *testing.T) {
	tests := []struct {
		route Route
		want  string
	}{
		{route: Route{Namespace: "operations", Handler: "ListTickets"}, want: "operations.listTickets"},
		{route: Route{Namespace: "users", Handler: "ListDevices"}, want: "users.listDevices"},
		{route: Route{Namespace: "finance", Handler: "GetInvoiceReceipt"}, want: "finance.getInvoiceReceipt"},
		{route: Route{Namespace: "", Handler: "Health", flat: true}, want: "health"},
		{route: Route{Namespace: "", Handler: "Health"}, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := operationIDForRoute(tt.route)
			if got != tt.want {
				t.Fatalf("expected operationId %q, got %q", tt.want, got)
			}
		})
	}
}

func TestHandlerKeyIsNamespacedOnlyInNestedMode(t *testing.T) {
	nested := Route{Namespace: "widgets", Handler: "List"}
	if nested.HandlerKey() != "widgets.List" {
		t.Fatalf("expected nested key widgets.List, got %q", nested.HandlerKey())
	}

	flat := Route{Namespace: "widgets", Handler: "List", flat: true}
	if flat.HandlerKey() != "List" {
		t.Fatalf("expected flat key List, got %q", flat.HandlerKey())
	}
}

func TestHandlerSummaryAddsNamespaceForBareMethod(t *testing.T) {
	got := handlerSummary("List", "packages", "GET", DefaultSummaryVerbs)
	if got != "List packages" {
		t.Fatalf("expected bare method summary to include namespace, got %q", got)
	}

	got = handlerSummary("Widget", "catalog", "GET", DefaultSummaryVerbs)
	if got != "Get Widget catalog" {
		t.Fatalf("expected a GET summary without a verb to get one, got %q", got)
	}
}

func TestTagForNamespace(t *testing.T) {
	g := &generator{cfg: Config{TagNames: map[string]string{"crm": "CRM"}}}

	cases := map[string]string{
		"":               "API",
		"crm":            "CRM",
		"accessControl":  "Access Control",
		"code_snapshots": "Code snapshots",
	}
	for namespace, want := range cases {
		got := g.tagForNamespace(namespace)
		if got != want {
			t.Errorf("tagForNamespace(%q) = %q, want %q", namespace, got, want)
		}
	}
}

func TestAssignOperationIDsRejectsHandlerlessRoute(t *testing.T) {
	g := &generator{}

	_, err := g.assignOperationIDs([]Route{{Method: "GET", Path: "/unregistered-route"}})
	if err == nil {
		t.Fatal("expected handlerless route to fail validation")
	}
}

func TestAssignOperationIDsSuffixesMethodsSharingAHandler(t *testing.T) {
	g := &generator{}

	ids, err := g.assignOperationIDs([]Route{
		{Method: "PATCH", Path: "/projects/:id", Namespace: "projects", Handler: "UpdateProject", flat: true},
		{Method: "PUT", Path: "/projects/:id", Namespace: "projects", Handler: "UpdateProject", flat: true},
		{Method: "GET", Path: "/projects/:id", Namespace: "projects", Handler: "GetProject", flat: true},
	})
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]string{
		"PATCH /projects/:id": "projects.updateProjectPatch",
		"PUT /projects/:id":   "projects.updateProjectPut",
		"GET /projects/:id":   "projects.getProject",
	}
	for key, id := range want {
		if ids[key] != id {
			t.Errorf("expected %s to be %q, got %q", key, id, ids[key])
		}
	}

	_, err = g.assignOperationIDs([]Route{
		{Method: "GET", Path: "/tutorials", Namespace: "platform", Handler: "ListTutorials"},
		{Method: "GET", Path: "/platform/tutorials", Namespace: "platform", Handler: "ListTutorials"},
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate operationId") {
		t.Fatalf("expected one handler on two paths to fail, got %v", err)
	}

	_, err = g.assignOperationIDs([]Route{
		{Method: "GET", Path: "/tutorials", Namespace: "platform", Handler: "ListTutorials", Deprecated: true},
		{Method: "GET", Path: "/platform/tutorials", Namespace: "platform", Handler: "ListTutorials"},
	})
	if err != nil {
		t.Fatalf("expected the deprecated alias to be left out of operationId validation, got %v", err)
	}
}

func TestToOpenAPIPath(t *testing.T) {
	cases := map[string]string{
		"/units/:id":                "/units/{id}",
		"/complexes/:complex_id/x":  "/complexes/{complex_id}/x",
		"/files/*":                  "/files/{path}",
		"/health":                   "/health",
		"/a/:a_id/b/:b_id/download": "/a/{a_id}/b/{b_id}/download",
	}
	for echoPath, want := range cases {
		got := toOpenAPIPath(echoPath)
		if got != want {
			t.Errorf("toOpenAPIPath(%q) = %q, want %q", echoPath, got, want)
		}
	}
}

func TestPathParameterSchemaDefaultsIDsToIntegers(t *testing.T) {
	params := buildPathParameters("/complexes/{complex_id}/units/{id}/{slug}", map[string]map[string]any{
		"id": {"type": "string", "format": "uuid"},
	})
	if len(params) != 3 {
		t.Fatalf("expected three path parameters, got %v", params)
	}

	complexID := params[0].(map[string]any)["schema"].(map[string]any)
	if complexID["type"] != "integer" || complexID["format"] != "int64" {
		t.Fatalf("expected an _id parameter to default to int64, got %v", complexID)
	}
	id := params[1].(map[string]any)["schema"].(map[string]any)
	if id["format"] != "uuid" {
		t.Fatalf("expected the harvested getter schema to win, got %v", id)
	}
	slug := params[2].(map[string]any)["schema"].(map[string]any)
	if slug["type"] != "string" {
		t.Fatalf("expected other parameters to default to strings, got %v", slug)
	}
}

func TestCursorRequestSchemaLeavesOutServerSideFilters(t *testing.T) {
	builder := &schemaBuilder{schemas: map[string]map[string]any{}}

	ref := builder.schemaRef(typeRef{
		ImportPath: "github.com/vanclief/compose/drivers/databases/relational/postgres/pagination",
		Name:       "CursorRequest",
	})
	if ref["$ref"] != schemaRefPrefix+"github_com_vanclief_compose_drivers_databases_relational_postgres_pagination_CursorRequest" {
		t.Fatalf("expected a $ref to the CursorRequest component, got %v", ref)
	}

	properties := builder.schemas["github_com_vanclief_compose_drivers_databases_relational_postgres_pagination_CursorRequest"]["properties"].(map[string]any)
	_, hasFilters := properties["filters"]
	if hasFilters || len(properties) != 2 {
		t.Fatalf("expected only limit and cursor, got %v", properties)
	}
	if len(builder.schemas) != 1 {
		t.Fatalf("expected no query.Filter component, got %v", sortedKeys(builder.schemas))
	}
}

// A custom namer names every component; the generator's own lookups (pagination, models,
// resources) follow the Go types, not the names.
func TestSchemaNameHook(t *testing.T) {
	root := writeFixture(t, nestedFixture)
	cfg := nestedFixtureConfig(root)
	cfg.SchemaName = func(importPath string, typeName string) string {
		return path.Base(importPath) + typeName
	}

	spec, err := Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	schemas := componentSchemas(spec.Document)

	for _, name := range []string{"widgetWidget", "widgetKind", "catalogListRequest", "paginationCursorRequest", "paginationCursorResponse"} {
		_, ok := schemas[name]
		if !ok {
			t.Fatalf("expected component %s, got %v", name, sortedKeys(schemas))
		}
	}
	if !reflect.DeepEqual(schemas["widgetWidget"]["required"], []string{"id", "name", "kind"}) {
		t.Fatalf("expected the model's required list, got %v", schemas["widgetWidget"])
	}
	list := operation(t, spec.Document, "/widgets", "get")
	if !reflect.DeepEqual(parameterNames(list), []string{"limit", "cursor", "search"}) {
		t.Fatalf("expected the pagination parameters through the renamed CursorRequest, got %v", parameterNames(list))
	}
}

func TestSchemaNameCollisionsFail(t *testing.T) {
	root := writeFixture(t, nestedFixture)
	cfg := nestedFixtureConfig(root)
	cfg.SchemaName = func(importPath string, typeName string) string {
		switch typeName {
		case "Widget":
			return "ErrorResponse"
		case "GetRequest", "DeleteRequest":
			return "Request"
		}
		return typeName
	}

	_, err := Generate(cfg)
	if err == nil {
		t.Fatal("expected colliding schema names to fail")
	}
	for _, want := range []string{
		"component Request names both example.com/app/modules/catalog.",
		"component ErrorResponse names both example.com/app/models/widget.Widget and compose's error envelope",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("expected %q in %v", want, err)
		}
	}
}
