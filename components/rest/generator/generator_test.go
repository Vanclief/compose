package generator

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// newTestGenerator is a generator over an empty module with the query helpers of an
// application that extends the defaults (emma's handler/base.go ones).
func newTestGenerator(t *testing.T) *generator {
	t.Helper()

	stringArray := map[string]any{
		"type":  "array",
		"items": map[string]any{"type": "string"},
	}
	getters := DefaultQueryGetters()
	getters["ParseQueryParamStrings"] = QueryGetter{Schema: stringArray, NameArg: 1}
	getters["ParseQueryParamTokens"] = QueryGetter{Schema: stringArray, NameArg: 1}
	getters["ParseQueryParamUUIDs"] = QueryGetter{
		Schema: map[string]any{
			"type":  "array",
			"items": map[string]any{"type": "string", "format": "uuid"},
		},
		NameArg: 1,
	}
	getters["ParseEnumQueryParams"] = QueryGetter{NameArg: 1, EnumArg: 2}

	cfg, err := Config{
		Root:         t.TempDir(),
		RoutesGlob:   "routes.go",
		HandlerGlob:  "handlers/*.go",
		QueryGetters: getters,
	}.withDefaults()
	if err != nil {
		t.Fatal(err)
	}

	return &generator{
		cfg:     cfg,
		builder: newTestSchemaBuilder(t, cfg.Root),
	}
}

