package api_test

import (
	"context"
	"encoding/json"
	"flag"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
	"github.com/vanclief/ez"

	"github.com/vanclief/compose/components/rest/api"
	"github.com/vanclief/compose/components/rest/api/internal/fixture"
	"github.com/vanclief/compose/components/rest/api/openapi"
	"github.com/vanclief/compose/components/rest/apitest"
	"github.com/vanclief/compose/components/rest/requests"
	"github.com/vanclief/compose/types"
)

var update = flag.Bool("update", false, "rewrite golden files")

func decode(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	err := json.Unmarshal(raw, &out)
	require.NoError(t, err)
	return out
}

// dig walks nested maps and arrays (numeric keys index arrays).
func dig(t *testing.T, v any, keys ...string) any {
	t.Helper()
	for _, key := range keys {
		switch node := v.(type) {
		case map[string]any:
			next, ok := node[key]
			require.True(t, ok, "missing key %q", key)
			v = next
		case []any:
			found := false
			for _, item := range node {
				m, ok := item.(map[string]any)
				if ok && m["name"] == key {
					v = item
					found = true
					break
				}
			}
			require.True(t, found, "missing item named %q", key)
		default:
			t.Fatalf("cannot index %T with %q", v, key)
		}
	}
	return v
}

func TestSpec31(t *testing.T) {
	s := newServer(t)
	raw, err := s.reg.Spec(api.V31)
	require.NoError(t, err)
	doc := decode(t, raw)

	require.Equal(t, "3.1.0", doc["openapi"])
	require.Equal(t, "runs.update", dig(t, doc, "paths", "/runs/{id}", "patch", "operationId"))

	item := dig(t, doc, "paths", "/runs/{id}").(map[string]any)
	require.NotContains(t, item, "put", "hidden operation leaked")
	require.NotContains(t, dig(t, doc, "components", "schemas"), "fixture.ArchiveRequest")

	require.Equal(t, []any{map[string]any{"apiKey": []any{}}}, dig(t, doc, "paths", "/runs", "get", "security"))
	require.NotContains(t, dig(t, doc, "paths", "/health", "get"), "security")
	require.Equal(t, true, dig(t, doc, "paths", "/health", "get", "deprecated"))
	require.Equal(t, "API-KEY", dig(t, doc, "components", "securitySchemes", "apiKey", "name"))

	require.Equal(t, "#/components/schemas/handler.ErrorResponse",
		dig(t, doc, "paths", "/runs", "get", "responses", "default", "content", "application/json", "schema", "$ref"))
	require.Equal(t, "#/components/schemas/fixture.ListResponse",
		dig(t, doc, "paths", "/runs", "get", "responses", "200", "content", "application/json", "schema", "$ref"))

	list := dig(t, doc, "paths", "/runs", "get", "parameters")
	require.Equal(t, "integer", dig(t, list, "limit", "schema", "type"))
	require.Equal(t, float64(1000), dig(t, list, "limit", "schema", "maximum"))
	require.Equal(t, "query", dig(t, list, "cursor", "in"))
	require.Equal(t, "#/components/schemas/fixture.RunStatus", dig(t, list, "status", "schema", "$ref"))

	get := dig(t, doc, "paths", "/runs/{id}", "get", "parameters")
	require.Equal(t, true, dig(t, get, "id", "required"))
	require.Equal(t, "path", dig(t, get, "id", "in"))

	create := dig(t, doc, "paths", "/runs", "post")
	require.Equal(t, "header", dig(t, create, "parameters", "x-trace", "in"))
	require.Equal(t, true, dig(t, create, "requestBody", "required"))
	require.NotContains(t, dig(t, doc, "paths", "/runs/{id}", "patch", "requestBody"), "required")

	require.Equal(t, []any{"pending", "running", "completed", "failed", "cancelled"},
		dig(t, doc, "components", "schemas", "fixture.RunStatus", "enum"))
	require.Equal(t, []any{
		map[string]any{"name": "health"}, map[string]any{"name": "runs"},
	}, doc["tags"])

	// Only maps declare additionalProperties; objects never close it.
	require.NotContains(t, string(raw), `"additionalProperties": false`)
	require.Equal(t, "#/components/schemas/fixture.Config",
		dig(t, doc, "components", "schemas", "fixture.CreateRequest", "properties", "labels", "additionalProperties", "$ref"))

	// huma marks every binary string application/octet-stream (the 3.1 form).
	binary := map[string]any{"type": "string", "format": "binary", "contentMediaType": "application/octet-stream"}
	require.Equal(t, map[string]any{"application/pdf": map[string]any{"schema": binary}},
		dig(t, doc, "paths", "/runs/{id}/report", "get", "responses", "200", "content"))
	require.Equal(t, map[string]any{
		"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/fixture.ListResponse"}},
		"text/csv":         map[string]any{"schema": binary},
	}, dig(t, doc, "paths", "/runs/export", "get", "responses", "200", "content"))
	require.Equal(t, "#/components/schemas/handler.ErrorResponse",
		dig(t, doc, "paths", "/runs/{id}/report", "get", "responses", "default", "content", "application/json", "schema", "$ref"))
	require.NotContains(t, dig(t, doc, "components", "schemas"), "api.Blob")

	upload := dig(t, doc, "paths", "/runs/{id}/artifacts", "post", "requestBody").(map[string]any)
	require.Equal(t, true, upload["required"])
	formSchema := dig(t, upload, "content", "multipart/form-data", "schema")
	require.Equal(t, "object", dig(t, formSchema, "type"))
	require.Equal(t, []any{"category", "manifest"}, dig(t, formSchema, "required"))
	require.Equal(t, []any{"log", "report"}, dig(t, formSchema, "properties", "category", "enum"))
	require.Equal(t, "integer", dig(t, formSchema, "properties", "attempt", "type").([]any)[0])
	require.Equal(t, map[string]any{"type": "string", "format": "binary", "contentMediaType": "application/octet-stream", "description": "Run manifest"},
		dig(t, formSchema, "properties", "manifest"))
	require.Equal(t, map[string]any{"type": "array", "items": binary}, dig(t, formSchema, "properties", "files"))
	require.Len(t, upload["content"], 1)

	again, err := s.reg.Spec(api.V31)
	require.NoError(t, err)
	require.Equal(t, raw, again)
	require.Equal(t, raw, specOf(t, api.V31), "output differs across registries")
}

