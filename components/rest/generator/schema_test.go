package generator

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSchemaForStructRequiresAlwaysSerializedModelFields(t *testing.T) {
	src := "package models\n" +
		"type Entity struct {\n" +
		"\tID int64 `json:\"id\"`\n" +
		"\tName string `json:\"name\"`\n" +
		"\tActive bool `json:\"active\"`\n" +
		"\tNote string `json:\"note,omitempty\"`\n" +
		"\tOwner *string `json:\"owner\"`\n" +
		"\tTags []string `json:\"tags\"`\n" +
		"\tMeta map[string]string `json:\"meta\"`\n" +
		"\tSkipped string `json:\"-\"`\n" +
		"}\n"

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "entity.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("failed to parse fixture: %v", err)
	}

	var structType *ast.StructType
	ast.Inspect(file, func(node ast.Node) bool {
		spec, ok := node.(*ast.TypeSpec)
		if ok {
			structType, _ = spec.Type.(*ast.StructType)
			return false
		}
		return true
	})
	if structType == nil {
		t.Fatal("fixture struct not found")
	}

	builder := &schemaBuilder{modulePath: "example.com/app", requiredPackagePrefix: "/application/models/"}

	// A struct in application/models: value-typed fields without omitempty are
	// required; omitempty, pointer, slice, and map fields stay optional.
	modelSchema := builder.schemaForStruct(structType, map[string]string{}, "example.com/app/application/models/entity")
	required, ok := modelSchema["required"].([]string)
	if !ok {
		t.Fatalf("expected model schema required list, got %v", modelSchema["required"])
	}
	wantRequired := []string{"id", "name", "active"}
	if len(required) != len(wantRequired) {
		t.Fatalf("expected required %v, got %v", wantRequired, required)
	}
	for i, field := range wantRequired {
		if required[i] != field {
			t.Fatalf("expected required[%d] = %s, got %s", i, field, required[i])
		}
	}

	// The same struct outside application/models (a module wrapper) follows the same rule;
	// splitRequestSide strips it again where a request reaches the type.
	wrapperSchema := builder.schemaForStruct(structType, map[string]string{}, "example.com/app/application/modules/entities")
	if !reflect.DeepEqual(wrapperSchema["required"], []string{"id", "name", "active"}) {
		t.Fatalf("expected the same required list outside models packages, got %v", wrapperSchema["required"])
	}
	if !builder.isDerived(wrapperSchema["required"]) {
		t.Fatal("expected the list recorded as derived")
	}
}

// A Go "Deprecated:" paragraph keeps the field in the contract, flagged, and still required on a
// model because the server still sends it; a looser mention of deprecation (a TODO) changes
// nothing, the field is served and documented like any other.
func TestDeprecatedFieldsAreFlaggedNeverDropped(t *testing.T) {
	src := "package entity\n" +
		"type Entity struct {\n" +
		"\t// Label mirrors the category's name.\n" +
		"\t//\n" +
		"\t// Deprecated: read category_id.\n" +
		"\tLabel string `json:\"label\"`\n" +
		"\tOld string `json:\"old\"` // TODO: Deprecate\n" +
		"}\n"

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "entity.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("failed to parse fixture: %v", err)
	}

	var structType *ast.StructType
	ast.Inspect(file, func(node ast.Node) bool {
		spec, ok := node.(*ast.TypeSpec)
		if ok {
			structType, _ = spec.Type.(*ast.StructType)
			return false
		}
		return true
	})
	if structType == nil {
		t.Fatal("fixture struct not found")
	}

	builder := &schemaBuilder{modulePath: "example.com/app", requiredPackagePrefix: "/application/models/"}
	schema := builder.schemaForStruct(structType, map[string]string{}, "example.com/app/application/models/entity")

	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("expected properties, got %v", schema["properties"])
	}

	label, ok := properties["label"].(map[string]any)
	if !ok {
		t.Fatalf("expected the label property, got %v", properties)
	}
	if label["deprecated"] != true || label["type"] != "string" {
		t.Fatalf("expected label to stay a string flagged deprecated, got %v", label)
	}

	old, ok := properties["old"].(map[string]any)
	if !ok {
		t.Fatalf("expected the field with a TODO about deprecation kept, got %v", properties)
	}
	_, oldDeprecated := old["deprecated"]
	if oldDeprecated || old["type"] != "string" {
		t.Fatalf("expected old to be a plain string, got %v", old)
	}

	required, ok := schema["required"].([]string)
	if !ok || !reflect.DeepEqual(required, []string{"label", "old"}) {
		t.Fatalf("expected label and old required, got %v", schema["required"])
	}

	// OpenAPI 3.0 ignores keywords beside a $ref, so a deprecated reference is wrapped
	reference := deprecatedSchema(map[string]any{"$ref": "#/components/schemas/Owner"})
	_, hasRef := reference["$ref"]
	allOf, hasAllOf := reference["allOf"].([]any)
	if reference["deprecated"] != true || hasRef || !hasAllOf || len(allOf) != 1 {
		t.Fatalf("expected a deprecated reference wrapped in allOf, got %v", reference)
	}
}

