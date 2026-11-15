package generator

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// moduleLayout writes, under a new directory, the app at app/ with goMod as its go.mod, a module
// example.com/lib at lib/, the same module inside the app's tree at app/third_party/lib/, a module
// example.com/app/sub nested in the app at app/sub/, and a module cache at cache/ holding
// example.com/lib v1.0.0, example.com/fork v1.2.3, example.com/app/sub v1.0.0 and
// github.com/Fork/Lib v1.2.3-RC1, each with a package models. GOMODCACHE points at the cache, and
// ABS in goMod stands for the directory written. It returns that directory.
func moduleLayout(t *testing.T, goMod string) string {
	t.Helper()

	model := "package models\n\ntype Thing struct{}\n"
	base := t.TempDir()
	goMod = strings.ReplaceAll(goMod, "ABS", filepath.ToSlash(base))
	files := map[string]string{
		"app/go.mod":                                           goMod,
		"app/api/api.go":                                       "package api\n",
		"lib/go.mod":                                           "module example.com/lib\n",
		"lib/models/models.go":                                 model,
		"app/third_party/lib/go.mod":                           "module example.com/lib\n",
		"app/third_party/lib/models/models.go":                 model,
		"app/sub/go.mod":                                       "module example.com/app/sub\n",
		"app/sub/models/models.go":                             model,
		"cache/example.com/lib@v1.0.0/models/m.go":             model,
		"cache/example.com/fork@v1.2.3/models/m.go":            model,
		"cache/example.com/app/sub@v1.0.0/models/m.go":         model,
		"cache/github.com/!fork/!lib@v1.2.3-!r!c1/models/m.go": model,
	}
	for name, contents := range files {
		writeGoFile(t, filepath.Join(base, filepath.FromSlash(name)), contents)
	}
	t.Setenv("GOMODCACHE", filepath.Join(base, "cache"))

	return base
}