func TestSpec30(t *testing.T) {
	s := newServer(t)
	raw, err := s.reg.Spec(api.V30)
	require.NoError(t, err)
	doc := decode(t, raw)

	require.Equal(t, "3.0.3", doc["openapi"])
	require.NotContains(t, string(raw), `"null"`)
	require.NotContains(t, string(raw), "anyOf")

	props := dig(t, doc, "components", "schemas", "fixture.UpdateRequest", "properties")
	require.Equal(t, true, dig(t, props, "failure_reason", "nullable"))
	require.Equal(t, "string", dig(t, props, "failure_reason", "type"))
	require.Equal(t, map[string]any{
		"nullable": true,
		"allOf":    []any{map[string]any{"$ref": "#/components/schemas/fixture.RunStatus"}},
		"enum":     []any{"failed", "cancelled"},
	}, dig(t, props, "status"))

	again, err := s.reg.Spec(api.V30)
	require.NoError(t, err)
	require.Equal(t, raw, again)
	require.Equal(t, raw, specOf(t, api.V30), "output differs across registries")
}

// refs collects every $ref value in a decoded document.
func refs(v any, out []string) []string {
	switch node := v.(type) {
	case []any:
		for _, item := range node {
			out = refs(item, out)
		}
	case map[string]any:
		for key, child := range node {
			ref, ok := child.(string)
			if key == "$ref" && ok {
				out = append(out, ref)
				continue
			}
			out = refs(child, out)
		}
	}
	return out
}

// requireRefsResolve asserts every $ref of doc names a component of doc.
func requireRefsResolve(t *testing.T, doc map[string]any) {
	t.Helper()
	components := dig(t, doc, "components", "schemas").(map[string]any)
	all := refs(doc, nil)
	require.NotEmpty(t, all)
	for _, ref := range all {
		name, ok := strings.CutPrefix(ref, "#/components/schemas/")
		require.True(t, ok, "unexpected $ref %q", ref)
		require.Contains(t, components, name, "dangling $ref %q", ref)
	}
}