func writeFixture(t *testing.T, files map[string]string) string {
	t.Helper()

	root := t.TempDir()
	for name, contents := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		err := os.MkdirAll(filepath.Dir(path), 0o755)
		if err != nil {
			t.Fatal(err)
		}
		err = os.WriteFile(path, []byte(contents), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	return root
}

// nestedFixture is an application with a Handlers struct of namespaces (emma's layout).
var nestedFixture = map[string]string{
	"go.mod": "module example.com/app\n\ngo 1.22\n",
	"api/handlers.go": `package api

import (
	"example.com/app/api/handler"
	"example.com/app/api/handler/widgets"
)

type Handlers struct {
	*handler.Base
	Widgets *widgets.Handler
}
`,
	"api/routes_widgets.go": `package api

func addWidgetRoutes(e *echo.Echo, h *Handlers) {
	widgets := e.Group("/widgets", requireAuth)
	widgets.GET("", h.Widgets.ListWidgets)
	widgets.GET("/:id", h.Widgets.GetWidget)
	widgets.POST("", h.Widgets.CreateWidget)
	widgets.DELETE("/:id", h.Widgets.DeleteWidget)
	widgets.GET("/:id/export", h.Widgets.ExportWidget)
	widgets.GET("/:id/details", h.Widgets.WidgetDetails)
	widgets.GET("/legacy", logDeprecated(h.Widgets.ListWidgets)) // old clients

	e.GET("/health", health)
	e.GET("/internal/stats", h.Widgets.Stats)
}
`,
	"api/handler/widgets/widgets.go": `package widgets

import (
	"github.com/labstack/echo/v4"
	"github.com/vanclief/compose/drivers/databases/relational/postgres/pagination"

	"example.com/app/api/handler"
	"example.com/app/modules/catalog"
)

type Handler struct{ *handler.Base }

func (h *Handler) ListWidgets(c echo.Context) error {
	body := &catalog.ListRequest{
		CursorRequest: pagination.CursorRequest{
			Limit:  h.GetListLimit(c, 50),
			Cursor: c.QueryParam("cursor"),
		},
		Search: c.QueryParam("search"),
	}
	return h.JSONResponse(c, "Widgets.ListWidgets", nil, body)
}

func (h *Handler) GetWidget(c echo.Context) error {
	id, err := h.GetParameterInt64(c, "id")
	if err != nil {
		return err
	}
	return h.JSONResponse(c, "Widgets.GetWidget", nil, &catalog.GetRequest{ID: id})
}

func (h *Handler) CreateWidget(c echo.Context) error {
	body := &catalog.CreateRequest{}
	return h.BindedJSONResponse(c, "Widgets.CreateWidget", nil, body)
}

func (h *Handler) DeleteWidget(c echo.Context) error {
	id, err := h.GetParameterInt64(c, "id")
	if err != nil {
		return err
	}
	return h.JSONResponse(c, "Widgets.DeleteWidget", nil, &catalog.DeleteRequest{ID: id})
}

func (h *Handler) ExportWidget(c echo.Context) error {
	_ = h.NegotiateContentType(c)
	id, err := h.GetParameterInt64(c, "id")
	if err != nil {
		return err
	}
	return h.JSONResponse(c, "Widgets.ExportWidget", nil, &catalog.GetRequest{ID: id})
}

func (h *Handler) WidgetDetails(c echo.Context) error {
	return h.GetWidget(c)
}

func (h *Handler) Stats(c echo.Context) error {
	return h.JSONResponse(c, "Widgets.Stats", nil, &catalog.StatsRequest{})
}
`,
	"modules/catalog/catalog.go": `package catalog

import (
	"context"

	"github.com/vanclief/compose/drivers/databases/relational/postgres/pagination"

	"example.com/app/models/widget"
)

type ListRequest struct {
	pagination.CursorRequest
	Search string ` + "`json:\"search\"`" + `
}

type ListResponse struct {
	pagination.CursorResponse
	Widgets []widget.Widget ` + "`json:\"widgets\"`" + `
}

func List(ctx context.Context, request *ListRequest) (*ListResponse, error) { return nil, nil }

type GetRequest struct {
	ID int64 ` + "`json:\"id\"`" + `
}

func Get(ctx context.Context, request *GetRequest) (*widget.Widget, error) { return nil, nil }

type CreateRequest struct {
	Name string ` + "`json:\"name\"`" + `
	Part *Part  ` + "`json:\"part,omitempty\"`" + `
}

type Part struct {
	SKU string ` + "`json:\"sku\"`" + `
}

func Create(ctx context.Context, request *CreateRequest) (*widget.Widget, error) { return nil, nil }

type DeleteRequest struct {
	ID int64 ` + "`json:\"id\"`" + `
}

type DeleteResponse struct {
	Deleted bool ` + "`json:\"deleted\"`" + `
}

func Delete(ctx context.Context, request *DeleteRequest) (*DeleteResponse, error) { return nil, nil }

type StatsRequest struct{}

type StatsResponse struct {
	Count int64 ` + "`json:\"count\"`" + `
}

func Stats(ctx context.Context, request *StatsRequest) (*StatsResponse, error) { return nil, nil }
`,
	"models/widget/widget.go": `package widget

import "github.com/vanclief/compose/primitives/enums"

type Kind string

const (
	SmallKind Kind = "small"
	LargeKind Kind = "large"
)

var kindSet = enums.Set([]Kind{SmallKind, LargeKind})

type Widget struct {
	ID   int64  ` + "`json:\"id\"`" + `
	Name string ` + "`json:\"name\"`" + `
	Kind Kind   ` + "`json:\"kind\"`" + `
	Note string ` + "`json:\"note,omitempty\"`" + `
}
`,
}

func nestedFixtureConfig(root string) Config {
	return Config{
		Root:                   root,
		RoutesGlob:             "api/routes_*.go",
		HandlersFile:           "api/handlers.go",
		HandlerGlob:            "api/handler/*/*.go",
		OutDir:                 "docs",
		Info:                   Info{Title: "Widgets API", Version: "1.0", Description: "Widgets."},
		Servers:                []Server{{URL: "http://localhost:{port}", Variables: map[string]string{"port": "8080"}}},
		Security:               []SecurityScheme{{Name: "ApiKeyAuth", Header: "API-KEY"}},
		RequiredPackagePrefix:  "/models/",
		Negotiate:              "NegotiateContentType",
		NegotiatedContentTypes: []string{"application/json", "text/csv"},
		DeprecatedWrapper:      "logDeprecated",
		Skip: func(method string, path string) bool {
			return path == "/health" || strings.HasPrefix(path, "/internal")
		},
		Public: func(method string, path string) bool {
			return method == "GET" && path == "/widgets"
		},
	}
}

func operation(t *testing.T, document map[string]any, path string, method string) map[string]any {
	t.Helper()

	paths := document["paths"].(map[string]any)
	item, ok := paths[path].(map[string]any)
	if !ok {
		t.Fatalf("expected path %s, got %v", path, sortedKeys(paths))
	}
	op, ok := item[method].(map[string]any)
	if !ok {
		t.Fatalf("expected %s %s, got %v", method, path, item)
	}

	return op
}

func parameterNames(op map[string]any) []string {
	params, _ := op["parameters"].([]any)
	names := []string{}
	for _, param := range params {
		names = append(names, param.(map[string]any)["name"].(string))
	}

	return names
}

func componentSchemas(document map[string]any) map[string]map[string]any {
	return document["components"].(map[string]any)["schemas"].(map[string]map[string]any)
}

func TestGenerateNestedFixture(t *testing.T) {
	root := writeFixture(t, nestedFixture)

	spec, err := Generate(nestedFixtureConfig(root))
	if err != nil {
		t.Fatal(err)
	}
	document := spec.Document

	if document["openapi"] != "3.0.3" {
		t.Fatalf("expected openapi 3.0.3, got %v", document["openapi"])
	}
	info := document["info"].(map[string]any)
	if info["title"] != "Widgets API" || info["version"] != "1.0" || info["description"] != "Widgets." {
		t.Fatalf("unexpected info %v", info)
	}
	wantSecurity := []any{map[string]any{"ApiKeyAuth": []any{}}}
	if !reflect.DeepEqual(document["security"], wantSecurity) {
		t.Fatalf("expected global security %v, got %v", wantSecurity, document["security"])
	}

	paths := document["paths"].(map[string]any)
	wantPaths := []string{"/widgets", "/widgets/{id}", "/widgets/{id}/details", "/widgets/{id}/export"}
	if !reflect.DeepEqual(sortedKeys(paths), wantPaths) {
		t.Fatalf("expected skipped and deprecated routes left out, got %v", sortedKeys(paths))
	}

	list := operation(t, document, "/widgets", "get")
	if list["operationId"] != "widgets.listWidgets" || list["summary"] != "List Widgets" {
		t.Fatalf("unexpected list operation %v", list)
	}
	if list["x-source"] != "api/routes_widgets.go:5" {
		t.Fatalf("expected the route's file:line, got %v", list["x-source"])
	}
	if !reflect.DeepEqual(list["tags"], []string{"Widgets"}) {
		t.Fatalf("expected the namespace tag, got %v", list["tags"])
	}
	if !reflect.DeepEqual(list["security"], []any{}) {
		t.Fatalf("expected the public list to opt out of security, got %v", list["security"])
	}
	// Pagination first, then the derived reads; limit and cursor are not repeated.
	if !reflect.DeepEqual(parameterNames(list), []string{"limit", "cursor", "search"}) {
		t.Fatalf("expected limit, cursor, search, got %v", parameterNames(list))
	}

	get := operation(t, document, "/widgets/{id}", "get")
	_, hasSecurity := get["security"]
	if hasSecurity {
		t.Fatalf("expected the get operation to inherit global security, got %v", get["security"])
	}
	idSchema := get["parameters"].([]any)[0].(map[string]any)["schema"]
	if !reflect.DeepEqual(idSchema, map[string]any{"type": "integer", "format": "int64"}) {
		t.Fatalf("expected the int64 id from GetParameterInt64, got %v", idSchema)
	}
	getContent := get["responses"].(map[string]any)["200"].(map[string]any)["content"].(map[string]any)
	getSchema := getContent["application/json"].(map[string]any)["schema"]
	// A pointer result is nullable; OpenAPI 3.0 ignores nullable beside a $ref, which the
	// generator emits as is (kept for byte compatibility with existing specs).
	if !reflect.DeepEqual(getSchema, map[string]any{"$ref": "#/components/schemas/models_widget_Widget", "nullable": true}) {
		t.Fatalf("expected the module function's result as response, got %v", getSchema)
	}
	errorSchema := get["responses"].(map[string]any)["default"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"]
	if !reflect.DeepEqual(errorSchema, map[string]any{"$ref": "#/components/schemas/ErrorResponse"}) {
		t.Fatalf("expected the error envelope as default response, got %v", errorSchema)
	}

	create := operation(t, document, "/widgets", "post")
	body := create["requestBody"].(map[string]any)
	if body["required"] != false {
		t.Fatalf("expected an optional request body, got %v", body)
	}
	bodySchema := body["content"].(map[string]any)["application/json"].(map[string]any)["schema"]
	if !reflect.DeepEqual(bodySchema, map[string]any{"$ref": "#/components/schemas/modules_catalog_CreateRequest"}) {
		t.Fatalf("expected the request type as body, got %v", bodySchema)
	}

	remove := operation(t, document, "/widgets/{id}", "delete")
	_, hasBody := remove["requestBody"]
	if hasBody {
		t.Fatalf("expected a DELETE without a body binder to have no body, got %v", remove["requestBody"])
	}

	export := operation(t, document, "/widgets/{id}/export", "get")
	exportContent := export["responses"].(map[string]any)["200"].(map[string]any)["content"].(map[string]any)
	if !reflect.DeepEqual(sortedKeys(exportContent), []string{"application/json", "text/csv"}) {
		t.Fatalf("expected the negotiated content types, got %v", sortedKeys(exportContent))
	}

	// WidgetDetails only returns h.GetWidget(c), so it inherits what GetWidget's body says.
	details := operation(t, document, "/widgets/{id}/details", "get")
	if details["summary"] != "Get Widget Details" {
		t.Fatalf("expected a Get prefix for a GET summary without a verb, got %v", details["summary"])
	}
	detailsContent := details["responses"].(map[string]any)["200"].(map[string]any)["content"].(map[string]any)
	detailsSchema := detailsContent["application/json"].(map[string]any)["schema"]
	if !reflect.DeepEqual(detailsSchema, getSchema) {
		t.Fatalf("expected the alias to inherit GetWidget's response, got %v", detailsSchema)
	}

	schemas := componentSchemas(document)
	widget := schemas["models_widget_Widget"]
	if !reflect.DeepEqual(widget["required"], []string{"id", "name", "kind"}) {
		t.Fatalf("expected the model's always-serialized fields required, got %v", widget["required"])
	}
	if !reflect.DeepEqual(schemas["models_widget_Kind"]["enum"], []any{"small", "large"}) {
		t.Fatalf("expected the closed set as enum, got %v", schemas["models_widget_Kind"])
	}
	_, hasCreateRequired := schemas["modules_catalog_CreateRequest"]["required"]
	if hasCreateRequired {
		t.Fatalf("expected request wrappers to require nothing, got %v", schemas["modules_catalog_CreateRequest"])
	}

	// Handler request types are kept although they travel as parameters, the skipped
	// route's ones included; the Schemas hook is where an application hides them.
	for _, name := range []string{"modules_catalog_GetRequest", "modules_catalog_StatsRequest", "modules_catalog_StatsResponse", "StandardError", "ErrorResponse", cursorRequestSchemaName} {
		_, ok := schemas[name]
		if !ok {
			t.Errorf("expected component %s, got %v", name, sortedKeys(schemas))
		}
	}

	if len(spec.Routes) != 9 {
		t.Fatalf("expected all nine parsed routes, got %d", len(spec.Routes))
	}
}

func TestGenerateIsDeterministic(t *testing.T) {
	root := writeFixture(t, nestedFixture)
	cfg := nestedFixtureConfig(root)

	first, err := Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}

	firstJSON, err := first.JSON()
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := second.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstJSON, secondJSON) {
		t.Fatal("expected two generations to encode the same JSON")
	}
	if !bytes.HasSuffix(firstJSON, []byte("}\n")) {
		t.Fatal("expected the JSON to end with a newline")
	}

	firstYAML, err := first.YAML()
	if err != nil {
		t.Fatal(err)
	}
	secondYAML, err := second.YAML()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstYAML, secondYAML) {
		t.Fatal("expected two generations to encode the same YAML")
	}

	err = os.MkdirAll(filepath.Join(root, "docs"), 0o755)
	if err != nil {
		t.Fatal(err)
	}
	err = Run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(filepath.Join(root, "docs", "openapi.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(written, firstJSON) {
		t.Fatal("expected Run to write the generated JSON")
	}
	written, err = os.ReadFile(filepath.Join(root, "docs", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(written, firstYAML) {
		t.Fatal("expected Run to write the generated YAML")
	}
	if bytes.Count(firstYAML, []byte("version: \"1.0\"")) != 1 {
		t.Fatalf("expected info.version exactly once in the YAML")
	}
}

func TestGeneratePrunesUnreachableSchemas(t *testing.T) {
	root := writeFixture(t, nestedFixture)
	cfg := nestedFixtureConfig(root)
	cfg.Schemas = func(schemas map[string]map[string]any) {
		properties := schemas["modules_catalog_CreateRequest"]["properties"].(map[string]any)
		delete(properties, "part")
		schemas["Unused"] = map[string]any{"type": "object"}
	}

	spec, err := Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}

	schemas := componentSchemas(spec.Document)
	for _, name := range []string{"modules_catalog_Part", "Unused"} {
		_, ok := schemas[name]
		if ok {
			t.Errorf("expected unreachable %s to be pruned", name)
		}
	}
	_, ok := schemas["modules_catalog_CreateRequest"]
	if !ok {
		t.Fatal("expected the request body schema to stay")
	}

	unpruned, err := Generate(nestedFixtureConfig(root))
	if err != nil {
		t.Fatal(err)
	}
	_, ok = componentSchemas(unpruned.Document)["modules_catalog_Part"]
	if !ok {
		t.Fatal("expected Part to be reached through CreateRequest without the hook")
	}
}

func TestGenerateFailsOnDanglingRefs(t *testing.T) {
	root := writeFixture(t, nestedFixture)

	cfg := nestedFixtureConfig(root)
	cfg.Schemas = func(schemas map[string]map[string]any) {
		properties := schemas["modules_catalog_CreateRequest"]["properties"].(map[string]any)
		properties["owner"] = map[string]any{"$ref": "#/components/schemas/MissingOwner"}
	}
	_, err := Generate(cfg)
	if err == nil || !strings.Contains(err.Error(), "MissingOwner") {
		t.Fatalf("expected a dangling component ref to fail, got %v", err)
	}

	cfg = nestedFixtureConfig(root)
	cfg.Schemas = func(schemas map[string]map[string]any) {
		delete(schemas, "modules_catalog_CreateRequest")
	}
	_, err = Generate(cfg)
	if err == nil || !strings.Contains(err.Error(), "modules_catalog_CreateRequest") {
		t.Fatalf("expected an operation ref to a deleted schema to fail, got %v", err)
	}

	cfg = nestedFixtureConfig(root)
	cfg.Operation = func(r Route, op map[string]any) {
		op["parameters"] = append(op["parameters"].([]any), map[string]any{
			"name":   "kind",
			"in":     "query",
			"schema": map[string]any{"$ref": "#/components/schemas/MissingKind"},
		})
	}
	_, err = Generate(cfg)
	if err == nil || !strings.Contains(err.Error(), "MissingKind") {
		t.Fatalf("expected a dangling parameter ref to fail, got %v", err)
	}
}

func TestGenerateRejectsHandlerlessRoutes(t *testing.T) {
	root := writeFixture(t, nestedFixture)
	cfg := nestedFixtureConfig(root)
	cfg.Skip = nil

	_, err := Generate(cfg)
	if err == nil {
		t.Fatal("expected a route on a bare function to fail without Skip")
	}
	if !strings.Contains(err.Error(), "routes_widgets.go:13 GET /health") {
		t.Fatalf("expected the error to list the route, got %v", err)
	}
}

func TestOperationHookParametersWinOverDerived(t *testing.T) {
	root := writeFixture(t, nestedFixture)
	cfg := nestedFixtureConfig(root)
	cfg.Operation = func(r Route, op map[string]any) {
		if r.HandlerKey() != "widgets.ListWidgets" {
			return
		}
		op["parameters"] = append(op["parameters"].([]any), map[string]any{
			"name":     "search",
			"in":       "query",
			"required": true,
			"schema":   map[string]any{"type": "string", "minLength": 2},
		})
	}

	spec, err := Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}

	list := operation(t, spec.Document, "/widgets", "get")
	if !reflect.DeepEqual(parameterNames(list), []string{"limit", "cursor", "search"}) {
		t.Fatalf("expected the hook's search to replace the derived one, got %v", parameterNames(list))
	}
	search := list["parameters"].([]any)[2].(map[string]any)
	if search["required"] != true {
		t.Fatalf("expected the hook's parameter, got %v", search)
	}
}

