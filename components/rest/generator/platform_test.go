package generator

import (
	"os"
	"path/filepath"
	"reflect"
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

// A type declared differently in several files, as one declared per platform may be, fails
// generation wherever its schema is built, naming the files, whatever platform the generator runs on, unless Config.TypeSchemas
// describes it. Before, the last file read won.
func TestPerPlatformTypeIsRefused(t *testing.T) {
	holder := "package things\n\ntype Holder struct {\n\tK Kind `json:\"k\"`\n}\n"
	marshalText := "\nfunc (k Kind) MarshalText() ([]byte, error) {\n\treturn nil, nil\n}\n"
	cases := []struct {
		name  string
		files map[string]string
		types string
	}{
		{"build line", map[string]string{
			"kind_a.go": "//go:build linux\n\npackage things\n\ntype Kind string\n",
			"kind_b.go": "//go:build !linux\n\npackage things\n\ntype Kind bool\n",
		}, "kind_a.go, kind_b.go"},
		{"file name", map[string]string{
			"kind_darwin.go":  "package things\n\ntype Kind string\n",
			"kind_linux.go":   "package things\n\ntype Kind int64\n",
			"kind_windows.go": "package things\n\ntype Kind bool\n",
			"kind_ignored.go": "//go:build ignore\n\npackage things\n\ntype Kind float64\n",
		}, "kind_darwin.go, kind_linux.go, kind_windows.go"},
		{"cgo", map[string]string{
			"kind_cgo.go":   "package things\n\nimport \"C\"\n\ntype Kind string\n",
			"kind_nocgo.go": "//go:build !cgo\n\npackage things\n\ntype Kind bool\n",
		}, "kind_cgo.go, kind_nocgo.go"},
		{"method beside it", map[string]string{
			"kind_a.go": "//go:build linux\n\npackage things\n\ntype Kind int\n" + marshalText,
			"kind_b.go": "//go:build !linux\n\npackage things\n\ntype Kind bool\n" + marshalText,
		}, "kind_a.go, kind_b.go"},
	}
	for _, c := range cases {
		files := map[string]string{"holder.go": holder}
		for name, source := range c.files {
			files[name] = source
		}
		expected := "example.com/app/things.Kind: type Kind is declared differently in " + c.types + ", one per platform, and the generator does not pick one; describe it with Config.TypeSchemas[\"example.com/app/things.Kind\"]"
		for _, name := range []string{"Kind", "Holder"} {
			builder := buildThingsFiles(t, files, nil, name)
			err := builder.unresolvedError()
			if err == nil || !strings.Contains(err.Error(), expected) {
				t.Errorf("%s, %s: expected %q, got %v", c.name, name, expected, err)
			}
		}

		described := map[string]map[string]any{"example.com/app/things.Kind": {"type": "string", "format": "kind"}}
		builder := buildThingsFiles(t, files, described, "Holder")
		err := builder.unresolvedError()
		if err != nil {
			t.Errorf("%s: expected the description to settle it, got %v", c.name, err)
			continue
		}
		if builder.schemas["things_Kind"]["format"] != "kind" {
			t.Errorf("%s: expected the description as Kind's schema, got %v", c.name, builder.schemas["things_Kind"])
		}
	}

	// A struct embedding a described per-platform type whose marshal method, declared in a file
	// every platform builds, Go promotes into the struct is refused, as for a type declared once:
	// encoding/json writes it as that type alone.
	method := func(name string) string {
		return "package things\n\nfunc (k Kind) " + name + "() ([]byte, error) {\n\treturn nil, nil\n}\n"
	}
	embedded := []struct {
		name    string
		kinds   [2]string
		method  string
		embed   string
		written string
	}{
		{"struct", [2]string{"struct {\n\tA int\n}", "struct {\n\tB int\n}"}, "MarshalJSON", "Kind", "Kind"},
		{"pointer", [2]string{"struct {\n\tA int\n}", "struct {\n\tB int\n}"}, "MarshalJSON", "*Kind", "Kind"},
		{"text", [2]string{"struct {\n\tA int\n}", "struct {\n\tB int\n}"}, "MarshalText", "Kind", "Kind's text"},
		{"not a struct", [2]string{"string", "int"}, "MarshalJSON", "Kind", "Kind"},
	}
	for _, c := range embedded {
		files := map[string]string{
			"kind_a.go": "//go:build linux\n\npackage things\n\ntype Kind " + c.kinds[0] + "\n",
			"kind_b.go": "//go:build !linux\n\npackage things\n\ntype Kind " + c.kinds[1] + "\n",
			"kind.go":   method(c.method),
			"holder.go": "package things\n\ntype Holder struct {\n\t" + c.embed + "\n\tX int `json:\"x\"`\n}\n",
		}
		described := map[string]map[string]any{"example.com/app/things.Kind": {"type": "object"}}
		builder := buildThingsFiles(t, files, described, "Holder")
		expected := "type Holder embeds Kind, which defines " + c.method + ", so it marshals as " + c.written + " alone; Config.TypeSchemas describes Kind, not the types that embed it"
		err := builder.unresolvedError()
		if err == nil || !strings.Contains(err.Error(), expected) {
			t.Errorf("embedded %s: expected %q, got %v", c.name, expected, err)
		}
	}
}

// A type declared in several files with declarations that are the same, its type parameters and
// type, tags and field order included, and the packages its qualified identifiers and its dot
// import name, is read as one; declarations that differ in any of these are refused. A
// marshal method declared on fewer platforms than the type is still refused.
func TestSamePerPlatformDeclarationsAreRead(t *testing.T) {
	unix := "//go:build !windows\n\npackage things\n\n"
	windows := "package things\n\n"
	info := "type Info struct {\n\tName string `json:\"name\"`\n\tSize int64  `json:\"size,omitempty\"`\n}\n"
	holder := "package things\n\ntype Holder struct {\n\tH Handle `json:\"h\"`\n\tI Info   `json:\"i\"`\n}\n"
	handle := "type Handle int64\n"

	files := map[string]string{
		"holder.go":     holder,
		"fd_unix.go":    unix + handle + "\n" + info,
		"fd_windows.go": windows + handle + "\n" + info,
	}
	builder := buildThingsFiles(t, files, nil, "Holder")
	err := builder.unresolvedError()
	if err != nil {
		t.Fatalf("expected the same declarations to be read as one, got %v", err)
	}
	if !reflect.DeepEqual(builder.schemas["things_Handle"], map[string]any{"type": "integer", "format": "int64"}) {
		t.Errorf("expected Handle to be an int64, got %v", builder.schemas["things_Handle"])
	}
	properties := componentProperties(t, builder, "things_Info")
	if !reflect.DeepEqual(sortedKeys(properties), []string{"name", "size"}) {
		t.Errorf("expected Info's tagged fields, got %v", sortedKeys(properties))
	}

	refused := func(name string) string {
		return "type " + name + " is declared differently in fd_unix.go, fd_windows.go, one per platform, and the generator does not pick one"
	}
	cases := []struct {
		name     string
		unix     string
		windows  string
		expected string
	}{
		{"a tag", handle + info, handle + strings.Replace(info, "size,omitempty", "size", 1), refused("Info")},
		{"field order", handle + info, handle + "type Info struct {\n\tSize int64  `json:\"size,omitempty\"`\n\tName string `json:\"name\"`\n}\n", refused("Info")},
		{"a qualifier", "import x \"example.com/app/one\"\n\ntype Handle x.ID\n\n" + info, "import x \"example.com/app/two\"\n\ntype Handle x.ID\n\n" + info, refused("Handle")},
		{"a dot import", "import . \"example.com/app/one\"\n\ntype Handle ID\n\n" + info, "import . \"example.com/app/two\"\n\ntype Handle ID\n\n" + info, refused("Handle")},
		{"an alias", handle + info, "type Handle = int64\n\n" + info, refused("Handle")},
		{"a platform method", handle + info, handle + "\nfunc (h Handle) MarshalText() ([]byte, error) {\n\treturn nil, nil\n}\n\n" + info, "method MarshalText of Handle is declared in fd_windows.go, whose build constraints leave out platforms Handle is declared on"},
	}
	for _, c := range cases {
		files := map[string]string{
			"holder.go":     holder,
			"fd_unix.go":    unix + c.unix,
			"fd_windows.go": windows + c.windows,
		}
		builder := buildThingsFiles(t, files, nil, "Holder")
		err := builder.unresolvedError()
		if err == nil || !strings.Contains(err.Error(), c.expected) {
			t.Errorf("%s: expected %q, got %v", c.name, c.expected, err)
		}
	}
}

// An AppendText or MarshalJSONTo signature that names a type declared the same in several files
// is read, as the type does not depend on the platform.
func TestCoderSignatureNamingASamePerPlatformTypeIsRead(t *testing.T) {
	files := map[string]string{
		"buf_unix.go":    "//go:build !windows\n\npackage things\n\ntype Buf = []byte\n",
		"buf_windows.go": "package things\n\ntype Buf = []byte\n",
		"level.go":       "package things\n\ntype Level int\n\nfunc (l Level) AppendText(b Buf) (Buf, error) {\n\treturn b, nil\n}\n",
	}
	builder := buildThingsFiles(t, files, nil)
	pkg, err := builder.loadPackage("example.com/app/things")
	if err != nil {
		t.Fatal(err)
	}
	reason := pkg.RefusedMethods["Level"]["AppendText"]
	if !pkg.Methods["Level"]["AppendText"] || reason != "" {
		t.Errorf("expected Level's AppendText read as called, got %v, %q", pkg.Methods["Level"], reason)
	}
}

// Declarations of a type in several files are compared by the type they declare, not by their
// layout: blank lines, names declared together, an empty struct over two lines, type parameters
// sharing a constraint, parentheses, a tag written as an interpreted string or with more spaces
// between its keys and two names for one import read as one declaration, and so do a field
// neither exported nor embedded, which encoding/json does not write, and, with DocComments off,
// comments the document does not show. Field names, types, tags as reflect.StructTag reads them,
// type parameters, an embedded field, a Deprecated: field comment and, with DocComments on, the
// doc and field comments still count.
func TestPerPlatformDeclarationLayoutDoesNotCount(t *testing.T) {
	cases := []struct {
		name        string
		unix        string
		windows     string
		same        bool
		docComments bool
	}{
		{"blank line", "type K struct {\n\tA int\n\tB int\n}\n", "type K struct {\n\tA int\n\n\tB int\n}\n", true, false},
		{"names together", "type K struct {\n\tA, B int\n}\n", "type K struct {\n\tA int\n\tB int\n}\n", true, false},
		{"empty struct", "type K struct{}\n", "type K struct {\n}\n", true, false},
		{"type parameters", "type K[T any, U any] struct {\n\tA T\n\tB U\n}\n", "type K[T, U any] struct {\n\tA T\n\tB U\n}\n", true, false},
		{"parentheses", "type K (int)\n", "type K int\n", true, false},
		{"interpreted tag", "type K struct {\n\tA int `json:\"a\"`\n}\n", "type K struct {\n\tA int \"json:\\\"a\\\"\"\n}\n", true, false},
		{"tag spacing", "type K struct {\n\tA int `json:\"a\" xml:\"a\"`\n}\n", "type K struct {\n\tA int `json:\"a\"  xml:\"a\"`\n}\n", true, false},
		{"import names", "import a \"time\"\n\ntype K struct {\n\tA a.Time\n}\n", "import b \"time\"\n\ntype K struct {\n\tA b.Time\n}\n", true, false},
		{"field name", "type K struct {\n\tA int\n}\n", "type K struct {\n\tB int\n}\n", false, false},
		{"field type", "type K struct {\n\tA int\n}\n", "type K struct {\n\tA int64\n}\n", false, false},
		{"tag value", "type K struct {\n\tA int `json:\"a\"`\n}\n", "type K struct {\n\tA int `json:\"b\"`\n}\n", false, false},
		{"constraint", "type K[T any] struct {\n\tA T\n}\n", "type K[T comparable] struct {\n\tA T\n}\n", false, false},
		{"unexported field", "type K struct {\n\tA int `json:\"a\"`\n\t// fd is the descriptor.\n\tfd int\n}\n", "type K struct {\n\tA int `json:\"a\"`\n\thandle, h uintptr `json:\"h\"`\n}\n", true, false},
		{"unexported embedded field", "type K struct {\n\tA int\n\tinner\n}\n\ntype inner struct{}\n", "type K struct {\n\tA int\n}\n\ntype inner struct{}\n", false, false},
		{"field comment, no doc comments", "type K struct {\n\t// A counts.\n\tA int\n}\n", "type K struct {\n\t// A measures.\n\tA int\n}\n", true, false},
		{"field comment", "type K struct {\n\t// A counts.\n\tA int\n}\n", "type K struct {\n\t// A measures.\n\tA int\n}\n", false, true},
		{"same doc comment", "// K counts.\ntype K struct {\n\tA int\n}\n", "// K counts.\ntype (\n\t// K counts.\n\tK struct {\n\t\tA int\n\t}\n)\n", true, true},
		{"doc comment, no doc comments", "// K counts.\ntype K struct {\n\tA int\n}\n", "// K measures.\ntype K struct {\n\tA int\n}\n", true, false},
		{"doc comment", "// K counts.\ntype K struct {\n\tA int\n}\n", "// K measures.\ntype K struct {\n\tA int\n}\n", false, true},
		{"deprecated field, no doc comments", "type K struct {\n\tA int\n}\n", "type K struct {\n\t// Deprecated: use B.\n\tA int\n}\n", false, false},
		{"deprecated field", "type K struct {\n\tA int\n}\n", "type K struct {\n\t// Deprecated: use B.\n\tA int\n}\n", false, true},
	}
	for _, c := range cases {
		files := map[string]string{
			"k_unix.go":    "//go:build !windows\n\npackage things\n\n" + c.unix,
			"k_windows.go": "package things\n\n" + c.windows,
		}
		builder := buildThingsFiles(t, files, nil)
		builder.docComments = c.docComments
		pkg, err := builder.loadPackage("example.com/app/things")
		if err != nil {
			t.Fatal(err)
		}
		reason, refused := pkg.RefusedTypes["K"]
		if c.same && refused {
			t.Errorf("%s: expected one declaration, got %q", c.name, reason)
		}
		expected := "type K is declared differently in k_unix.go, k_windows.go, one per platform, and the generator does not pick one"
		if !c.same && !strings.Contains(reason, expected) {
			t.Errorf("%s: expected %q, got %q", c.name, expected, reason)
		}
	}
}