// TestSpecNamer: with a namer, the error response still points at the
// component the namer named, in both documents and the contract.
func TestSpecNamer(t *testing.T) {
	e := newEcho()
	app := &fixture.App{}
	reg := api.New(e, api.Public(), api.Namer(openapi.SegmentsNamer(2)))
	g := reg.Group("/runs")
	api.GET(g, "/:id", "get", app.Get)
	reg.Ready()

	for _, v := range []api.Version{api.V31, api.V30} {
		raw, err := reg.Spec(v)
		require.NoError(t, err)
		doc := decode(t, raw)
		requireRefsResolve(t, doc)
		require.Equal(t, "#/components/schemas/rest.handler.ErrorResponse",
			dig(t, doc, "paths", "/runs/{id}", "get", "responses", "default", "content", "application/json", "schema", "$ref"))
	}

	contract := apitest.New(t, reg)
	rec := contract.Do(t, e, "runs.get", httptest.NewRequest(http.MethodGet, "/runs/"+fixture.KnownID.String(), nil))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = contract.Do(t, e, "runs.get", httptest.NewRequest(http.MethodGet, "/runs/"+fixture.MissingID.String(), nil))
	require.Equal(t, http.StatusNotFound, rec.Code)

	fake := &fakeTB{TB: t}
	contract.AssertResponse(fake, "runs.get", http.StatusNotFound, []byte(`{"error":{"code":"not_found"}}`))
	require.Len(t, fake.errors, 1)
}

func specOf(t *testing.T, v api.Version) []byte {
	t.Helper()
	raw, err := newServer(t).reg.Spec(v)
	require.NoError(t, err)
	return raw
}

func TestSpecGolden(t *testing.T) {
	s := newServer(t)
	for name, v := range map[string]api.Version{"openapi-3.1.golden.json": api.V31, "openapi-3.0.golden.json": api.V30} {
		got, err := s.reg.Spec(v)
		require.NoError(t, err)

		path := filepath.Join("testdata", name)
		if *update {
			err = os.WriteFile(path, got, 0o644)
			require.NoError(t, err)
		}

		want, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, string(want), string(got), "run go test -update to accept the change")
	}
}

func TestOperations(t *testing.T) {
	s := newServer(t)
	ops := s.reg.Operations()
	require.Len(t, ops, 11)
	require.Equal(t, api.OperationInfo{
		Method: http.MethodPatch, Path: "/runs/:id", SpecPath: "/runs/{id}", OperationID: "runs.update", Tag: "runs",
		Produces: []string{echo.MIMEApplicationJSON},
	}, ops[3])
	require.True(t, ops[4].Hidden)
	require.Equal(t, []string{"application/pdf"}, ops[6].Produces)
	require.Equal(t, []string{echo.MIMEApplicationJSON, "text/csv"}, ops[7].Produces)
	require.True(t, ops[10].Public)
	require.False(t, ops[9].Public)
}

func TestTags(t *testing.T) {
	e := echo.New()
	reg := api.New(e)
	v1 := reg.Group("/v1")
	runs := v1.Group("/runs/:id")
	issues := runs.Group("/issues")
	api.GET[fixture.HealthRequest, fixture.HealthResponse](v1, "/health", "check", stub)
	api.GET[fixture.GetRequest, fixture.Run](runs, "", "get", stub)
	api.GET[fixture.GetRequest, fixture.Run](issues, "", "list", stub)
	root := reg.Group("")
	api.GET[fixture.HealthRequest, fixture.HealthResponse](root, "/ping", "ping", stub, api.Public())

	var ids []string
	for _, op := range reg.Operations() {
		ids = append(ids, op.OperationID)
	}
	require.Equal(t, []string{"v1.check", "runs.get", "issues.list", "ping"}, ids)
}

type paramTagRequest struct {
	requests.NoRules
	ID string `json:"-" param:"id" path:"id"`
}

// searchRequest forgot its query tags: on GET it would become a body.
type searchRequest struct {
	requests.NoRules
	Status string `json:"status"`
}

type bytesParamRequest struct {
	requests.NoRules
	Data []byte `json:"-" query:"data"`
}

type mapParamRequest struct {
	requests.NoRules
	Filter map[string]string `json:"-" query:"filter"`
}

type optionalPathRequest struct {
	requests.NoRules
	ID types.Optional[string] `json:"-" path:"id"`
}

type optionalQueryRequest struct {
	requests.NoRules
	Status types.Optional[fixture.RunStatus] `json:"-" query:"status"`
}

type optionalHeaderRequest struct {
	requests.NoRules
	Trace *types.Optional[string] `json:"-" header:"x-trace"`
}

