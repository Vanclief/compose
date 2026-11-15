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
	"github.com/labstack/echo/v5"

	"example.com/app/modules/invoices"
)

type Handler struct{}

// CreateInvoice issues an invoice.
func (h *Handler) CreateInvoice(c *echo.Context) error {
	return h.BindedJSONResponse(c, "CreateInvoice", nil, &invoices.CreateRequest{})
}

func (h *Handler) CreateInvoiceV2(c *echo.Context) error {
	return h.CreateInvoice(c)
}

func (h *Handler) CreateInvoiceV3(c *echo.Context) error {
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

// pathReadFixture routes GET /<name>/:id to a handler <Name> that reads id one way.
func pathReadFixture(handlers map[string]string) map[string]string {
	routes := "package api\n\nfunc addRoutes(e *echo.Echo, h *Handler) {\n"
	source := `package api

import (
	"time"

	"github.com/labstack/echo/v5"
	other "example.com/app/other"

	"example.com/app/modules/things"
)

type Handler struct{}
`
	for _, name := range sortedKeys(handlers) {
		routes += "\te.GET(\"/" + strings.ToLower(name) + "/:id\", h." + name + ")\n"
		source += "\nfunc (h *Handler) " + name + "(c *echo.Context) error {\n" + handlers[name] + "\n}\n"
	}
	routes += "}\n"

	return map[string]string{
		"go.mod":         "module example.com/app\n\ngo 1.22\n",
		"api/routes.go":  routes,
		"api/handler.go": source,
		"api/names.go":   "package api\n\nconst idParam = \"id\"\n\nconst otherParam = \"other\"\n",
		"other/other.go": "package other\n\nconst ID = \"id\"\n",
		"modules/things/things.go": `package things

import "context"

type IntRequest struct {
	ID   int64  ` + "`param:\"id\"`" + `
	Name string ` + "`json:\"name\"`" + `
}

type StringRequest struct {
	ID string ` + "`param:\"id\"`" + `
}

type NamedRequest struct {
	ID Number ` + "`param:\"id\"`" + `
}

type Number int64

type HiddenRequest struct {
	id int64 ` + "`param:\"id\"`" + `
}

type FillRequest struct {
	ID   int64
	Name string ` + "`json:\"name\"`" + `
}

type Thing struct {
	Name string ` + "`json:\"name\"`" + `
}

func GetInt(ctx context.Context, request *IntRequest) (*Thing, error) { return nil, nil }

func GetString(ctx context.Context, request *StringRequest) (*Thing, error) { return nil, nil }

func GetNamed(ctx context.Context, request *NamedRequest) (*Thing, error) { return nil, nil }

func GetHidden(ctx context.Context, request *HiddenRequest) (*Thing, error) { return nil, nil }

func Fill(ctx context.Context, request *FillRequest) (*Thing, error) { return nil, nil }
`,
	}
}

// A path parameter is documented by the read the handler makes of it: echo's PathParam[T] and
// PathParamOr[T] by T, a request bound by a BodyBinders call by its field with the param tag, and
// a ParamGetters call naming it with a string literal or a constant declared with one by the
// getter. A read the generator does not see leaves it a string. Of several reads, one other than
// a plain string wins over a plain string; of two others, a read in the body wins over a bound
// field, and the last over an earlier read in the body.
func TestPathParameterReads(t *testing.T) {
	int64Schema := map[string]any{"type": "integer", "format": "int64"}
	stringSchema := map[string]any{"type": "string"}
	cases := map[string]struct {
		body string
		want map[string]any
	}{
		"PathParamInt64":   {`id, err := echo.PathParam[int64](c, "id")`, int64Schema},
		"PathParamOrUint":  {`id, err := echo.PathParamOr[uint32](c, "id", 0)`, map[string]any{"type": "integer", "format": "int32", "minimum": 0}},
		"PathParamBool":    {`id, err := echo.PathParam[bool](c, "id")`, map[string]any{"type": "boolean"}},
		"PathParamFloat":   {`id, err := echo.PathParam[float64](c, "id")`, map[string]any{"type": "number", "format": "double"}},
		"PathParamString":  {`id, err := echo.PathParam[string](c, "id")`, stringSchema},
		"PathParamConst":   {`id, err := echo.PathParam[int64](c, idParam)`, int64Schema},
		"PathParamTime":    {`id, err := echo.PathParam[time.Time](c, "id")`, stringSchema},
		"PathParamNamed":   {`id, err := echo.PathParam[things.Number](c, "id")`, stringSchema},
		"PathParamUintptr": {`id, err := echo.PathParam[uintptr](c, "id")`, stringSchema},
		"PathParamNotEcho": {`id, err := other.PathParam[int64](c, "id")`, stringSchema},
		"PathParamOther":   {`id, err := echo.PathParam[int64](c, "other")`, stringSchema},

		"BoundInt":          {`return h.BindedJSONResponse(c, "GetInt", nil, &things.IntRequest{})`, int64Schema},
		"BoundString":       {`return h.BindedJSONResponse(c, "GetString", nil, &things.StringRequest{})`, stringSchema},
		"BoundNamed":        {`return h.BindedJSONResponse(c, "GetNamed", nil, &things.NamedRequest{})`, stringSchema},
		"BoundUnexported":   {`return h.BindedJSONResponse(c, "GetHidden", nil, &things.HiddenRequest{})`, stringSchema},
		"BoundNotBind":      {`return h.JSONResponse(c, "GetInt", nil, &things.IntRequest{})`, stringSchema},
		"BoundAndRead":      {"id := c.Param(\"id\")\n\treturn h.BindedJSONResponse(c, \"GetInt\", nil, &things.IntRequest{})", int64Schema},
		"BoundStringAndInt": {"id, err := h.GetParameterInt64(c, \"id\")\n\treturn h.BindedJSONResponse(c, \"GetString\", nil, &things.StringRequest{})", int64Schema},
		"BoundIntAndBool":   {"id, err := echo.PathParam[bool](c, \"id\")\n\treturn h.BindedJSONResponse(c, \"GetInt\", nil, &things.IntRequest{})", map[string]any{"type": "boolean"}},

		"BoundOtherBranch":  {"if true {\n\t\treturn h.JSONResponse(c, \"GetInt\", nil, &things.IntRequest{})\n\t}\n\treturn h.BindedJSONResponse(c, \"GetString\", nil, &things.Thing{})", stringSchema},
		"BoundBinderBranch": {"if true {\n\t\treturn h.JSONResponse(c, \"GetString\", nil, &things.Thing{})\n\t}\n\treturn h.BindedJSONResponse(c, \"GetInt\", nil, &things.IntRequest{})", int64Schema},

		"PathParamOrInferred": {`id, err := echo.PathParamOr(c, "id", int64(0))`, int64Schema},
		"PathParamOrUntyped":  {`id, err := echo.PathParamOr(c, "id", 0)`, stringSchema},
		"PathParamOrNamed":    {`id, err := echo.PathParamOr(c, "id", things.Number(0))`, stringSchema},

		"IntThenString": {"id, err := echo.PathParam[int64](c, \"id\")\n\t_ = c.Param(\"id\")", int64Schema},
		"StringThenInt": {"_ = c.Param(\"id\")\n\tid, err := echo.PathParam[int64](c, \"id\")", int64Schema},
		"IntThenBool":   {"id, err := echo.PathParam[int64](c, \"id\")\n\tok, err := echo.PathParam[bool](c, \"id\")", map[string]any{"type": "boolean"}},

		"ShadowImportGetter":    {"other := struct{ ID string }{\"nope\"}\n\tid, err := h.GetParameterInt64(c, other.ID)", stringSchema},
		"ShadowImportPathParam": {"other := struct{ ID string }{\"nope\"}\n\tid, err := echo.PathParam[int64](c, other.ID)", stringSchema},
		"ShadowEcho":            {"echo := fakeEcho{}\n\tid, err := echo.PathParam[int64](c, \"id\")", stringSchema},

		"GetterPackageConst":  {`id, err := h.GetParameterInt64(c, idParam)`, int64Schema},
		"GetterImportedConst": {`id, err := h.GetParameterInt64(c, other.ID)`, int64Schema},
		"GetterLocalConst":    {"const name = \"id\"\n\tid, err := h.GetParameterInt64(c, name)", int64Schema},
		"GetterOtherConst":    {`id, err := h.GetParameterInt64(c, otherParam)`, stringSchema},
		"GetterVariable":      {"name := \"id\"\n\tid, err := h.GetParameterInt64(c, name)", stringSchema},
		"GetterShadow":        {"idParam := \"other\"\n\tid, err := h.GetParameterInt64(c, idParam)", stringSchema},
		"GetterLocalShadow":   {"const idParam = \"other\"\n\tid, err := h.GetParameterInt64(c, idParam)", stringSchema},
		"GetterParameter":     {`return h.read(c, "id")`, stringSchema},
		"HelperMethod":        {`id, err := h.thingID(c)`, stringSchema},
	}
	handlers := map[string]string{}
	for name, c := range cases {
		handlers[name] = "\t" + c.body + "\n\treturn nil"
	}
	files := pathReadFixture(handlers)
	files["api/handler.go"] += `
func (h *Handler) read(c *echo.Context, name string) error {
	_, err := h.GetParameterInt64(c, name)
	return err
}

func (h *Handler) thingID(c *echo.Context) (int64, error) {
	return h.GetParameterInt64(c, "thing_id")
}
`
	root := writeFixture(t, files)

	spec, err := Generate(Config{Root: root, RoutesGlob: "api/routes.go", HandlerGlob: "api/*.go"})
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range cases {
		op := operation(t, spec.Document, "/"+strings.ToLower(name)+"/{id}", "get")
		params, _ := op["parameters"].([]any)
		var schema any
		for _, param := range params {
			paramMap := param.(map[string]any)
			if paramMap["in"] == "path" && paramMap["name"] == "id" {
				schema = paramMap["schema"]
			}
		}
		if !reflect.DeepEqual(schema, c.want) {
			t.Errorf("%s: expected id %v, got %v", name, c.want, schema)
		}
	}
}

// A request field filled from echo's PathParam[T], or from a ParamGetters call given a constant
// name, is left out of the request body's schema.
func TestPathParameterReadsLeaveTheBody(t *testing.T) {
	fill := "\trequest := &things.FillRequest{}\n\trequest.ID = id\n\treturn h.BindedJSONResponse(c, \"Fill\", nil, request)"
	handlers := map[string]string{
		"FillPathParam": "\tid, err := echo.PathParam[int64](c, \"id\")\n" + fill,
		"FillGetter":    "\tid, err := h.GetParameterInt64(c, idParam)\n" + fill,
	}
	files := pathReadFixture(handlers)
	files["api/routes.go"] = strings.ReplaceAll(files["api/routes.go"], "e.GET(", "e.PUT(")
	root := writeFixture(t, files)

	spec, err := Generate(Config{Root: root, RoutesGlob: "api/routes.go", HandlerGlob: "api/*.go"})
	if err != nil {
		t.Fatal(err)
	}
	for name := range handlers {
		op := operation(t, spec.Document, "/"+strings.ToLower(name)+"/{id}", "put")
		requestBody, ok := op["requestBody"].(map[string]any)
		if !ok {
			t.Fatalf("%s: expected a request body, got %v", name, op)
		}
		body := requestBody["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)
		component := componentNameFromSchema(body)
		if component != "" {
			body = componentSchemas(spec.Document)[component]
		}
		properties := sortedKeys(body["properties"].(map[string]any))
		if !reflect.DeepEqual(properties, []string{"name"}) {
			t.Errorf("%s: expected the body without the path-filled ID, got %v", name, properties)
		}
	}
}