func TestFunctionQueryParameterSchemasFromHandlerBody(t *testing.T) {
	src := "package handlers\n" +
		"func (h *Handler) List(c echo.Context) error {\n" +
		"\tsearch := c.QueryParam(\"search\")\n" +
		"\tif c.QueryParam(\"unit_id\") != \"\" {\n" +
		"\t\t_, _ = h.GetQueryParamInt64(c, \"unit_id\")\n" +
		"\t}\n" +
		"\t_ = c.QueryParam(\"search\")\n" +
		"\tamenityID := int64(h.GetNumericQueryParam(c, \"amenity_id\", 0))\n" +
		"\tclusters, _ := h.GetQueryParamInt64s(c, \"clusters\")\n" +
		"\tfrom, _ := h.GetQueryParamTime(c, \"date_from\")\n" +
		"\tids, _ := h.ParseQueryParamUUIDs(c, \"bank_account_ids\")\n" +
		"\tcategories := handler.ParseQueryParamStrings(c, \"category\")\n" +
		"\troles := handler.ParseQueryParamTokens(c, \"role\")\n" +
		"\tlimit := h.GetListLimit(c, 50)\n" +
		"\t_ = c.QueryParam(dynamicKey)\n" +
		"\t_, _, _, _, _, _, _, _ = search, amenityID, clusters, from, ids, categories, roles, limit\n" +
		"\treturn nil\n" +
		"}\n"

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "list.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("failed to parse fixture: %v", err)
	}

	var body *ast.BlockStmt
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok {
			body = fn.Body
		}
	}
	if body == nil {
		t.Fatal("fixture handler not found")
	}

	params := newTestGenerator(t).functionQueryParameterSchemas(body, newTestSchemaBuilder(t, t.TempDir()).importMap(file))

	// The literal-name reads, deduped; GetListLimit reads its fixed name "limit"
	// and the non-literal read cannot be attributed.
	wantTypes := map[string]string{
		"limit":            "integer",
		"search":           "string",
		"amenity_id":       "integer",
		"clusters":         "array",
		"date_from":        "string",
		"bank_account_ids": "array",
		"category":         "array",
		"role":             "array",
		// Read by c.QueryParam first, then by a typed getter: the typed one wins.
		"unit_id": "integer",
	}
	if len(params) != len(wantTypes) {
		t.Fatalf("expected %d params, got %v", len(wantTypes), params)
	}
	for name, wantType := range wantTypes {
		schema, ok := params[name]
		if !ok {
			t.Fatalf("expected param %s, got %v", name, params)
		}
		if schema["type"] != wantType {
			t.Fatalf("expected %s type %s, got %v", name, wantType, schema["type"])
		}
	}
	if params["date_from"]["format"] != "date-time" {
		t.Fatalf("expected date_from format date-time, got %v", params["date_from"]["format"])
	}
	clusterItems, ok := params["clusters"]["items"].(map[string]any)
	if !ok || clusterItems["format"] != "int64" {
		t.Fatalf("expected clusters int64 items, got %v", params["clusters"]["items"])
	}
	uuidItems, ok := params["bank_account_ids"]["items"].(map[string]any)
	if !ok || uuidItems["format"] != "uuid" {
		t.Fatalf("expected bank_account_ids uuid items, got %v", params["bank_account_ids"]["items"])
	}
	for _, name := range []string{"category", "role"} {
		items, ok := params[name]["items"].(map[string]any)
		if !ok || items["type"] != "string" {
			t.Fatalf("expected %s string items, got %v", name, params[name]["items"])
		}
	}

	// Merging: parameters already declared for the operation win by name (the
	// hand-curated search), path params do not block a query name, and the
	// derived remainder comes out sorted.
	existing := []any{
		map[string]any{"name": "search", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
		map[string]any{"name": "complex_id", "in": "path", "required": true},
	}
	derived := derivedQueryParameters(params, existing, nil)
	wantOrder := []string{"amenity_id", "bank_account_ids", "category", "clusters", "date_from", "limit", "role", "unit_id"}
	if len(derived) != len(wantOrder) {
		t.Fatalf("expected %d derived params, got %v", len(wantOrder), derived)
	}
	for i, wantName := range wantOrder {
		param, ok := derived[i].(map[string]any)
		if !ok || param["name"] != wantName || param["in"] != "query" || param["required"] != false {
			t.Fatalf("expected derived[%d] = optional query param %s, got %v", i, wantName, derived[i])
		}
	}
}