// Packages are read from where the go command reads them, following the replace directives of
// the app's go.mod: a directory, relative to go.mod or absolute, or a module at a version in the
// module cache, for every version or, with a version on the left, for that one, which wins over a
// replacement for every version. Import paths keep the required module's path.
func TestReplaceDirectivesResolvePackages(t *testing.T) {
	header := "module example.com/app\n\ngo 1.25\n\n"
	cases := []struct {
		name       string
		goMod      string
		importPath string
		// want is the package directory under the layout's directory, or wantErr part of the error.
		want    string
		wantErr string
	}{
		{
			name:       "no replace",
			goMod:      header + "require example.com/lib v1.0.0\n",
			importPath: "example.com/lib/models",
			want:       "cache/example.com/lib@v1.0.0/models",
		},
		{
			name:       "local relative",
			goMod:      header + "require example.com/lib v1.0.0\n\nreplace example.com/lib => ../lib\n",
			importPath: "example.com/lib/models",
			want:       "lib/models",
		},
		{
			name:       "local absolute",
			goMod:      header + "require example.com/lib v1.0.0\n\nreplace example.com/lib => ABS/lib\n",
			importPath: "example.com/lib/models",
			want:       "lib/models",
		},
		{
			name:       "module at version",
			goMod:      header + "require example.com/lib v1.0.0\n\nreplace example.com/lib => example.com/fork v1.2.3\n",
			importPath: "example.com/lib/models",
			want:       "cache/example.com/fork@v1.2.3/models",
		},
		{
			name:       "versioned left side matching",
			goMod:      header + "require example.com/lib v1.0.0\n\nreplace example.com/lib v1.0.0 => ../lib\n",
			importPath: "example.com/lib/models",
			want:       "lib/models",
		},
		{
			name:       "versioned left side not matching",
			goMod:      header + "require example.com/lib v1.0.0\n\nreplace example.com/lib v0.9.0 => ../lib\n",
			importPath: "example.com/lib/models",
			want:       "cache/example.com/lib@v1.0.0/models",
		},
		{
			name:       "both forms, the versioned one matching",
			goMod:      header + "require example.com/lib v1.0.0\n\nreplace (\n\texample.com/lib => example.com/fork v1.2.3\n\texample.com/lib v1.0.0 => ../lib\n)\n",
			importPath: "example.com/lib/models",
			want:       "lib/models",
		},
		{
			name:       "both forms, the versioned one not matching",
			goMod:      header + "require example.com/lib v1.0.0\n\nreplace (\n\texample.com/lib v0.9.0 => ../lib\n\texample.com/lib => example.com/fork v1.2.3\n)\n",
			importPath: "example.com/lib/models",
			want:       "cache/example.com/fork@v1.2.3/models",
		},
		{
			name:       "uppercase path and version escaped in the module cache",
			goMod:      header + "require github.com/Org/Lib v1.0.0\n\nreplace github.com/Org/Lib => github.com/Fork/Lib v1.2.3-RC1\n",
			importPath: "github.com/Org/Lib/models",
			want:       "cache/github.com/!fork/!lib@v1.2.3-!r!c1/models",
		},
		{
			name:       "replacement directory inside the app's tree",
			goMod:      header + "require example.com/lib v1.0.0\n\nreplace example.com/lib => ./third_party/lib\n",
			importPath: "example.com/lib/models",
			want:       "app/third_party/lib/models",
		},
		{
			name:       "the app's path to a directory with a go.mod of its own",
			goMod:      header + "require example.com/lib v1.0.0\n\nreplace example.com/lib => ./third_party/lib\n",
			importPath: "example.com/app/third_party/lib/models",
			wantErr:    filepath.Join("app", "third_party", "lib", "go.mod") + " makes ",
		},
		{
			name:       "module nested under the app's path, from the module cache",
			goMod:      header + "require example.com/app/sub v1.0.0\n",
			importPath: "example.com/app/sub/models",
			want:       "cache/example.com/app/sub@v1.0.0/models",
		},
		{
			name:       "module nested under the app's path, replaced by its directory",
			goMod:      header + "require example.com/app/sub v1.0.0\n\nreplace example.com/app/sub => ./sub\n",
			importPath: "example.com/app/sub/models",
			want:       "app/sub/models",
		},
		{
			name:       "the app's own package beside a nested module",
			goMod:      header + "require example.com/app/sub v1.0.0\n\nreplace example.com/app/sub => ./sub\n",
			importPath: "example.com/app/api",
			want:       "app/api",
		},
		{
			name:       "replaced module not required",
			goMod:      header + "replace example.com/lib => ../lib\n",
			importPath: "example.com/lib/models",
			wantErr:    "module not found for import example.com/lib/models",
		},
		{
			name: "every directive the go command accepts",
			goMod: header + "toolchain go1.25.1\n\ngodebug default=go1.21\n\n" +
				"require (\n\t\"example.com/lib\" v0.9.0 // indirect\n\texample.com/lib v1.0.0\n)\n\n" +
				"tool example.com/lib/models\n\nignore ./node_modules\n\nexclude example.com/lib v0.8.0\n\nretract v0.1.0\n\n" +
				"replace \"example.com/lib\" v1.0.0 => \"../lib\"\n",
			importPath: "example.com/lib/models",
			want:       "lib/models",
		},
		{
			name:       "the highest required version, listed first",
			goMod:      header + "require (\n\texample.com/lib v1.0.0\n\texample.com/lib v0.9.0 // indirect\n)\n\nreplace example.com/lib v1.0.0 => ../lib\n",
			importPath: "example.com/lib/models",
			want:       "lib/models",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := moduleLayout(t, tc.goMod)
			builder, err := newSchemaBuilder(Config{Root: filepath.Join(base, "app")})
			if err != nil {
				t.Fatal(err)
			}

			dir, _, err := builder.resolveImportPath(tc.importPath)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("expected an error with %q, got %q, %v", tc.wantErr, dir, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := filepath.Join(base, filepath.FromSlash(tc.want))
			if dir != want {
				t.Fatalf("expected %s, got %s", want, dir)
			}
		})
	}
}