func TestRequestSchemaAndContentTypesHooks(t *testing.T) {
	root := writeFixture(t, nestedFixture)
	cfg := nestedFixtureConfig(root)
	cfg.RequestSchema = func(handlerKey string) (map[string]any, bool) {
		if handlerKey != "widgets.CreateWidget" {
			return nil, false
		}
		return map[string]any{"type": "object", "properties": map[string]any{"file": map[string]any{"type": "string", "format": "binary"}}}, true
	}
	cfg.ContentTypes = func(r Route) []string {
		if r.Negotiates {
			return []string{"text/csv"}
		}
		return nil
	}
	cfg.MultipartPath = func(path string) bool {
		return path == "/widgets"
	}

	spec, err := Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}

	create := operation(t, spec.Document, "/widgets", "post")
	content := create["requestBody"].(map[string]any)["content"].(map[string]any)
	multipart, ok := content["multipart/form-data"].(map[string]any)
	if !ok {
		t.Fatalf("expected a multipart body, got %v", content)
	}
	allOf := multipart["schema"].(map[string]any)["allOf"].([]any)
	if allOf[0].(map[string]any)["properties"].(map[string]any)["file"] == nil {
		t.Fatalf("expected the hook's request schema in the multipart body, got %v", allOf)
	}

	export := operation(t, spec.Document, "/widgets/{id}/export", "get")
	exportContent := export["responses"].(map[string]any)["200"].(map[string]any)["content"].(map[string]any)
	if !reflect.DeepEqual(sortedKeys(exportContent), []string{"text/csv"}) {
		t.Fatalf("expected the hook's content types, got %v", sortedKeys(exportContent))
	}
	get := operation(t, spec.Document, "/widgets/{id}", "get")
	getContent := get["responses"].(map[string]any)["200"].(map[string]any)["content"].(map[string]any)
	if !reflect.DeepEqual(sortedKeys(getContent), []string{"application/json"}) {
		t.Fatalf("expected a nil hook result to fall back to JSON, got %v", sortedKeys(getContent))
	}
}

