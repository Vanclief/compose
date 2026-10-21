package generator

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// buildThings writes source as the package example.com/app/things and builds the schema of
// each named type, returning the builder holding the components.
func buildThings(t *testing.T, source string, names ...string) *schemaBuilder {
	t.Helper()

	repoRoot := t.TempDir()
	pkgDir := filepath.Join(repoRoot, "things")
	err := os.MkdirAll(pkgDir, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(filepath.Join(pkgDir, "things.go"), []byte(source), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	builder := newTestSchemaBuilder(t, repoRoot)
	for _, name := range names {
		builder.schemaRef(typeRef{ImportPath: "example.com/app/things", Name: name})
	}

	return builder
}

func componentProperties(t *testing.T, builder *schemaBuilder, name string) map[string]any {
	t.Helper()

	schema := builder.schemas[name]
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("expected %s to have properties, got %v", name, schema)
	}

	return properties
}

// An unnamed import is referred to by its package name, not by a major version element.
func TestImportMapNamesVersionedImports(t *testing.T) {
	source := `package things

import (
	"math/rand/v2"

	"github.com/gofrs/uuid/v5"
	"github.com/labstack/echo/v4"
	"gopkg.in/guregu/null.v4"
	"gopkg.in/yaml.v3"
	pg "github.com/go-pg/pg/v10"
	"github.com/mattn/go-sqlite3"
	"example.com/app/things"
)
`
	file, err := parser.ParseFile(token.NewFileSet(), "things.go", source, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]string{
		"rand":    "math/rand/v2",
		"uuid":    "github.com/gofrs/uuid/v5",
		"echo":    "github.com/labstack/echo/v4",
		"null":    "gopkg.in/guregu/null.v4",
		"yaml":    "gopkg.in/yaml.v3",
		"pg":      "github.com/go-pg/pg/v10",
		"sqlite3": "github.com/mattn/go-sqlite3",
		"things":  "example.com/app/things",
	}
	got := newTestSchemaBuilder(t, t.TempDir()).importMap(file)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
}

// Fields typed from versioned imports reference those types instead of falling back to an
// opaque object.
func TestVersionedImportTypesResolve(t *testing.T) {
	root := writeFixture(t, thingFixture(`package things

import (
	"context"

	"github.com/gofrs/uuid/v5"
	"gopkg.in/guregu/null.v4"
)

type Thing struct {
	ID   uuid.UUID   `+"`json:\"id\"`"+`
	Note null.String `+"`json:\"note\"`"+`
}

type GetRequest struct{}

func Get(ctx context.Context, request *GetRequest) (*Thing, error) { return nil, nil }
`, nil))

	cfg := thingConfig(root)
	cfg.TypeSchemas = map[string]map[string]any{
		"github.com/gofrs/uuid/v5.UUID":  {"type": "string", "format": "uuid"},
		"gopkg.in/guregu/null.v4.String": {"type": "string", "nullable": true},
	}
	spec, err := Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}

	properties := componentSchemas(spec.Document)["things_Thing"]["properties"].(map[string]any)
	want := map[string]any{
		"id":   map[string]any{"$ref": "#/components/schemas/github_com_gofrs_uuid_v5_UUID"},
		"note": map[string]any{"$ref": "#/components/schemas/gopkg_in_guregu_null_v4_String"},
	}
	if !reflect.DeepEqual(properties, want) {
		t.Fatalf("expected %v, got %v", want, properties)
	}
}