// A go.mod the go command refuses is refused: two replace directives that disagree, and a
// package in two modules at once.
func TestReplaceDirectivesThatTheGoCommandRefuses(t *testing.T) {
	base := moduleLayout(t, "module example.com/app\n\nrequire example.com/lib v1.0.0\n\nreplace example.com/lib => ../lib\nreplace example.com/lib => ./third_party/lib\n")
	_, err := newSchemaBuilder(Config{Root: filepath.Join(base, "app")})
	if err == nil || !strings.Contains(err.Error(), "conflicting replacements for example.com/lib: ../lib and ./third_party/lib") {
		t.Fatalf("expected the conflicting replacements refused, got %v", err)
	}

	// The same replacement twice is no conflict.
	base = moduleLayout(t, "module example.com/app\n\nrequire example.com/lib v1.0.0\n\nreplace example.com/lib => ../lib\nreplace example.com/lib => ../lib\n")
	_, err = newSchemaBuilder(Config{Root: filepath.Join(base, "app")})
	if err != nil {
		t.Fatal(err)
	}

	// app/sub/models/x/x.go would put the package in the app as well as in
	// example.com/app/sub, but app/sub/go.mod keeps the app out.
	base = moduleLayout(t, "module example.com/app\n\nrequire example.com/app/sub v1.0.0\n")
	writeGoFile(t, filepath.Join(base, "app", "sub", "models", "x", "x.go"), "package x\n")
	writeGoFile(t, filepath.Join(base, "cache", "example.com", "app", "sub@v1.0.0", "models", "x", "x.go"), "package x\n")
	builder, err := newSchemaBuilder(Config{Root: filepath.Join(base, "app")})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = builder.resolveImportPath("example.com/app/sub/models/x")
	if err != nil {
		t.Fatalf("expected app/sub/go.mod to keep the app out, got %v", err)
	}
	writeGoFile(t, filepath.Join(base, "app", "pkg", "x.go"), "package pkg\n")
	builder.modules["example.com/app/pkg"] = moduleSource{Path: "example.com/app/pkg", Version: "v1.0.0"}
	writeGoFile(t, filepath.Join(base, "cache", "example.com", "app", "pkg@v1.0.0", "x.go"), "package pkg\n")
	_, _, err = builder.resolveImportPath("example.com/app/pkg")
	if err == nil || !strings.Contains(err.Error(), "ambiguous import: package example.com/app/pkg is in module example.com/app/pkg at ") {
		t.Fatalf("expected an ambiguous import refused, got %v", err)
	}

	// A go.mod below a replacement directory cuts that directory off its module too.
	base = moduleLayout(t, "module example.com/app\n\nrequire example.com/lib v1.0.0\n\nreplace example.com/lib => ../lib\n")
	writeGoFile(t, filepath.Join(base, "lib", "nested", "go.mod"), "module example.com/lib/nested\n")
	writeGoFile(t, filepath.Join(base, "lib", "nested", "n", "n.go"), "package n\n")
	builder, err = newSchemaBuilder(Config{Root: filepath.Join(base, "app")})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = builder.resolveImportPath("example.com/lib/nested/n")
	if err == nil || !strings.Contains(err.Error(), "a module of its own") {
		t.Fatalf("expected the nested go.mod to cut the package off example.com/lib, got %v", err)
	}
}

// A package missing from a directory a replace directive names is not fixed by downloading, so
// the error names the directive instead.
func TestMissingReplacementDirectoryNamesTheDirective(t *testing.T) {
	base := moduleLayout(t, "module example.com/app\n\nrequire example.com/lib v1.0.0\n\nreplace example.com/lib => ../missing\n")
	builder, err := newSchemaBuilder(Config{Root: filepath.Join(base, "app")})
	if err != nil {
		t.Fatal(err)
	}

	builder.schemaRef(typeRef{ImportPath: "example.com/lib/models", Name: "Thing"})
	err = builder.unresolvedError()
	missing := filepath.Join(base, "missing")
	want := "package source not found at " + filepath.Join(missing, "models") + " (go.mod replaces example.com/lib with the directory " + missing + ")"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("expected %q, got %v", want, err)
	}
	if strings.Contains(err.Error(), "go mod download") {
		t.Fatalf("expected no download advice for a local replacement, got %v", err)
	}
}