type formOptionalRequest struct {
	requests.NoRules
	Note types.Optional[string] `json:"note"`
	File *multipart.FileHeader  `json:"-" form:"file"`
}

type runIDRequest struct {
	requests.NoRules
	RunID string `json:"-" path:"run_id"`
}

type formStringRequest struct {
	requests.NoRules
	Doc string `json:"-" form:"doc"`
}

type formNoDashRequest struct {
	requests.NoRules
	File *multipart.FileHeader `form:"file"`
}

type formUntaggedRequest struct {
	requests.NoRules
	File *multipart.FileHeader `json:"-"`
}

type formDuplicateRequest struct {
	requests.NoRules
	File  *multipart.FileHeader `json:"-" form:"file"`
	Other *multipart.FileHeader `json:"-" form:"file"`
}

type formNestedRequest struct {
	requests.NoRules
	Name   string                `json:"name"`
	Config *fixture.Config       `json:"config"`
	Labels map[string]string     `json:"labels"`
	File   *multipart.FileHeader `json:"-" form:"file"`
}

type formFileRequest struct {
	requests.NoRules
	File *multipart.FileHeader `json:"-" form:"file"`
}

type noDashRequest struct {
	requests.NoRules
	ID string `path:"id"`
}

// stub is a handler for operations whose responses a test never reads.
func stub[Res any, P requests.Body](ctx context.Context, body P) (Res, error) {
	var zero Res
	return zero, nil
}

// panicMessage runs fn and returns the message of the ez error it panics with.
func panicMessage(t *testing.T, fn func()) (msg string) {
	t.Helper()
	defer func() {
		rec := recover()
		require.NotNil(t, rec, "expected a panic")
		err, ok := rec.(error)
		require.True(t, ok, "panic value %T is not an error", rec)
		require.Equal(t, ez.EINVALID, ez.ErrorCode(err))
		msg = ez.ErrorMessage(err)
	}()
	fn()
	return ""
}

