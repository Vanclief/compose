package generator

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// internalFixture is nestedFixture plus an Admin namespace whose operations are all internal.
func internalFixture() map[string]string {
	files := map[string]string{}
	for name, contents := range nestedFixture {
		files[name] = contents
	}

	files["api/handlers.go"] = `package api

import (
	"example.com/app/api/handler"
	"example.com/app/api/handler/admin"
	"example.com/app/api/handler/widgets"
)

type Handlers struct {
	*handler.Base
	Widgets *widgets.Handler
	Admin   *admin.Handler
}
`
	files["api/routes_admin.go"] = `package api

func addAdminRoutes(e *echo.Echo, h *Handlers) {
	admin := e.Group("/admin", requireAuth)
	admin.GET("/widgets/:id", h.Admin.InspectWidget)
	admin.POST("/purge", h.Admin.Purge)
}
`
	files["api/handler/admin/admin.go"] = `package admin

import (
	"github.com/labstack/echo/v4"

	"example.com/app/api/handler"
	"example.com/app/modules/catalog"
)

type Handler struct{ *handler.Base }

func (h *Handler) InspectWidget(c echo.Context) error {
	id, err := h.GetParameterInt64(c, "id")
	if err != nil {
		return err
	}
	return h.JSONResponse(c, "Admin.InspectWidget", nil, &catalog.GetRequest{ID: id})
}

func (h *Handler) Purge(c echo.Context) error {
	body := &catalog.PurgeRequest{}
	return h.BindedJSONResponse(c, "Admin.Purge", nil, body)
}
`
	files["modules/catalog/purge.go"] = `package catalog

import "context"

type PurgeRequest struct {
	Before int64 ` + "`json:\"before\"`" + `
}

type PurgeResponse struct {
	Purged int64 ` + "`json:\"purged\"`" + `
}

func Purge(ctx context.Context, request *PurgeRequest) (*PurgeResponse, error) { return nil, nil }
`

	return files
}

func internalFixtureConfig(root string) Config {
	cfg := nestedFixtureConfig(root)
	cfg.Internal = func(method string, path string) bool {
		if strings.HasPrefix(path, "/admin") {
			return true
		}
		return method == "DELETE" && path == "/widgets/{id}"
	}

	return cfg
}