func TestSchemaNameAliasesAndPrefixCollapse(t *testing.T) {
	builder := &schemaBuilder{
		modulePath: "example.com/app",
		nameAliases: map[string]string{
			"modules/operations/tasks": "modules_tasks",
			"modules/people/staff":     "modules_property_staff",
		},
		prefixCollapse: map[string]string{
			"api/handler/": "api/handler",
		},
	}

	cases := map[typeRef]string{
		{ImportPath: "example.com/app/modules/operations/tasks", Name: "ListRequest"}:          "modules_tasks_ListRequest",
		{ImportPath: "example.com/app/modules/operations/tasks/comments", Name: "ListRequest"}: "modules_tasks_comments_ListRequest",
		{ImportPath: "example.com/app/modules/operations/tasksets", Name: "ListRequest"}:       "modules_operations_tasksets_ListRequest",
		{ImportPath: "example.com/app/modules/people/staff", Name: "GetRequest"}:               "modules_property_staff_GetRequest",
		{ImportPath: "example.com/app/api/handler/crm", Name: "calWebhook"}:                    "api_handler_calWebhook",
		{ImportPath: "example.com/app", Name: "Root"}:                                          "root_Root",
		{ImportPath: "github.com/google/go-github/v2/x.y", Name: "Thing"}:                      "github_com_google_go_github_v2_x_y_Thing",
	}

	for ref, want := range cases {
		got := builder.schemaName(ref)
		if got != want {
			t.Errorf("schemaName(%+v) = %q, want %q", ref, got, want)
		}
	}
}