func TestFunctionQueryParameterSchemasDeriveEnumArrays(t *testing.T) {
	src := "package users\n" +
		"import (\n" +
		"\t\"example.com/app/application/interfaces/rest/handler\"\n" +
		"\t\"example.com/app/application/models/user\"\n" +
		")\n" +
		"func (h *Handler) ListNotifications(c echo.Context) error {\n" +
		"\ttypes, _ := handler.ParseEnumQueryParams(c, \"types\", user.NotificationTypeFromString)\n" +
		"\tstatuses, _ := handler.ParseEnumQueryParams(c, \"statuses\", user.NotificationStatusFromString)\n" +
		"\t_, _ = handler.ParseEnumQueryParams(c, dynamicKey, user.NotificationTypeFromString)\n" +
		"\t_, _ = handler.ParseEnumQueryParams(c, \"orphan\", localParse)\n" +
		"\t_, _ = types, statuses\n" +
		"\treturn nil\n" +
		"}\n"

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "notifications.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("failed to parse fixture: %v", err)
	}

	var body *ast.BlockStmt
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok {
			body = fn.Body
		}
	}
	if body == nil {
		t.Fatal("fixture handler not found")
	}

	params := newTestGenerator(t).functionQueryParameterSchemas(body, newTestSchemaBuilder(t, t.TempDir()).importMap(file))

	// The enum type is named by the parse-function argument; a non-literal name
	// and a parse function that is not an imported <Type>FromString are skipped.
	wantRefs := map[string]string{
		"types":    "#/components/schemas/application_models_user_NotificationType",
		"statuses": "#/components/schemas/application_models_user_NotificationStatus",
	}
	if len(params) != len(wantRefs) {
		t.Fatalf("expected %d params, got %v", len(wantRefs), params)
	}
	for name, wantRef := range wantRefs {
		schema, ok := params[name]
		if !ok {
			t.Fatalf("expected param %s, got %v", name, params)
		}
		if schema["type"] != "array" {
			t.Fatalf("expected %s to be an array, got %v", name, schema["type"])
		}
		items, ok := schema["items"].(map[string]any)
		if !ok || items["$ref"] != wantRef {
			t.Fatalf("expected %s items ref %s, got %v", name, wantRef, schema["items"])
		}
	}
}

func newTestSchemaBuilder(t *testing.T, repoRoot string) *schemaBuilder {
	t.Helper()

	return &schemaBuilder{
		requiredPackagePrefix: "/application/models/",
		modulePath:            "example.com/app",
		moduleVersions:        map[string]string{"example.com/dep": "v1.2.3"},
		repoRoot:              repoRoot,
		modCache:              t.TempDir(),
		packages:              map[string]*packageInfo{},
		schemas:               map[string]map[string]any{},
		building:              map[typeRef]bool{},
	}
}

func TestBuildSchemaFailsWhenDependencySourceIsMissing(t *testing.T) {
	builder := newTestSchemaBuilder(t, t.TempDir())

	builder.schemaRef(typeRef{ImportPath: "example.com/dep/models", Name: "Thing"})

	err := builder.unresolvedError()
	if err == nil {
		t.Fatalf("expected an error for a dependency missing from the module cache")
	}
	if !strings.Contains(err.Error(), "example.com/dep/models.Thing") || !strings.Contains(err.Error(), "go mod download") {
		t.Fatalf("expected the error to name the type and the fix, got %q", err.Error())
	}
}

