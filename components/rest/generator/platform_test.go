package generator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildThingsFiles is buildThings for a package of several files, named in files, with
// Config.TypeSchemas set to described.
func buildThingsFiles(t *testing.T, files map[string]string, described map[string]map[string]any, names ...string) *schemaBuilder {
	t.Helper()

	repoRoot := t.TempDir()
	pkgDir := filepath.Join(repoRoot, "things")
	err := os.MkdirAll(pkgDir, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	for name, source := range files {
		err = os.WriteFile(filepath.Join(pkgDir, name), []byte(source), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	builder := newTestSchemaBuilder(t, repoRoot)
	builder.typeSchemas = described
	for _, name := range names {
		builder.schemaRef(typeRef{ImportPath: "example.com/app/things", Name: name})
	}

	return builder
}

const levelSource = `package things

type Level int
`

const levelMarshalText = `package things

func (l Level) MarshalText() ([]byte, error) {
	return []byte("level"), nil
}
`

// A marshal method or IsZero declared in a file that some platform its type is declared on does
// not build makes encoding/json write the type differently by platform, so generation fails
// naming the type, the method and the file, unless Config.TypeSchemas describes the type.
func TestPlatformMethodsAreRefused(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		types []string
		file  string
	}{
		{"file name", map[string]string{"things.go": levelSource, "level_windows.go": levelMarshalText}, []string{"Level"}, "level_windows.go"},
		{"build line", map[string]string{"things.go": levelSource, "level.go": "//go:build linux && amd64\n\n" + levelMarshalText}, []string{"Level"}, "level.go"},
		{"cgo", map[string]string{"things.go": levelSource, "level.go": strings.Replace(levelMarshalText, "package things\n", "package things\n\nimport \"C\"\n", 1)}, []string{"Level"}, "level.go"},
		{"field", map[string]string{"things.go": levelSource + "\ntype Holder struct {\n\tL Level `json:\"l\"`\n}\n", "level_darwin.go": levelMarshalText}, []string{"Holder"}, "level_darwin.go"},
		{"IsZero", map[string]string{
			"things.go":      "package things\n\ntype Tags []string\n\ntype Holder struct {\n\tT Tags `json:\"t,omitzero\"`\n}\n",
			"tags_linux.go":  "package things\n\nfunc (t Tags) IsZero() bool {\n\treturn false\n}\n",
			"tags_ignore.go": "//go:build ignore\n\npackage things\n",
		}, []string{"Holder"}, "tags_linux.go"},
	}
	for _, c := range cases {
		builder := buildThingsFiles(t, c.files, nil, c.types...)
		err := builder.unresolvedError()
		method := "MarshalText"
		name := "Level"
		if c.name == "IsZero" {
			method = "IsZero"
			name = "Tags"
		}
		expected := "example.com/app/things." + name + ": method " + method + " of " + name + " is declared in " + c.file + ", whose build constraints leave out platforms " + name + " is declared on, so encoding/json writes " + name + " differently by platform and the generator does not pick one; describe it with Config.TypeSchemas[\"example.com/app/things." + name + "\"]"
		if err == nil || !strings.Contains(err.Error(), expected) {
			t.Errorf("%s: expected %q, got %v", c.name, expected, err)
		}

		described := map[string]map[string]any{"example.com/app/things." + name: {"type": "string"}}
		builder = buildThingsFiles(t, c.files, described, c.types...)
		err = builder.unresolvedError()
		if err != nil {
			t.Errorf("%s: expected the description to settle it, got %v", c.name, err)
		}
	}
}

// A method declared for every platform its type is, in files that split the platforms between
// them or beside a type declared for the same platforms, is the type's everywhere; a file that
// no platform builds, such as one marked //go:build ignore, is not read.
func TestPlatformMethodsEverywhereAreRead(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		kind  string
	}{
		{"split", map[string]string{
			"things.go":        levelSource,
			"level_windows.go": levelMarshalText,
			"level_other.go":   "//go:build !windows\n\n" + levelMarshalText,
		}, "string"},
		{"same platforms", map[string]string{"level_linux.go": levelSource + strings.TrimPrefix(levelMarshalText, "package things\n")}, "string"},
		{"ignored", map[string]string{"things.go": levelSource, "level.go": "//go:build ignore\n\n" + levelMarshalText}, "integer"},
		{"ignored main", map[string]string{"things.go": levelSource, "gen.go": "//go:build ignore\n\npackage main\n\nfunc (l Level) MarshalText() ([]byte, error) {\n"}, "integer"},
	}
	for _, c := range cases {
		builder := buildThingsFiles(t, c.files, nil, "Level")
		err := builder.unresolvedError()
		if err != nil {
			t.Errorf("%s: expected Level's schema, got %v", c.name, err)
			continue
		}
		schema := builder.schemas["things_Level"]
		if schema["type"] != c.kind {
			t.Errorf("%s: expected a schema of type %s, got %v", c.name, c.kind, schema)
		}
	}
}