func TestPublicLeavesOutInternalOperations(t *testing.T) {
	root := writeFixture(t, internalFixture())

	spec, err := Generate(internalFixtureConfig(root))
	if err != nil {
		t.Fatal(err)
	}
	public, err := spec.Public()
	if err != nil {
		t.Fatal(err)
	}
	full := spec.Document

	for _, internal := range [][2]string{{"/admin/widgets/{id}", "get"}, {"/admin/purge", "post"}, {"/widgets/{id}", "delete"}} {
		op := operation(t, full, internal[0], internal[1])
		if op["x-internal"] != true {
			t.Errorf("expected %s %s marked x-internal in the full spec, got %v", internal[1], internal[0], op["x-internal"])
		}
	}
	_, marked := operation(t, full, "/widgets/{id}", "get")["x-internal"]
	if marked {
		t.Fatal("expected a public operation to carry no x-internal")
	}

	paths := public.Document["paths"].(map[string]any)
	wantPaths := []string{"/widgets", "/widgets/{id}", "/widgets/{id}/details", "/widgets/{id}/export"}
	if !reflect.DeepEqual(sortedKeys(paths), wantPaths) {
		t.Fatalf("expected the admin path items dropped from the public spec, got %v", sortedKeys(paths))
	}
	item := paths["/widgets/{id}"].(map[string]any)
	if !reflect.DeepEqual(sortedKeys(item), []string{"get"}) {
		t.Fatalf("expected only the public get on /widgets/{id}, got %v", sortedKeys(item))
	}

	wantFullTags := []any{map[string]any{"name": "Admin"}, map[string]any{"name": "Widgets"}}
	if !reflect.DeepEqual(full["tags"], wantFullTags) {
		t.Fatalf("expected Admin and Widgets tags in the full spec, got %v", full["tags"])
	}
	wantPublicTags := []any{map[string]any{"name": "Widgets"}}
	if !reflect.DeepEqual(public.Document["tags"], wantPublicTags) {
		t.Fatalf("expected the unused Admin tag dropped from the public spec, got %v", public.Document["tags"])
	}

	fullSchemas := componentSchemas(full)
	publicSchemas := componentSchemas(public.Document)
	for _, name := range []string{"modules_catalog_PurgeRequest", "modules_catalog_PurgeResponse", "modules_catalog_DeleteRequest", "modules_catalog_DeleteResponse"} {
		_, ok := fullSchemas[name]
		if !ok {
			t.Errorf("expected internal-only %s in the full spec", name)
		}
		_, ok = publicSchemas[name]
		if ok {
			t.Errorf("expected internal-only %s pruned from the public spec", name)
		}
	}
	// Shared by public and internal operations; the skipped route's types stay as well.
	for _, name := range []string{"modules_catalog_GetRequest", "models_widget_Widget", "modules_catalog_StatsRequest", "ErrorResponse"} {
		_, ok := fullSchemas[name]
		if !ok {
			t.Errorf("expected %s in the full spec", name)
		}
		_, ok = publicSchemas[name]
		if !ok {
			t.Errorf("expected %s in the public spec", name)
		}
	}

	if !reflect.DeepEqual(public.Document["info"], full["info"]) {
		t.Fatalf("expected the same info in both specs, got %v and %v", public.Document["info"], full["info"])
	}

	// Public works on a copy.
	operation(t, full, "/widgets/{id}", "delete")
	_, ok := fullSchemas["modules_catalog_PurgeRequest"]
	if !ok {
		t.Fatal("expected Public to leave the full spec untouched")
	}
}

func TestPublicFailsOnDanglingRefs(t *testing.T) {
	spec := &Spec{
		Document: map[string]any{
			"paths": map[string]any{
				"/widgets": map[string]any{
					"get": map[string]any{
						"responses": map[string]any{
							"200": map[string]any{"$ref": "#/components/schemas/MissingWidget"},
						},
					},
				},
			},
			"components": map[string]any{
				"schemas": map[string]map[string]any{},
			},
		},
	}

	_, err := spec.Public()
	if err == nil || !strings.Contains(err.Error(), "MissingWidget") {
		t.Fatalf("expected a dangling ref in the public spec to fail, got %v", err)
	}
}

func TestPublicWithoutInternalOperationsEncodesTheFullSpec(t *testing.T) {
	root := writeFixture(t, nestedFixture)
	cfg := nestedFixtureConfig(root)
	skip := cfg.Skip
	cfg.Skip = func(method string, path string) bool {
		return path == "/widgets/legacy" || skip(method, path)
	}

	spec, err := Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	public, err := spec.Public()
	if err != nil {
		t.Fatal(err)
	}

	// The public spec leaves out where operations are declared.
	for _, item := range spec.Document["paths"].(map[string]any) {
		for _, op := range item.(map[string]any) {
			delete(op.(map[string]any), "x-source")
		}
	}
	fullJSON, err := spec.JSON()
	if err != nil {
		t.Fatal(err)
	}
	publicJSON, err := public.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fullJSON, publicJSON) {
		t.Fatal("expected the public spec of a spec without internal operations to be the same document")
	}
}