// A module path without a dot is the module's own, not the standard library: its types are
// modeled, and its marshalers still refuse to be guessed.
func TestDotlessModuleTypesAreModeled(t *testing.T) {
	repoRoot := t.TempDir()
	pkgDir := filepath.Join(repoRoot, "models", "thing")
	err := os.MkdirAll(pkgDir, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	source := `package thing

import "encoding/json"

type Thing struct {
	ID int64 ` + "`json:\"id\"`" + `
}

type Custom struct{}

func (c Custom) MarshalJSON() ([]byte, error) { return json.Marshal("x") }

type Wrapper struct {
	Custom
}
`
	err = os.WriteFile(filepath.Join(pkgDir, "thing.go"), []byte(source), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	builder := newTestSchemaBuilder(t, repoRoot)
	builder.modulePath = "emma"

	builder.schemaRef(typeRef{ImportPath: "emma/models/thing", Name: "Thing"})
	err = builder.unresolvedError()
	if err != nil {
		t.Fatalf("expected the own type to resolve, got %v", err)
	}
	want := map[string]any{"id": map[string]any{"type": "integer", "format": "int64"}}
	if !reflect.DeepEqual(builder.schemas["models_thing_Thing"]["properties"], want) {
		t.Fatalf("expected the own type modeled, got %v", builder.schemas["models_thing_Thing"])
	}

	builder.schemaRef(typeRef{ImportPath: "emma/models/thing", Name: "Wrapper"})
	err = builder.unresolvedError()
	if err == nil || !strings.Contains(err.Error(), "embeds Custom, which defines MarshalJSON") {
		t.Fatalf("expected the embedded own marshaler reported, got %v", err)
	}

	// The standard library stays opaque.
	stdlib := newTestSchemaBuilder(t, repoRoot)
	stdlib.modulePath = "emma"
	stdlib.schemaRef(typeRef{ImportPath: "net/url", Name: "URL"})
	err = stdlib.unresolvedError()
	if err != nil {
		t.Fatalf("expected net/url to stay opaque, got %v", err)
	}
	if !reflect.DeepEqual(stdlib.schemas["net_url_URL"], map[string]any{"type": "object"}) {
		t.Fatalf("expected an opaque net/url.URL, got %v", stdlib.schemas["net_url_URL"])
	}
}

// A dependency whose module path has no dot (a replaced local module) is not the standard
// library either.
func TestDotlessDependencyIsNotStandardLibrary(t *testing.T) {
	builder := &schemaBuilder{
		modulePath:     "emma",
		moduleVersions: map[string]string{"shared": "v0.0.0", "example.com/dep": "v1.0.0"},
	}

	cases := map[string]bool{
		"net/url":           true,
		"time":              true,
		"emma":              false,
		"emma/models/thing": false,
		"emmaline/thing":    true,
		"shared/types":      false,
		"example.com/dep/x": false,
		"github.com/x/y":    false,
		"":                  false,
	}
	for importPath, want := range cases {
		got := builder.isStandardLibraryImport(importPath)
		if got != want {
			t.Errorf("%q: expected %v, got %v", importPath, want, got)
		}
	}
}

// Embedded fields follow encoding/json: a struct is inlined unless its tag names it, "-" drops
// it, and any other type is a property named after the type.
func TestEmbeddedFieldsFollowEncodingJSON(t *testing.T) {
	builder := buildThings(t, `package things

type Base struct {
	ID int64 `+"`json:\"id\"`"+`
}

type Code string

type secret string

type Shape interface{}

type Inlined struct {
	Base
	*Audit
	Name string `+"`json:\"name\"`"+`
}

type Audit struct {
	By string `+"`json:\"by\"`"+`
}

type Tagged struct {
	Base  `+"`json:\"base\"`"+`
	*Audit `+"`json:\"audit,omitempty\"`"+`
}

type Skipped struct {
	Base `+"`json:\"-\"`"+`
	Name string `+"`json:\"name\"`"+`
}

type NonStruct struct {
	Code
	secret
	Shape
	Labels
}

type Labels []string
`, "Inlined", "Tagged", "Skipped", "NonStruct")

	err := builder.unresolvedError()
	if err != nil {
		t.Fatal(err)
	}

	baseRef := map[string]any{"$ref": "#/components/schemas/things_Base"}
	auditRef := map[string]any{"$ref": "#/components/schemas/things_Audit"}

	inlined := builder.schemas["things_Inlined"]
	wantInlined := map[string]any{
		"allOf": []any{
			baseRef,
			auditRef,
			map[string]any{
				"type":       "object",
				"properties": map[string]any{"name": map[string]any{"type": "string"}},
				"required":   []string{"name"},
			},
		},
	}
	if !reflect.DeepEqual(inlined, wantInlined) {
		t.Fatalf("expected embedded structs inlined, got %v", inlined)
	}

	tagged := builder.schemas["things_Tagged"]
	wantTagged := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"base":  baseRef,
			"audit": map[string]any{"allOf": []any{auditRef}, "nullable": true},
		},
		"required": []string{"base"},
	}
	if !reflect.DeepEqual(tagged, wantTagged) {
		t.Fatalf("expected tagged embeds as named properties, got %v", tagged)
	}

	skipped := builder.schemas["things_Skipped"]
	wantSkipped := map[string]any{
		"type":       "object",
		"properties": map[string]any{"name": map[string]any{"type": "string"}},
		"required":   []string{"name"},
	}
	if !reflect.DeepEqual(skipped, wantSkipped) {
		t.Fatalf("expected the json:\"-\" embed dropped, got %v", skipped)
	}

	nonStruct := builder.schemas["things_NonStruct"]
	wantNonStruct := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"Code":   map[string]any{"$ref": "#/components/schemas/things_Code"},
			"Shape":  map[string]any{"$ref": "#/components/schemas/things_Shape"},
			"Labels": map[string]any{"$ref": "#/components/schemas/things_Labels"},
		},
		"required": []string{"Code"},
	}
	if !reflect.DeepEqual(nonStruct, wantNonStruct) {
		t.Fatalf("expected non-struct embeds as properties named after their type, got %v", nonStruct)
	}
}

