package spectest_test

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"

	"github.com/vanclief/compose/components/rest/generator"
	"github.com/vanclief/compose/components/rest/spectest"
	"github.com/vanclief/compose/components/rest/spectest/internal/testapp"
	"github.com/vanclief/compose/components/rest/spectest/internal/testapp/widgets"
)

// generateTestApp runs the generator on internal/testapp, read from the
// module root as an app reads its own, and returns the full and the public
// document as JSON.
func generateTestApp(t *testing.T) ([]byte, []byte) {
	t.Helper()

	root, err := filepath.Abs("../../..")
	require.NoError(t, err)

	spec, err := generator.Generate(generator.Config{
		Root:                  root,
		RoutesGlob:            "components/rest/spectest/internal/testapp/routes.go",
		HandlerGlob:           "components/rest/spectest/internal/testapp/handler.go",
		RequiredPackagePrefix: "/testapp/",
		SchemaNameAliases:     map[string]string{"components/rest/spectest/internal/testapp/widgets": "widgets"},
		Info:                  generator.Info{Title: "Test app", Version: "1.0"},
		Skip: func(method string, path string) bool {
			return path == "/health"
		},
		Internal: func(method string, path string) bool {
			return strings.HasSuffix(path, "/admin")
		},
		// The generator does not document a route registered through Add
		// and fails on it unless the app leaves it out.
		SkipUnresolved: func(route generator.UnresolvedRoute) bool {
			return strings.Contains(route.Path, "/widgets/:id/history")
		},
	})
	require.NoError(t, err)

	full, err := spec.JSON()
	require.NoError(t, err)

	public, err := spec.Public()
	require.NoError(t, err)
	publicJSON, err := public.JSON()
	require.NoError(t, err)

	return full, publicJSON
}

func newTestAppEcho() *echo.Echo {
	e := echo.New()
	testapp.Routes(e, &testapp.Handler{})
	return e
}

func TestGeneratedDocumentRoutes(t *testing.T) {
	full, public := generateTestApp(t)
	e := newTestAppEcho()

	// The route registered through Add is left out of the document by
	// SkipUnresolved, and /health is skipped by the generator's config.
	fake := &fakeTB{TB: t}
	spectest.Routes(fake, e, full)
	require.Len(t, fake.errors, 1)
	require.Contains(t, fake.errors[0], "in Echo, not in the document:\n    GET /health\n    GET /widgets/{id}/history")
	require.NotContains(t, fake.errors[0], "in the document, not in Echo")

	// With those ignored on purpose, the generator and Echo agree on every
	// path, the wildcard and the parameters included.
	spectest.Routes(t, e, full, spectest.Ignore("GET", "/health"), spectest.Ignore("GET", "/widgets/:id/history"))

	// The public document leaves the internal route out: the subset case.
	spectest.Routes(t, e, public,
		spectest.Ignore("GET", "/health"),
		spectest.Ignore("GET", "/widgets/{id}/history"),
		spectest.Ignore("GET", "/widgets/{id}/admin"),
	)
}

func TestGeneratedDocumentSchemas(t *testing.T) {
	full, _ := generateTestApp(t)

	created := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	parent := &widgets.Widget{ID: 1, Name: "root", Tags: []string{"a"}, Note: "first", CreatedAt: created}

	// A nil slice and a nil pointer marshal as null, which the generator
	// documents as nullable; an empty omitempty field is left out.
	spectest.Schema(t, full, "widgets_Widget", widgets.Widget{ID: 2, Name: "child", CreatedAt: created})
	spectest.Schema(t, full, "widgets_Widget", widgets.Widget{ID: 2, Name: "child", Parent: parent, CreatedAt: created})
	spectest.Schema(t, full, "widgets_ListResponse", widgets.ListResponse{})
	spectest.Schema(t, full, "widgets_ListResponse", widgets.ListResponse{Widgets: []widgets.Widget{*parent}})
	spectest.Schema(t, full, "widgets_AdminWidget", widgets.AdminWidget{Widget: *parent, Owner: "ops"})
	spectest.Schema(t, full, "widgets_File", widgets.File{Path: "a/b.txt", Size: 3})

	// A recorded body passes through as written.
	body, err := json.Marshal(parent)
	require.NoError(t, err)
	spectest.Schema(t, full, "widgets_Widget", json.RawMessage(body))

	// A value of another type does not match.
	fake := &fakeTB{TB: t}
	spectest.Schema(fake, full, "widgets_Widget", widgets.File{Path: "x"})
	require.Len(t, fake.errors, 1)
	require.Contains(t, fake.errors[0], "component widgets_Widget")

	// A key the document names otherwise than encoding/json writes it fails,
	// on the model and on the one that embeds it, though the document's note
	// is optional and its objects state no additionalProperties.
	renamed := bytes.Replace(full, []byte(`"note":`), []byte(`"remark":`), 1)
	require.NotEqual(t, full, renamed)
	values := map[string]any{"widgets_Widget": *parent, "widgets_AdminWidget": widgets.AdminWidget{Widget: *parent, Owner: "ops"}}
	for component, value := range values {
		fake = &fakeTB{TB: t}
		spectest.Schema(fake, renamed, component, value)
		require.Len(t, fake.errors, 1)
		require.Contains(t, fake.errors[0], `at "/note": the document does not declare this key`)
	}
}
