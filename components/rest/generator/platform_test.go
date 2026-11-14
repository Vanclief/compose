package generator

import (
	"os"
	"path/filepath"
	"slices"
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
		{"cgo tag", map[string]string{"things.go": levelSource, "level.go": "//go:build cgo\n\n" + levelMarshalText}, []string{"Level"}, "level.go"},
		{"field", map[string]string{"things.go": levelSource + "\ntype Holder struct {\n\tL Level `json:\"l\"`\n}\n", "level_darwin.go": levelMarshalText}, []string{"Holder"}, "level_darwin.go"},
		{"IsZero", map[string]string{
			"things.go":      "package things\n\ntype Tags []string\n\ntype Holder struct {\n\tT Tags `json:\"t,omitzero\"`\n}\n",
			"tags_linux.go":  "package things\n\nfunc (t Tags) IsZero() bool {\n\treturn false\n}\n",
			"tags_ignore.go": "//go:build ignore\n\npackage things\n",
		}, []string{"Holder"}, "tags_linux.go"},
		// A method declared everywhere that ranks below it does not outrank it.
		{"ranked above", map[string]string{"things.go": levelSource + strings.TrimPrefix(levelMarshalText, "package things\n"), "level_windows.go": "package things\n\nfunc (l Level) MarshalJSON() ([]byte, error) {\n\treturn []byte(\"1\"), nil\n}\n"}, []string{"Level"}, "level_windows.go"},
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
		if c.name == "ranked above" {
			method = "MarshalJSON"
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
		{"cgo split", map[string]string{
			"things.go":      levelSource,
			"level_cgo.go":   "//go:build cgo\n\n" + levelMarshalText,
			"level_nocgo.go": "//go:build !cgo\n\n" + levelMarshalText,
		}, "string"},
		{"outranked", map[string]string{
			"things.go":        levelSource + "\nfunc (l Level) MarshalJSON() ([]byte, error) {\n\treturn []byte(\"1\"), nil\n}\n",
			"level_windows.go": levelMarshalText,
		}, "integer"},
		{"same platforms", map[string]string{"level_linux.go": levelSource + strings.TrimPrefix(levelMarshalText, "package things\n")}, "string"},
		{"ignored", map[string]string{"things.go": levelSource, "level.go": "//go:build ignore\n\n" + levelMarshalText}, "integer"},
		{"ignored main", map[string]string{"things.go": levelSource, "gen.go": "//go:build ignore\n\npackage main\n\nfunc (l Level) MarshalText() ([]byte, error) {\n"}, "integer"},
		{"ignored template", map[string]string{"things.go": levelSource, "gen.go": "//go:build ignore\n\npackage {{.Name}}\n\nfunc (l Level) MarshalText() ([]byte, error) {\n"}, "integer"},
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

// A type declared from one with a method only some platforms build gets none of its methods, so
// encoding/json writes it by its kind on every platform; an alias is that type, and is refused.
func TestPlatformMethodsOfADeclaredFromType(t *testing.T) {
	files := map[string]string{
		"things.go":        levelSource + "\ntype D Level\n\ntype A = Level\n\ntype Holder struct {\n\tX D `json:\"x\"`\n}\n\ntype AliasHolder struct {\n\tX A `json:\"x\"`\n}\n",
		"level_windows.go": levelMarshalText,
	}
	builder := buildThingsFiles(t, files, nil, "Holder")
	err := builder.unresolvedError()
	if err != nil {
		t.Fatalf("expected Holder's schema, got %v", err)
	}
	schema := builder.schemas["things_D"]
	if schema["type"] != "integer" {
		t.Errorf("expected D to be an integer, got %v", schema)
	}

	builder = buildThingsFiles(t, files, nil, "AliasHolder")
	err = builder.unresolvedError()
	if err == nil || !strings.Contains(err.Error(), "method MarshalText of Level is declared in level_windows.go") {
		t.Errorf("expected the alias of Level refused, got %v", err)
	}
}

// Files are selected with the goexperiment.jsonv2 tag of the rules compiled into the generator,
// not of the environment it runs in, which a go env file or a prebuilt generator can set apart.
func TestPlatformsFollowTheCompiledRules(t *testing.T) {
	if slices.Contains(toolTags, "goexperiment.jsonv2") != encodingJSONV2 {
		t.Fatalf("expected goexperiment.jsonv2 in %v exactly when encodingJSONV2 (%v)", toolTags, encodingJSONV2)
	}
	src := []byte("//go:build goexperiment.jsonv2\n\npackage things\n")
	selected, err := filePlatforms(".", "v2.go", src)
	if err != nil {
		t.Fatal(err)
	}
	if (selected != platformSet{}) != encodingJSONV2 {
		t.Errorf("expected a jsonv2 file selected exactly when encodingJSONV2 (%v), got %v", encodingJSONV2, selected)
	}
}

// Config.TypeSchemas settles a type with a method only some platforms build also where its
// methods are read for a field holding it, as a slice of a byte type reads them to tell base64
// from an array.
func TestPlatformMethodsOfADescribedField(t *testing.T) {
	files := map[string]string{
		"things.go":        "package things\n\ntype Level uint8\n\ntype Holder struct {\n\tL []Level `json:\"l\"`\n}\n",
		"level_windows.go": levelMarshalText,
	}
	described := map[string]map[string]any{"example.com/app/things.Level": {"type": "string"}}
	builder := buildThingsFiles(t, files, described, "Holder")
	err := builder.unresolvedError()
	if err != nil {
		t.Fatalf("expected Holder's schema, got %v", err)
	}

	builder = buildThingsFiles(t, files, nil, "Holder")
	err = builder.unresolvedError()
	if err == nil || !strings.Contains(err.Error(), "method MarshalText of Level is declared in level_windows.go") {
		t.Errorf("expected Level refused without its description, got %v", err)
	}
}
