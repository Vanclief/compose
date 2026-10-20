package api_test

import (
	"encoding/json"
	"flag"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
	"github.com/vanclief/ez"

	"github.com/vanclief/compose/components/rest/api"
	"github.com/vanclief/compose/components/rest/api/internal/fixture"
	"github.com/vanclief/compose/components/rest/handler"
	"github.com/vanclief/compose/components/rest/requests"
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
	require.Len(t, ops, 7)
	require.Equal(t, api.OperationInfo{
		Method: http.MethodPatch, Path: "/runs/:id", SpecPath: "/runs/{id}", OperationID: "runs.update", Tag: "runs",
	}, ops[3])
	require.True(t, ops[4].Hidden)
}

func TestTags(t *testing.T) {
	e := echo.New()
	reg := api.New(e, handler.NewHandler(&fixture.App{}))
	v1 := reg.Group("/v1")
	runs := v1.Group("/runs/:id")
	issues := runs.Group("/issues")
	api.GET[fixture.HealthRequest, fixture.HealthResponse](v1, "/health", "check")
	api.GET[fixture.GetRequest, fixture.Run](runs, "", "get")
	api.GET[fixture.GetRequest, fixture.Run](issues, "", "list")
	root := reg.Group("")
	api.GET[fixture.HealthRequest, fixture.HealthResponse](root, "/ping", "ping", api.Public())

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

type runIDRequest struct {
	requests.NoRules
	RunID string `json:"-" path:"run_id"`
}

type noDashRequest struct {
	requests.NoRules
	ID string `path:"id"`
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
		reg := api.New(echo.New(), handler.NewHandler(&fixture.App{}))
		return reg, reg.Group("/runs")
	}

	_, g := newGroup()
	api.GET[fixture.HealthRequest, fixture.HealthResponse](g, "", "list")
	msg := panicMessage(t, func() {
		api.POST[fixture.HealthRequest, fixture.HealthResponse](g, "/x", "list")
	})
	require.Contains(t, msg, "duplicate operationId")
	require.Contains(t, msg, "runs.list")

	_, g = newGroup()
	msg = panicMessage(t, func() {
		api.GET[fixture.HealthRequest, fixture.HealthResponse](g, "/:id", "get")
	})
	require.Contains(t, msg, `placeholder :id has no path:"id" field`)

	_, g = newGroup()
	msg = panicMessage(t, func() {
		api.GET[fixture.GetRequest, fixture.Run](g, "/:run_id", "get")
	})
	require.Contains(t, msg, `path:"id" field has no :id placeholder`)

	_, g = newGroup()
	msg = panicMessage(t, func() {
		api.GET[paramTagRequest, fixture.Run](g, "/:id", "get")
	})
	require.Contains(t, msg, "param is Echo's binder tag")

	_, g = newGroup()
	msg = panicMessage(t, func() {
		api.GET[noDashRequest, fixture.Run](g, "/:id", "get")
	})
	require.Contains(t, msg, `must carry json:"-"`)

	_, g = newGroup()
	msg = panicMessage(t, func() {
		api.GET[fixture.HealthRequest, fixture.HealthResponse](g, "", "bad name")
	})
	require.Contains(t, msg, "operation name")

	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodDelete} {
		_, g = newGroup()
		msg = panicMessage(t, func() {
			api.Handle[searchRequest, fixture.Run](g, method, "", "search")
		})
		require.Contains(t, msg, method+" requests cannot have a body")
		require.Contains(t, msg, `tag them query:"..." and json:"-"`)
	}

	_, g = newGroup()
	msg = panicMessage(t, func() {
		api.GET[bytesParamRequest, fixture.Run](g, "", "get")
	})
	require.Contains(t, msg, "query parameter data: type []uint8 is not supported")

	_, g = newGroup()
	msg = panicMessage(t, func() {
		api.GET[mapParamRequest, fixture.Run](g, "", "get")
	})
	require.Contains(t, msg, "query parameter filter: type map[string]string is not supported")

	_, g = newGroup()
	api.POST[fixture.ArchiveRequest, fixture.Run](g, "/x", "archive")
	msg = panicMessage(t, func() {
		api.POST[fixture.CreateRequest, fixture.Run](g, "/x", "create")
	})
	require.Contains(t, msg, "duplicate route, already registered as runs.archive")

	// Echo treats routes differing only by placeholder name as one route.
	_, g = newGroup()
	api.POST[fixture.GetRequest, fixture.Run](g, "/:id", "touch")
	msg = panicMessage(t, func() {
		api.POST[runIDRequest, fixture.Run](g, "/:run_id", "poke")
	})
	require.Contains(t, msg, "duplicate route, already registered as runs.touch")

	reg, g := newGroup()
	api.GET[fixture.HealthRequest, fixture.HealthResponse](g, "", "list")
	err := reg.Ready()
	require.NoError(t, err)
	msg = panicMessage(t, func() {
		api.GET[fixture.GetRequest, fixture.Run](g, "/:id", "get")
	})
	require.True(t, strings.Contains(msg, "after Ready"), msg)
}