func TestRegistrationFailures(t *testing.T) {
	newGroup := func() (*api.Registry, *api.Group) {
		reg := api.New(newEcho(), api.Authenticate((&fixture.App{}).Authenticate))
		return reg, reg.Group("/runs")
	}

	_, g := newGroup()
	api.GET[fixture.HealthRequest, fixture.HealthResponse](g, "", "list", stub)
	msg := panicMessage(t, func() {
		api.POST[fixture.HealthRequest, fixture.HealthResponse](g, "/x", "list", stub)
	})
	require.Contains(t, msg, "duplicate operationId")
	require.Contains(t, msg, "runs.list")

	// The full path starts with /: Echo would serve "runs" as /runs, but the
	// document cannot publish it and the duplicate-route check would miss it.
	reg, g := newGroup()
	api.GET[fixture.HealthRequest, fixture.HealthResponse](g, "", "list", stub)
	msg = panicMessage(t, func() {
		api.GET[fixture.HealthRequest, fixture.HealthResponse](reg.Group("runs"), "", "again", stub)
	})
	require.Contains(t, msg, "api: GET runs (runs.again): path must start with /")
	msg = panicMessage(t, func() {
		api.GET[fixture.HealthRequest, fixture.HealthResponse](reg.Group(""), "", "root", stub)
	})
	require.Contains(t, msg, "path must start with /")
	api.GET[fixture.HealthRequest, fixture.HealthResponse](reg.Group(""), "/", "root", stub)

	_, g = newGroup()
	msg = panicMessage(t, func() {
		api.GET[fixture.HealthRequest, fixture.HealthResponse](g, "/:id", "get", stub)
	})
	require.Contains(t, msg, `placeholder :id has no path:"id" field`)

	_, g = newGroup()
	msg = panicMessage(t, func() {
		api.GET[fixture.GetRequest, fixture.Run](g, "/:run_id", "get", stub)
	})
	require.Contains(t, msg, `path:"id" field has no :id placeholder`)

	_, g = newGroup()
	msg = panicMessage(t, func() {
		api.GET[paramTagRequest, fixture.Run](g, "/:id", "get", stub)
	})
	require.Contains(t, msg, "param is Echo's binder tag")

	_, g = newGroup()
	msg = panicMessage(t, func() {
		api.GET[noDashRequest, fixture.Run](g, "/:id", "get", stub)
	})
	require.Contains(t, msg, `must carry json:"-"`)

	_, g = newGroup()
	msg = panicMessage(t, func() {
		api.GET[fixture.HealthRequest, fixture.HealthResponse](g, "", "bad name", stub)
	})
	require.Contains(t, msg, "operation name")

	// A wildcard in the operation path or the group prefix.
	_, g = newGroup()
	msg = panicMessage(t, func() {
		api.GET[fixture.HealthRequest, fixture.HealthResponse](g, "/w/*", "tail", stub)
	})
	require.Contains(t, msg, "wildcard paths are not supported")
	_, g = newGroup()
	msg = panicMessage(t, func() {
		api.GET[fixture.HealthRequest, fixture.HealthResponse](g.Group("/*/files"), "", "tail", stub)
	})
	require.Contains(t, msg, "wildcard paths are not supported")

	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodDelete} {
		_, g = newGroup()
		msg = panicMessage(t, func() {
			api.Handle[searchRequest, fixture.Run](g, method, "", "search", stub)
		})
		require.Contains(t, msg, method+" requests cannot have a body")
		require.Contains(t, msg, `tag them query:"..." and json:"-"`)
	}

	_, g = newGroup()
	msg = panicMessage(t, func() {
		api.GET[bytesParamRequest, fixture.Run](g, "", "get", stub)
	})
	require.Contains(t, msg, "query parameter data: type []uint8 is not supported")

	_, g = newGroup()
	msg = panicMessage(t, func() {
		api.GET[mapParamRequest, fixture.Run](g, "", "get", stub)
	})
	require.Contains(t, msg, "query parameter filter: type map[string]string is not supported")

	optionalParams := []struct {
		register func(g *api.Group)
		want     string
	}{
		{func(g *api.Group) { api.PATCH[optionalPathRequest, fixture.Run](g, "/:id", "edit", stub) }, `path parameter "id" is types.Optional[string], but types.Optional is a body field; use a pointer for an optional parameter`},
		{func(g *api.Group) { api.GET[optionalQueryRequest, fixture.Run](g, "", "list", stub) }, `query parameter "status" is types.Optional[`},
		{func(g *api.Group) { api.GET[optionalHeaderRequest, fixture.Run](g, "", "list", stub) }, `header parameter "x-trace" is *types.Optional[string], but types.Optional is a body field`},
	}
	for _, tt := range optionalParams {
		_, g = newGroup()
		msg = panicMessage(t, func() {
			tt.register(g)
		})
		require.Contains(t, msg, tt.want)
	}

	_, g = newGroup()
	api.POST[fixture.ArchiveRequest, fixture.Run](g, "/x", "archive", stub)
	msg = panicMessage(t, func() {
		api.POST[fixture.CreateRequest, fixture.Run](g, "/x", "create", stub)
	})
	require.Contains(t, msg, "duplicate route, already registered as runs.archive")

	// Echo treats routes differing only by placeholder name as one route.
	_, g = newGroup()
	api.POST[fixture.GetRequest, fixture.Run](g, "/:id", "touch", stub)
	msg = panicMessage(t, func() {
		api.POST[runIDRequest, fixture.Run](g, "/:run_id", "poke", stub)
	})
	require.Contains(t, msg, "duplicate route, already registered as runs.touch")

	// Under another method the names must still match: OpenAPI forbids
	// /runs/{id} next to /runs/{run_id}.
	_, g = newGroup()
	api.GET[fixture.GetRequest, fixture.Run](g, "/:id", "get", stub)
	msg = panicMessage(t, func() {
		api.DELETE[runIDRequest, fixture.Run](g, "/:run_id", "delete", stub)
	})
	require.Contains(t, msg, "api: DELETE /runs/:run_id (runs.delete): path /runs/:run_id names its placeholders differently from GET /runs/:id (runs.get)")

	// One placeholder name twice, across the group prefix and the path.
	_, g = newGroup()
	msg = panicMessage(t, func() {
		api.GET[fixture.GetRequest, fixture.Run](g.Group("/:id/steps"), "/:id", "get", stub)
	})
	require.Contains(t, msg, "placeholder :id appears more than once")

	// A router that does not overwrite routes refuses a method and path
	// registered outside the registry (echo.New overwrites, as in v4).
	e := echo.NewWithConfig(echo.Config{Router: echo.NewRouter(echo.RouterConfig{})})
	e.POST("/runs/x", func(c *echo.Context) error { return nil })
	g = api.New(e, api.Authenticate((&fixture.App{}).Authenticate)).Group("/runs")
	msg = panicMessage(t, func() {
		api.POST[fixture.ArchiveRequest, fixture.Run](g, "/x", "archive", stub)
	})
	require.Contains(t, msg, "api: POST /runs/x (runs.archive): echo refused the route")
	require.Contains(t, msg, "adding duplicate route (same method+path) is not allowed")

	app := &fixture.App{}
	produces := []struct {
		opts []api.Option
		blob bool
		want string
	}{
		{nil, true, "api.Blob responses need api.ContentType"},
		{[]api.Option{api.ContentType("text/csv")}, false, "api.ContentType needs a handler returning api.Blob, not fixture.ListResponse"},
		{[]api.Option{api.ContentType(echo.MIMEApplicationJSON)}, true, "ContentType cannot declare application/json"},
		{[]api.Option{api.ContentType("text/*")}, true, `ContentType media type "text/*" must be a lowercase type/subtype without parameters or wildcards`},
		{[]api.Option{api.Produces(echo.MIMEApplicationJSON, app.ExportCSV)}, false, "Produces cannot declare application/json"},
		{[]api.Option{api.Produces("text/*", app.ExportCSV)}, false, `Produces media type "text/*" must be a lowercase type/subtype without parameters or wildcards`},
		{[]api.Option{api.Produces("text/plain; charset=utf-8", app.ExportCSV)}, false, "without parameters or wildcards"},
		{[]api.Option{api.Produces("Text/CSV", app.ExportCSV)}, false, "must be a lowercase type/subtype"},
		{[]api.Option{api.Produces("text/csv", app.ExportCSV), api.Produces("text/csv", app.ExportCSV)}, false, `Produces lists "text/csv" twice`},
		{[]api.Option{api.ContentType("text/csv"), api.Produces("text/csv", app.ExportCSV)}, true, `Produces "text/csv" repeats the primary media type`},
		{[]api.Option{api.Produces("application/pdf", app.Report)}, false, `Produces "application/pdf" handler takes fixture.ReportRequest, but the operation's request type is fixture.ExportRequest`},
	}
	for _, tt := range produces {
		_, g = newGroup()
		msg = panicMessage(t, func() {
			if tt.blob {
				api.GET[fixture.ExportRequest, api.Blob](g, "/export", "export", stub, tt.opts...)
				return
			}
			api.GET[fixture.ExportRequest, fixture.ListResponse](g, "/export", "export", stub, tt.opts...)
		})
		require.Contains(t, msg, tt.want)
	}

	uploads := []struct {
		register func(g *api.Group)
		want     string
	}{
		{func(g *api.Group) { api.POST[formStringRequest, fixture.Run](g, "", "upload", stub) }, `form:"doc" field Doc must be *multipart.FileHeader or []*multipart.FileHeader`},
		{func(g *api.Group) { api.POST[formNoDashRequest, fixture.Run](g, "", "upload", stub) }, `form:"file" field File must carry json:"-"`},
		{func(g *api.Group) { api.POST[formUntaggedRequest, fixture.Run](g, "", "upload", stub) }, `file field File needs a form:"..." tag and json:"-"`},
		{func(g *api.Group) { api.POST[formDuplicateRequest, fixture.Run](g, "", "upload", stub) }, `duplicate form:"file" field Other`},
		{func(g *api.Group) { api.POST[formNestedRequest, fixture.Run](g, "", "upload", stub) }, "multipart body field Config (config): type *fixture.Config cannot be sent as a form value"},
		{func(g *api.Group) { api.PUT[formNestedRequest, fixture.Run](g, "", "upload", stub) }, "multipart body field Labels (labels): type map[string]string cannot be sent as a form value"},
		{func(g *api.Group) { api.Handle[formFileRequest, fixture.Run](g, http.MethodGet, "", "upload", stub) }, "GET requests cannot upload files"},
		{func(g *api.Group) { api.POST[formOptionalRequest, fixture.Run](g, "", "upload", stub) }, "multipart body field Note (note): types.Optional is a JSON body field; use a pointer for an optional form value"},
	}
	for _, tt := range uploads {
		_, g = newGroup()
		msg = panicMessage(t, func() {
			tt.register(g)
		})
		require.Contains(t, msg, tt.want)
	}

	reg, g = newGroup()
	api.GET[fixture.HealthRequest, fixture.HealthResponse](g, "", "list", stub)
	reg.Ready()
	msg = panicMessage(t, func() {
		api.GET[fixture.GetRequest, fixture.Run](g, "/:id", "get", stub)
	})
	require.True(t, strings.Contains(msg, "after Ready"), msg)
}