func TestParseRoutesRejectsFlatHandlerReferenceWithHandlersFile(t *testing.T) {
	root := writeFixture(t, map[string]string{
		"routes_example.go": "e.GET(\"/example\", h.List)\n",
	})

	_, err := ParseRoutes(Config{Root: root, RoutesGlob: "routes_*.go", HandlersFile: "handlers.go", HandlerGlob: "handler/*/*.go"})
	if err == nil {
		t.Fatal("expected flat handler reference to fail parsing")
	}
	if !strings.Contains(err.Error(), "flat handler reference") {
		t.Fatalf("expected flat handler error, got %v", err)
	}
}

func TestParseRoutesRejectsUnknownGroups(t *testing.T) {
	root := writeFixture(t, map[string]string{
		"routes_example.go": "api := missing.Group(\"/api\")\n",
	})

	_, err := ParseRoutes(Config{Root: root, RoutesGlob: "routes_*.go", HandlerGlob: "handler/*/*.go"})
	if err == nil || !strings.Contains(err.Error(), "unknown parent group") {
		t.Fatalf("expected an unknown parent group to fail, got %v", err)
	}
}

func TestUnwiredHandlerPackageFailsCollection(t *testing.T) {
	root := writeFixture(t, map[string]string{
		"go.mod":                     "module example.com/app\n",
		"api/handlers.go":            "package api\n\ntype Handlers struct{}\n",
		"api/handler/forgotten/f.go": "package forgotten\n\ntype Handler struct{}\n\nfunc (h *Handler) List() {}\n",
	})

	cfg := nestedFixtureConfig(root)
	cfg.HandlersFile = "api/handlers.go"
	cfg.HandlerGlob = "api/handler/*/*.go"
	cfg, err := cfg.withDefaults()
	if err != nil {
		t.Fatal(err)
	}

	g := &generator{cfg: cfg}
	err = g.loadHandlerSources()
	if err == nil {
		t.Fatal("expected unwired handler package to fail collection")
	}
	if !strings.Contains(err.Error(), "not wired") {
		t.Fatalf("expected unwired handler error, got %v", err)
	}
}