func TestBuildSchemaFailsWhenTypeIsNotDeclared(t *testing.T) {
	repoRoot := t.TempDir()
	pkgDir := filepath.Join(repoRoot, "application", "models", "thing")
	err := os.MkdirAll(pkgDir, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(filepath.Join(pkgDir, "thing.go"), []byte("package thing\n\ntype Thing struct {\n\tID int64 `json:\"id\"`\n}\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	builder := newTestSchemaBuilder(t, repoRoot)
	importPath := "example.com/app/application/models/thing"

	builder.schemaRef(typeRef{ImportPath: importPath, Name: "Thing"})
	err = builder.unresolvedError()
	if err != nil {
		t.Fatalf("expected the declared type to resolve, got %v", err)
	}

	builder.schemaRef(typeRef{ImportPath: importPath, Name: "Missing"})
	err = builder.unresolvedError()
	if err == nil || !strings.Contains(err.Error(), "thing.Missing") {
		t.Fatalf("expected an error naming thing.Missing, got %v", err)
	}
}

func TestBuildSchemaKeepsStandardLibraryTypesOpaque(t *testing.T) {
	builder := newTestSchemaBuilder(t, t.TempDir())

	builder.schemaRef(typeRef{ImportPath: "net/url", Name: "URL"})

	err := builder.unresolvedError()
	if err != nil {
		t.Fatalf("expected standard-library types to stay opaque without error, got %v", err)
	}
	schema := builder.schemas["net_url_URL"]
	if schema == nil || schema["type"] != "object" || schema["properties"] != nil {
		t.Fatalf("expected an opaque object schema, got %v", schema)
	}
}

func TestRequestTypeFromHandlerResolvesHelperResult(t *testing.T) {
	source := `package crm

import "example.com/mod/leads"

func parseBody(c echo.Context) (*leads.WebhookRequest, error) { return nil, nil }

func (h *Handler) Webhook(c echo.Context) error {
	body, err := parseBody(c)
	if err != nil {
		return err
	}
	return h.JSONResponse(c, "Crm.Webhook", request, body)
}
`
	file, err := parser.ParseFile(token.NewFileSet(), "cal.go", source, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}

	imports := newTestSchemaBuilder(t, t.TempDir()).importMap(file)
	helpers := map[string]funcInfo{}
	var handlerBody *ast.BlockStmt
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if fn.Recv == nil {
			helpers[fn.Name.Name] = funcInfo{Decl: fn, Imports: imports}
			continue
		}
		handlerBody = fn.Body
	}

	ref, ok := newTestGenerator(t).requestTypeFromHandler(handlerBody, imports, "example.com/mod/handler/crm", helpers)
	if !ok {
		t.Fatalf("expected the handler request type to resolve through the helper")
	}
	want := typeRef{ImportPath: "example.com/mod/leads", Name: "WebhookRequest"}
	if ref != want {
		t.Fatalf("expected %+v, got %+v", want, ref)
	}
}

func TestBuildSchemaDerivesEnumsFromClosedSets(t *testing.T) {
	repoRoot := t.TempDir()
	pkgDir := filepath.Join(repoRoot, "application", "models", "shape")
	err := os.MkdirAll(pkgDir, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	source := `package shape

import "github.com/vanclief/compose/primitives/enums"

type Kind string

const (
	UndefinedKind Kind = ""
	RoundKind     Kind = "round"
	SquareKind    Kind = "square"
	LEGACY_LABEL       = "untyped"
)

var kindSet = enums.Set([]Kind{UndefinedKind, RoundKind, SquareKind})

type Mode string

const (
	UndefinedMode Mode = ""
	EnabledMode   Mode = "enabled"
)

var modeSet = enums.Set([]Mode{EnabledMode})

type Stage string

const (
	FirstStage Stage = "first"
	LastStage  Stage = "last"
)

var stagePipeline = []Stage{FirstStage}

var stageSet = enums.Set(append(append([]Stage{}, stagePipeline...), LastStage))

type Slug string

const DefaultSlug Slug = "default"
`
	err = os.WriteFile(filepath.Join(pkgDir, "shape.go"), []byte(source), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	builder := newTestSchemaBuilder(t, repoRoot)
	importPath := "example.com/app/application/models/shape"

	builder.schemaRef(typeRef{ImportPath: importPath, Name: "Kind"})
	builder.schemaRef(typeRef{ImportPath: importPath, Name: "Mode"})
	builder.schemaRef(typeRef{ImportPath: importPath, Name: "Stage"})
	builder.schemaRef(typeRef{ImportPath: importPath, Name: "Slug"})
	err = builder.unresolvedError()
	if err != nil {
		t.Fatal(err)
	}

	want := map[string][]any{
		"application_models_shape_Kind":  {"", "round", "square"},
		"application_models_shape_Mode":  {"enabled"},
		"application_models_shape_Stage": {"first", "last"},
	}
	for name, values := range want {
		got, ok := builder.schemas[name]["enum"].([]any)
		if !ok || len(got) != len(values) {
			t.Fatalf("expected %s enum %v, got %v", name, values, builder.schemas[name]["enum"])
		}
		for i := range values {
			if got[i] != values[i] {
				t.Fatalf("expected %s enum %v, got %v", name, values, got)
			}
		}
	}

	// The constants' names align with enum: the accepted empty Kind keeps its name, the
	// unaccepted empty Mode is left out of both.
	wantNames := map[string][]any{
		"application_models_shape_Kind":  {"UndefinedKind", "RoundKind", "SquareKind"},
		"application_models_shape_Mode":  {"EnabledMode"},
		"application_models_shape_Stage": {"FirstStage", "LastStage"},
	}
	for name, names := range wantNames {
		if !reflect.DeepEqual(builder.schemas[name]["x-enum-varnames"], names) {
			t.Fatalf("expected %s x-enum-varnames %v, got %v", name, names, builder.schemas[name]["x-enum-varnames"])
		}
	}

	slug := builder.schemas["application_models_shape_Slug"]
	_, hasEnum := slug["enum"]
	if hasEnum || slug["type"] != "string" {
		t.Fatalf("expected a string type without enums.Set to stay an open string, got %v", slug)
	}
}

// OpenAPI 3.0 ignores the siblings of a $ref: a pointer field to a component is an allOf
// wrapper typed like the component, its array items and map values are not nullable, and
// neither is a pointer result.
func TestPointerReferencesAreWrappedNotSiblings(t *testing.T) {
	root := writeFixture(t, map[string]string{
		"go.mod": "module example.com/app\n\ngo 1.22\n",
		"api/routes.go": `package api

func addRoutes(e *echo.Echo, h *Handler) {
	e.GET("/things/:id", h.GetThing)
}
`,
		"api/handler.go": `package api

import (
	"github.com/labstack/echo/v4"

	"example.com/app/things"
)

type Handler struct{}

func (h *Handler) GetThing(c echo.Context) error {
	return h.JSONResponse(c, "GetThing", nil, &things.GetRequest{})
}
`,
		"things/things.go": `package things

import "context"

type Kind string

type Part struct {
	SKU string ` + "`json:\"sku\"`" + `
}

type Thing struct {
	Part   *Part            ` + "`json:\"part\"`" + `
	Kind   *Kind            ` + "`json:\"kind\"`" + `
	Parts  []*Part          ` + "`json:\"parts\"`" + `
	ByName map[string]*Part ` + "`json:\"by_name\"`" + `
	Note   *string          ` + "`json:\"note\"`" + `
}

type GetRequest struct{}

func Get(ctx context.Context, request *GetRequest) (*Thing, error) { return nil, nil }
`,
	})

	spec, err := Generate(Config{Root: root, RoutesGlob: "api/routes.go", HandlerGlob: "api/*.go"})
	if err != nil {
		t.Fatal(err)
	}

	partRef := map[string]any{"$ref": "#/components/schemas/things_Part"}
	properties := componentSchemas(spec.Document)["things_Thing"]["properties"].(map[string]any)
	want := map[string]any{
		"part":    map[string]any{"allOf": []any{partRef}, "nullable": true, "type": "object"},
		"kind":    map[string]any{"allOf": []any{map[string]any{"$ref": "#/components/schemas/things_Kind"}}, "nullable": true, "type": "string"},
		"parts":   map[string]any{"type": "array", "items": partRef},
		"by_name": map[string]any{"type": "object", "additionalProperties": partRef},
		"note":    map[string]any{"type": "string", "nullable": true},
	}
	if !reflect.DeepEqual(properties, want) {
		t.Fatalf("expected %v, got %v", want, properties)
	}

	get := operation(t, spec.Document, "/things/{id}", "get")
	body := get["responses"].(map[string]any)["200"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"]
	if !reflect.DeepEqual(body, map[string]any{"$ref": "#/components/schemas/things_Thing"}) {
		t.Fatalf("expected a pointer result to be a bare $ref, got %v", body)
	}
}

// The module function is the exported one returning (T, error), not a helper declared before
// it that also takes the request.
func TestResponseFunctionPrefersValueAndError(t *testing.T) {
	root := writeFixture(t, thingFixture(`package things

import "context"

type Thing struct {
	ID int64 `+"`json:\"id\"`"+`
}

type GetRequest struct{}

func valid(request *GetRequest) (bool, error) { return true, nil }

func SignatureVerified(request *GetRequest) bool { return true }

func Get(ctx context.Context, request *GetRequest) (*Thing, error) { return nil, nil }
`, nil))

	spec, err := Generate(thingConfig(root))
	if err != nil {
		t.Fatal(err)
	}

	get := operation(t, spec.Document, "/things/{id}", "get")
	body := get["responses"].(map[string]any)["200"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"]
	if !reflect.DeepEqual(body, map[string]any{"$ref": "#/components/schemas/things_Thing"}) {
		t.Fatalf("expected Get's result, got %v", body)
	}
}