func TestOptionFailures(t *testing.T) {
	app := &fixture.App{}
	newGroup := func() *api.Group {
		return api.New(echo.New(), api.Authenticate(app.Authenticate)).Group("/runs")
	}

	tests := []struct {
		fn   func()
		want string
	}{
		{func() { api.GET[fixture.HealthRequest, fixture.HealthResponse](newGroup(), "", "check", nil) }, "api: GET /runs (runs.check): nil handler"},
		{func() {
			api.GET(newGroup(), "", "check", func(ctx context.Context, body *fixture.HealthRequest) (any, error) {
				return nil, nil
			})
		}, "response type interface {} is an interface: the handler must return a concrete type"},
		{func() { api.GET[fixture.ReportRequest, *api.Blob](newGroup(), "/:id", "report", stub) }, "the handler must return api.Blob, not *api.Blob"},
		{func() { api.Authenticate(nil) }, "api: Authenticate needs a function"},
		{func() { api.Produces[fixture.ExportRequest]("text/csv", nil) }, "api: Produces needs a function"},
		{func() { api.New(echo.New(), api.Public(), api.Authenticate(app.Authenticate)) }, "api: New: api.Public and api.Authenticate cannot both be passed to api.New"},
		{func() { newGroup().Group("/:id", api.Authenticate(app.Authenticate), api.Public()) }, "api: group /runs/:id: api.Public and api.Authenticate cannot both be passed to a group"},
		{func() { api.GET(newGroup(), "", "check", app.Health, api.Public(), api.Authenticate(app.Authenticate)) }, "api: GET /runs (runs.check): api.Public and api.Authenticate cannot both be passed to an operation"},
		{func() { newGroup().Group("/x", api.Info("x", "1")) }, "api.Info is a registry option: pass it to api.New, not to a group"},
		{func() { api.GET(newGroup(), "", "check", app.Health, api.Description("x")) }, "api.Description is a registry option: pass it to api.New, not to an operation"},
		{func() { api.New(echo.New(), api.ContentType("text/csv")) }, "api.ContentType is an operation option: pass it to the operation, not to api.New"},
		{func() { newGroup().Group("/x", api.Produces("text/csv", app.ExportCSV)) }, "api.Produces is an operation option: pass it to the operation, not to a group"},
		{func() { newGroup().Group("/x", api.Option{}) }, "zero api.Option"},
	}
	for _, tt := range tests {
		msg := panicMessage(t, tt.fn)
		require.Contains(t, msg, tt.want)
	}

	// Ready lists every protected operation without an authenticator,
	// hidden ones included.
	reg := api.New(newEcho())
	g := reg.Group("/runs")
	api.GET(g, "", "list", app.List)
	api.POST(g, "/archive", "archive", app.Archive, api.Hidden())
	api.GET(reg.Group("/health", api.Public()), "", "check", app.Health)
	msg := panicMessage(t, func() {
		reg.Ready()
	})
	require.Contains(t, msg, "api: operations without an authenticator (use api.Authenticate or api.Public): runs.list, runs.archive")
	require.NotContains(t, msg, "health.check")
}

// pairRequest is sent and returned, so its request variant is
// pairRequestInput, which collides with the response type below.
type pairRequest struct {
	requests.NoRules
	V *int `json:"v,omitempty"`
}

type pairRequestInput struct {
	W int `json:"w"`
}

func TestReadyPanics(t *testing.T) {
	reg := api.New(newEcho(), api.Authenticate((&fixture.App{}).Authenticate))
	g := reg.Group("/pairs")
	api.POST[pairRequest, pairRequest](g, "", "create", stub)
	api.GET[fixture.HealthRequest, pairRequestInput](g, "", "input", stub)

	for range 2 {
		msg := panicMessage(t, func() {
			reg.Ready()
		})
		require.Contains(t, msg, "duplicate component name")
	}
	require.Panics(t, func() {
		reg.StrictSpec()
	})
}