// Embedding a type that marshals itself (time.Time) promotes its marshaler: the embedding
// struct marshals as that type alone, so the generator refuses to guess.
func TestEmbeddedSelfMarshalingTypeIsUnresolved(t *testing.T) {
	builder := buildThings(t, `package things

import "time"

type Stamp struct {
	time.Time
	Zone string `+"`json:\"zone\"`"+`
}
`, "Stamp")

	err := builder.unresolvedError()
	if err == nil || !strings.Contains(err.Error(), "type Stamp embeds Time") {
		t.Fatalf("expected the embedded time.Time reported, got %v", err)
	}
}

// Only a byte slice is base64; a byte array marshals as an array of numbers, and a lone byte
// as a number.
func TestByteArraysAreArraysOfNumbers(t *testing.T) {
	builder := buildThings(t, `package things

const KeySize = 16

type Thing struct {
	Digest [32]byte      `+"`json:\"digest\"`"+`
	Key    [KeySize]byte `+"`json:\"key\"`"+`
	Raw    []byte        `+"`json:\"raw\"`"+`
	Octets []uint8       `+"`json:\"octets\"`"+`
	Blobs  [][]byte      `+"`json:\"blobs\"`"+`
	Single byte          `+"`json:\"single\"`"+`
}
`, "Thing")

	byteSchema := map[string]any{"type": "integer", "format": "int32"}
	base64 := map[string]any{"type": "string", "format": "byte"}
	want := map[string]any{
		"digest": map[string]any{"type": "array", "items": byteSchema, "minItems": 32, "maxItems": 32},
		"key":    map[string]any{"type": "array", "items": byteSchema},
		"raw":    base64,
		"octets": base64,
		"blobs":  map[string]any{"type": "array", "items": base64},
		"single": byteSchema,
	}
	properties := componentProperties(t, builder, "things_Thing")
	if !reflect.DeepEqual(properties, want) {
		t.Fatalf("expected %v, got %v", want, properties)
	}

	// A fixed array is never null, so it is always serialized.
	required := builder.schemas["things_Thing"]["required"]
	if !reflect.DeepEqual(required, []string{"digest", "key", "single"}) {
		t.Fatalf("expected the arrays and the byte required, got %v", required)
	}
}