// A file read by its name, as HandlerGlob matches them, is in the package resolveImportPath finds
// its directory for: the app's under Root, the replaced module's under a replacement directory,
// and none under a go.mod that is neither.
func TestPackagePathForFileFollowsReplaceDirectives(t *testing.T) {
	base := moduleLayout(t, "module example.com/app\n\nrequire example.com/lib v1.0.0\n\nreplace example.com/lib => ./third_party/lib\n")
	root := filepath.Join(base, "app")
	builder, err := newSchemaBuilder(Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}

	cases := map[string]string{
		filepath.Join(root, "main.go"):                                   "example.com/app",
		filepath.Join(root, "api", "api.go"):                             "example.com/app/api",
		filepath.Join(root, "third_party", "lib", "models", "models.go"): "example.com/lib/models",
	}
	for file, want := range cases {
		got, err := builder.packagePathForFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s: expected %s, got %s", file, want, got)
		}
		dir, _, err := builder.resolveImportPath(got)
		if err != nil {
			t.Fatal(err)
		}
		if dir != filepath.Dir(file) {
			t.Fatalf("%s: expected %s to resolve to its directory, got %s", file, got, dir)
		}
	}

	_, err = builder.packagePathForFile(filepath.Join(root, "sub", "models", "models.go"))
	if err == nil || !strings.Contains(err.Error(), "which is not the app's and replaces no module the app's go.mod requires") {
		t.Fatalf("expected a file of a module outside the build refused, got %v", err)
	}
}

// A response type from a module a replace directive puts a local directory in place of is
// documented from that directory, not from the module cache's copy of the required version.
func TestGenerateDocumentsALocallyReplacedModule(t *testing.T) {
	base := writeFixture(t, map[string]string{
		"app/go.mod": "module example.com/app\n\ngo 1.22\n\nrequire example.com/lib v1.0.0\n\nreplace example.com/lib => ../lib\n",
		"app/api/routes.go": `package api

func addRoutes(e *echo.Echo, h *Handler) {
	e.GET("/things/:id", h.GetThing)
}
`,
		"app/api/handler.go": `package api

import (
	"github.com/labstack/echo/v5"

	"example.com/app/things"
)

type Handler struct{}

func (h *Handler) GetThing(c *echo.Context) error {
	return h.JSONResponse(c, "GetThing", nil, &things.GetRequest{})
}
`,
		"app/things/things.go": `package things

import (
	"context"

	"example.com/lib/models"
)

type GetRequest struct{}

func Get(ctx context.Context, request *GetRequest) (*models.Thing, error) { return nil, nil }
`,
		"lib/go.mod":                                    "module example.com/lib\n",
		"lib/models/models.go":                          "package models\n\ntype Thing struct {\n\tLocal string `json:\"local\"`\n}\n",
		"cache/example.com/lib@v1.0.0/go.mod":           "module example.com/lib\n",
		"cache/example.com/lib@v1.0.0/models/models.go": "package models\n\ntype Thing struct {\n\tCached string `json:\"cached\"`\n}\n",
	})
	t.Setenv("GOMODCACHE", filepath.Join(base, "cache"))

	spec, err := Generate(thingConfig(filepath.Join(base, "app")))
	if err != nil {
		t.Fatal(err)
	}
	thing, ok := componentSchemas(spec.Document)["example_com_lib_models_Thing"]
	if !ok {
		t.Fatalf("expected the component example_com_lib_models_Thing, got %v", sortedKeys(componentSchemas(spec.Document)))
	}
	properties := thing["properties"].(map[string]any)
	if !reflect.DeepEqual(sortedKeys(properties), []string{"local"}) {
		t.Fatalf("expected the local Thing's fields, got %v", properties)
	}
}

// Without a module cache path, a package the app's own tree holds is still found where a
// required module nested under the app's path is looked in too, and a package only the module
// cache could hold fails for the module cache path not being set.
func TestResolveImportPathWithoutAModuleCache(t *testing.T) {
	goMod := "module example.com/app\n\ngo 1.25\n\nrequire example.com/app/sub v1.0.0\n"
	base := moduleLayout(t, goMod)
	builder, err := newSchemaBuilder(Config{Root: filepath.Join(base, "app")})
	if err != nil {
		t.Fatal(err)
	}
	builder.modCache = ""

	dir, _, err := builder.resolveImportPath("example.com/app/api")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(base, "app", "api")
	if dir != want {
		t.Fatalf("expected %s, got %s", want, dir)
	}

	_, _, err = builder.resolveImportPath("example.com/app/sub/models")
	if err == nil || !strings.Contains(err.Error(), "module cache path not set") {
		t.Fatalf("expected the module cache error, got %v", err)
	}
}