// flatFixture is an application with one Handler type and every route in one function
// (auditor3's layout).
var flatFixture = map[string]string{
	"go.mod": "module example.com/flat\n\ngo 1.22\n",
	"api/rest/server.go": `package rest

func (s *Server) Start() error {
	e := echo.New()
	h := s.handler

	e.GET("/health", health)

	projects := e.Group("/projects", s.requireAuth)
	projects.GET("", h.ListProjects)
	projects.GET("/:id", h.GetProject, s.audit)
	projects.PATCH("/:id", h.UpdateProject)
	projects.PUT("/:id", h.UpdateProject)

	snapshots := e.Group("/code_snapshots")
	snapshots.POST("/:id/refresh", h.RefreshSnapshot)

	return e.Start(":8080")
}
`,
	"api/rest/handler.go": `package rest

import (
	"github.com/labstack/echo/v4"
	"github.com/vanclief/compose/components/rest/handler"

	"example.com/flat/projects"
	"example.com/flat/snapshots"
)

type Handler struct{ *handler.BaseHandler }

func (h *Handler) ListProjects(c echo.Context) error {
	request := &projects.ListRequest{
		Limit:  h.GetListLimit(c, 50),
		Search: c.QueryParam("q"),
	}
	loc, err := h.GetQueryParamInt64(c, "loc")
	if err != nil {
		return err
	}
	request.Loc = loc
	return h.JSONResponse(c, "ListProjects", nil, request)
}

func (h *Handler) GetProject(c echo.Context) error {
	return h.JSONResponse(c, "GetProject", nil, &projects.GetRequest{ID: c.Param("id")})
}

func (h *Handler) UpdateProject(c echo.Context) error {
	request := &projects.UpdateRequest{ID: c.Param("id")}
	return h.BindedJSONResponse(c, "UpdateProject", nil, request)
}

func (h *Handler) RefreshSnapshot(c echo.Context) error {
	return h.JSONResponse(c, "RefreshSnapshot", nil, &snapshots.RefreshRequest{ID: c.Param("id")})
}
`,
	"projects/projects.go": `package projects

import "context"

type Project struct {
	ID   string ` + "`json:\"id\"`" + `
	Name string ` + "`json:\"name\"`" + `
}

type ListRequest struct {
	Limit  int    ` + "`json:\"limit\"`" + `
	Search string ` + "`json:\"search\"`" + `
	Loc    int64  ` + "`json:\"loc\"`" + `
}

type ListResponse struct {
	Projects []Project ` + "`json:\"projects\"`" + `
}

func List(ctx context.Context, request *ListRequest) (*ListResponse, error) { return nil, nil }

type GetRequest struct {
	ID string ` + "`json:\"id\"`" + `
}

func Get(ctx context.Context, request *GetRequest) (*Project, error) { return nil, nil }

type UpdateRequest struct {
	ID   string ` + "`json:\"id\"`" + `
	Name string ` + "`json:\"name\"`" + `
}

func Update(ctx context.Context, request *UpdateRequest) (*Project, error) { return nil, nil }
`,
	"snapshots/snapshots.go": `package snapshots

import "context"

type RefreshRequest struct {
	ID string ` + "`json:\"id\"`" + `
}

type RefreshResponse struct {
	Status string ` + "`json:\"status\"`" + `
}

func Refresh(ctx context.Context, request *RefreshRequest) (*RefreshResponse, error) { return nil, nil }
`,
}