// Every name of a field declaration is a property, each with the shared tag and doc.
func TestFieldDeclarationsWithSeveralNames(t *testing.T) {
	builder := buildThings(t, `package things

type Thing struct {
	Width, Height int64
	First, Last   string `+"`json:\",omitempty\"`"+`
	hidden, Shown bool
	// Deprecated: read size.
	Old, Older string
}
`, "Thing")

	deprecatedString := map[string]any{"type": "string", "deprecated": true}
	want := map[string]any{
		"Width":  map[string]any{"type": "integer", "format": "int64"},
		"Height": map[string]any{"type": "integer", "format": "int64"},
		"First":  map[string]any{"type": "string"},
		"Last":   map[string]any{"type": "string"},
		"Shown":  map[string]any{"type": "boolean"},
		"Old":    deprecatedString,
		"Older":  deprecatedString,
	}
	properties := componentProperties(t, builder, "things_Thing")
	if !reflect.DeepEqual(properties, want) {
		t.Fatalf("expected %v, got %v", want, properties)
	}

	required := builder.schemas["things_Thing"]["required"]
	if !reflect.DeepEqual(required, []string{"Width", "Height", "Shown", "Old", "Older"}) {
		t.Fatalf("expected every non-omitempty name required, got %v", required)
	}
}

// Path fields of a field declaration with several names map to their JSON names.
func TestJSONFieldNamesCoverEveryName(t *testing.T) {
	builder := buildThings(t, `package things

type GetRequest struct {
	OrgID, ThingID int64 `+"`json:\",omitempty\"`"+`
}
`)

	names := builder.jsonFieldNames(typeRef{ImportPath: "example.com/app/things", Name: "GetRequest"}, []string{"ThingID"})
	if !reflect.DeepEqual(names, []string{"ThingID"}) {
		t.Fatalf("expected ThingID, got %v", names)
	}
}

// The ,string option puts a boolean, number or string inside a JSON string.
func TestStringOptionIsAString(t *testing.T) {
	builder := buildThings(t, `package things

import "encoding/json"

type Count int

type Kind string

type Level int

func (l Level) MarshalJSON() ([]byte, error) { return json.Marshal(int(l)) }

type Thing struct {
	N      int64   `+"`json:\"n,string\"`"+`
	P      *int    `+"`json:\"p,string\"`"+`
	F      float64 `+"`json:\"f,string\"`"+`
	OK     bool    `+"`json:\"ok,string,omitempty\"`"+`
	S      string  `+"`json:\"s,string\"`"+`
	Count  Count   `+"`json:\"count,string\"`"+`
	Kind   Kind    `+"`json:\"kind,string\"`"+`
	Level  Level   `+"`json:\"level,string\"`"+`
	Tags   []int   `+"`json:\"tags,string\"`"+`
	Plain  int64   `+"`json:\"plain\"`"+`
}
`, "Thing")

	stringSchema := map[string]any{"type": "string"}
	want := map[string]any{
		"n":     stringSchema,
		"p":     map[string]any{"type": "string", "nullable": true},
		"f":     stringSchema,
		"ok":    stringSchema,
		"s":     stringSchema,
		"count": stringSchema,
		"kind":  stringSchema,
		"level": map[string]any{"$ref": "#/components/schemas/things_Level"},
		"tags":  map[string]any{"type": "array", "items": map[string]any{"type": "integer", "format": "int64"}},
		"plain": map[string]any{"type": "integer", "format": "int64"},
	}
	properties := componentProperties(t, builder, "things_Thing")
	if !reflect.DeepEqual(properties, want) {
		t.Fatalf("expected %v, got %v", want, properties)
	}

	required := builder.schemas["things_Thing"]["required"]
	if !reflect.DeepEqual(required, []string{"n", "f", "s", "count", "kind", "level", "plain"}) {
		t.Fatalf("expected the value-typed fields required, got %v", required)
	}
}