func TestRunWritesPublicAndInternalSpecs(t *testing.T) {
	root := writeFixture(t, internalFixture())
	cfg := internalFixtureConfig(root)
	err := os.MkdirAll(filepath.Join(root, "docs"), 0o755)
	if err != nil {
		t.Fatal(err)
	}

	names := []string{"openapi.json", "openapi.yaml", "openapi.internal.json", "openapi.internal.yaml"}
	read := func() map[string][]byte {
		t.Helper()

		err := Run(cfg)
		if err != nil {
			t.Fatal(err)
		}
		files := map[string][]byte{}
		for _, name := range names {
			data, err := os.ReadFile(filepath.Join(root, "docs", name))
			if err != nil {
				t.Fatal(err)
			}
			files[name] = data
		}
		return files
	}

	first := read()
	second := read()
	for _, name := range names {
		if !bytes.Equal(first[name], second[name]) {
			t.Errorf("expected two runs to write the same %s", name)
		}
	}

	spec, err := Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	public, err := spec.Public()
	if err != nil {
		t.Fatal(err)
	}
	fullJSON, err := spec.JSON()
	if err != nil {
		t.Fatal(err)
	}
	publicYAML, err := public.YAML()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first["openapi.internal.json"], fullJSON) {
		t.Fatal("expected openapi.internal.json to be the full spec")
	}
	if !bytes.Equal(first["openapi.yaml"], publicYAML) {
		t.Fatal("expected openapi.yaml to be the public spec")
	}

	if bytes.Contains(first["openapi.json"], []byte("x-internal")) || bytes.Contains(first["openapi.yaml"], []byte("x-internal")) {
		t.Fatal("expected no internal operation in the public files")
	}
	if !bytes.Contains(first["openapi.internal.yaml"], []byte("x-internal: true")) {
		t.Fatal("expected the internal operations marked in openapi.internal.yaml")
	}
	for _, name := range []string{"openapi.yaml", "openapi.internal.yaml"} {
		if bytes.Count(first[name], []byte("version: \"1.0\"")) != 1 {
			t.Errorf("expected info.version exactly once in %s", name)
		}
	}
}

// The public spec leaves out deprecated routes and x-source; the info, external docs and
// security scheme descriptions come from the config.
func TestPublicSpecHygiene(t *testing.T) {
	root := writeFixture(t, nestedFixture)
	cfg := nestedFixtureConfig(root)
	cfg.Info.TermsOfService = "https://example.com/terms"
	cfg.Info.Contact = &Contact{Name: "API team", Email: "api@example.com"}
	cfg.Info.License = &License{Name: "Proprietary", URL: "https://example.com/license"}
	cfg.ExternalDocs = &ExternalDocs{Description: "Guides", URL: "https://example.com/docs"}
	cfg.Security = []SecurityScheme{{Name: "ApiKeyAuth", Header: "API-KEY", Description: "The key POST /login returns."}}

	spec, err := Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}

	info := spec.Document["info"].(map[string]any)
	if info["termsOfService"] != "https://example.com/terms" ||
		!reflect.DeepEqual(info["contact"], map[string]any{"name": "API team", "email": "api@example.com"}) ||
		!reflect.DeepEqual(info["license"], map[string]any{"name": "Proprietary", "url": "https://example.com/license"}) {
		t.Fatalf("unexpected info %v", info)
	}
	if !reflect.DeepEqual(spec.Document["externalDocs"], map[string]any{"description": "Guides", "url": "https://example.com/docs"}) {
		t.Fatalf("unexpected externalDocs %v", spec.Document["externalDocs"])
	}
	scheme := spec.Document["components"].(map[string]any)["securitySchemes"].(map[string]any)["ApiKeyAuth"].(map[string]any)
	if scheme["description"] != "The key POST /login returns." {
		t.Fatalf("expected the scheme's description, got %v", scheme)
	}

	public, err := spec.Public()
	if err != nil {
		t.Fatal(err)
	}
	paths := public.Document["paths"].(map[string]any)
	_, ok := paths["/widgets/legacy"]
	if ok {
		t.Fatal("expected the deprecated route left out of the public spec")
	}
	for path, item := range paths {
		for method, op := range item.(map[string]any) {
			_, hasSource := op.(map[string]any)["x-source"]
			if hasSource {
				t.Fatalf("expected no x-source in the public spec, %s %s has one", method, path)
			}
		}
	}
	list := operation(t, spec.Document, "/widgets", "get")
	if list["x-source"] == nil {
		t.Fatal("expected the full spec to keep x-source")
	}
}