func flatFixtureConfig(root string) Config {
	return Config{
		Root:        root,
		RoutesGlob:  "api/rest/server.go",
		HandlerGlob: "api/rest/*.go",
		Info:        Info{Title: "Flat API", Version: "1.0"},
		Skip: func(method string, path string) bool {
			return path == "/health"
		},
	}
}

func TestGenerateFlatFixture(t *testing.T) {
	root := writeFixture(t, flatFixture)

	spec, err := Generate(flatFixtureConfig(root))
	if err != nil {
		t.Fatal(err)
	}
	document := spec.Document

	paths := document["paths"].(map[string]any)
	wantPaths := []string{"/code_snapshots/{id}/refresh", "/projects", "/projects/{id}"}
	if !reflect.DeepEqual(sortedKeys(paths), wantPaths) {
		t.Fatalf("expected %v, got %v", wantPaths, sortedKeys(paths))
	}
	_, hasSecurity := document["security"]
	if hasSecurity {
		t.Fatalf("expected no global security without schemes, got %v", document["security"])
	}

	list := operation(t, document, "/projects", "get")
	if list["operationId"] != "projects.listProjects" || !reflect.DeepEqual(list["tags"], []string{"Projects"}) {
		t.Fatalf("expected the namespace from the group's path, got %v %v", list["operationId"], list["tags"])
	}
	// No cursor, so no pagination parameters: limit comes from GetListLimit, q from a
	// struct-literal c.QueryParam, loc from a BaseHandler helper.
	if !reflect.DeepEqual(parameterNames(list), []string{"limit", "loc", "q"}) {
		t.Fatalf("expected limit, loc, q, got %v", parameterNames(list))
	}
	params := list["parameters"].([]any)
	if !reflect.DeepEqual(params[0].(map[string]any)["schema"], map[string]any{"type": "integer", "format": "int32"}) {
		t.Fatalf("expected an int32 limit, got %v", params[0])
	}
	if !reflect.DeepEqual(params[1].(map[string]any)["schema"], map[string]any{"type": "integer", "format": "int64"}) {
		t.Fatalf("expected an int64 loc, got %v", params[1])
	}
	listResponse := list["responses"].(map[string]any)["200"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"]
	if !reflect.DeepEqual(listResponse, map[string]any{"$ref": "#/components/schemas/projects_ListResponse", "nullable": true}) {
		t.Fatalf("expected the module response, got %v", listResponse)
	}

	get := operation(t, document, "/projects/{id}", "get")
	if get["operationId"] != "projects.getProject" {
		t.Fatalf("expected a middleware argument after the handler to be ignored, got %v", get["operationId"])
	}
	idSchema := get["parameters"].([]any)[0].(map[string]any)["schema"]
	if !reflect.DeepEqual(idSchema, map[string]any{"type": "string"}) {
		t.Fatalf("expected c.Param to type the id as a string, got %v", idSchema)
	}

	patch := operation(t, document, "/projects/{id}", "patch")
	put := operation(t, document, "/projects/{id}", "put")
	if patch["operationId"] != "projects.updateProjectPatch" || put["operationId"] != "projects.updateProjectPut" {
		t.Fatalf("expected method-suffixed operationIds, got %v and %v", patch["operationId"], put["operationId"])
	}
	// The id is filled from c.Param in the request literal, so it is not part of the body.
	patchBody := patch["requestBody"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)
	patchProperties := patchBody["properties"].(map[string]any)
	if !reflect.DeepEqual(sortedKeys(patchProperties), []string{"name"}) {
		t.Fatalf("expected the body without the path-filled id, got %v", patchBody)
	}
	update := componentSchemas(document)["projects_UpdateRequest"]
	if !reflect.DeepEqual(sortedKeys(update["properties"].(map[string]any)), []string{"id", "name"}) {
		t.Fatalf("expected the component to keep its fields, got %v", update)
	}

	refresh := operation(t, document, "/code_snapshots/{id}/refresh", "post")
	if refresh["operationId"] != "code_snapshots.refreshSnapshot" || !reflect.DeepEqual(refresh["tags"], []string{"Code snapshots"}) {
		t.Fatalf("unexpected refresh operation %v", refresh)
	}
	refreshBody := refresh["requestBody"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)
	if len(refreshBody["properties"].(map[string]any)) != 0 {
		t.Fatalf("expected a body-less POST to document no body fields, got %v", refreshBody)
	}
	refreshID := refresh["parameters"].([]any)[0].(map[string]any)["schema"]
	if !reflect.DeepEqual(refreshID, map[string]any{"type": "string"}) {
		t.Fatalf("expected c.Param inside the literal to type the id, got %v", refreshID)
	}
}

func TestGenerateFlatFixtureListsBareFunctionRoutes(t *testing.T) {
	root := writeFixture(t, flatFixture)
	cfg := flatFixtureConfig(root)
	cfg.Skip = nil

	_, err := Generate(cfg)
	if err == nil {
		t.Fatal("expected the bare health function to fail without Skip")
	}
	if !strings.Contains(err.Error(), "server.go:7 GET /health") {
		t.Fatalf("expected the error to list the bare route, got %v", err)
	}
}