// An unnamed import is referred to by the name in its package clause, even when that name is a
// major version element (aws-sdk-go-v2's signer/v4, apimachinery's meta/v1).
func TestVersionNamedPackagesResolve(t *testing.T) {
	root := writeFixture(t, thingFixture(`package things

import (
	"context"

	"example.com/app/aws/signer/v4"
	"example.com/app/apis/meta/v1"
)

type Thing struct {
	Signature v4.Signature `+"`json:\"signature\"`"+`
	Meta      v1.ObjectMeta `+"`json:\"meta\"`"+`
}

type GetRequest struct{}

func Get(ctx context.Context, request *GetRequest) (*Thing, error) { return nil, nil }
`, map[string]string{
		"aws/signer/v4/signer.go": "package v4\n\ntype Signature struct {\n\tValue string `json:\"value\"`\n}\n",
		"apis/meta/v1/meta.go":    "package v1\n\ntype ObjectMeta struct {\n\tName string `json:\"name\"`\n}\n",
	}))

	spec, err := Generate(thingConfig(root))
	if err != nil {
		t.Fatal(err)
	}

	schemas := componentSchemas(spec.Document)
	properties := schemas["things_Thing"]["properties"].(map[string]any)
	want := map[string]any{
		"signature": map[string]any{"$ref": "#/components/schemas/aws_signer_v4_Signature"},
		"meta":      map[string]any{"$ref": "#/components/schemas/apis_meta_v1_ObjectMeta"},
	}
	if !reflect.DeepEqual(properties, want) {
		t.Fatalf("expected %v, got %v", want, properties)
	}
	_, ok := schemas["aws_signer_v4_Signature"]["properties"].(map[string]any)["value"]
	if !ok {
		t.Fatalf("expected the v4 Signature fields, got %v", schemas["aws_signer_v4_Signature"])
	}
}

// A selector whose qualifier is not among the file's imports fails generation instead of
// publishing an opaque object.
func TestUnknownQualifierIsUnresolved(t *testing.T) {
	root := writeFixture(t, thingFixture(`package things

import "context"

type Thing struct {
	Other missing.Other `+"`json:\"other\"`"+`
}

type GetRequest struct{}

func Get(ctx context.Context, request *GetRequest) (*Thing, error) { return nil, nil }
`, nil))

	_, err := Generate(thingConfig(root))
	if err == nil {
		t.Fatal("expected an unknown qualifier to fail generation")
	}
	if !strings.Contains(err.Error(), "missing.Other") {
		t.Fatalf("expected the error to name missing.Other, got %v", err)
	}
}

// A dependency's package is named by its package clause too, skipping test files and a
// go:build ignore main beside it.
func TestImportMapReadsDependencyPackageClause(t *testing.T) {
	builder := newTestSchemaBuilder(t, t.TempDir())
	pkgDir := filepath.Join(builder.modCache, "example.com", "dep@v1.2.3", "apis", "meta", "v1")
	err := os.MkdirAll(pkgDir, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	sources := map[string]string{
		"gen.go":       "//go:build ignore\n\npackage main\n",
		"meta.go":      "package v1\n\ntype ObjectMeta struct{}\n",
		"meta_test.go": "package v1_test\n",
	}
	for name, source := range sources {
		err = os.WriteFile(filepath.Join(pkgDir, name), []byte(source), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	source := `package things

import (
	"example.com/dep/apis/meta/v1"
	"github.com/gofrs/uuid/v5"
)
`
	file, err := parser.ParseFile(token.NewFileSet(), "things.go", source, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]string{
		"v1":   "example.com/dep/apis/meta/v1",
		"uuid": "github.com/gofrs/uuid/v5",
	}
	got := builder.importMap(file)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
}
