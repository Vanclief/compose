package generator

import (
	"bytes"
	"context"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"database/sql/driver"
	"debug/elf"
	"encoding"
	"encoding/asn1"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
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
	"github.com/labstack/echo/v5"
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
		"echo":    "github.com/labstack/echo/v5",
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
	Audit
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
			"Labels": map[string]any{"allOf": []any{map[string]any{"$ref": "#/components/schemas/things_Labels"}}, "nullable": true},
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
	// A nil byte slice is written null, so a []byte field is nullable; an item is not.
	nullableBase64 := map[string]any{"type": "string", "format": "byte", "nullable": true}
	want := map[string]any{
		"digest": map[string]any{"type": "array", "items": byteSchema, "minItems": 32, "maxItems": 32},
		"key":    map[string]any{"type": "array", "items": byteSchema},
		"raw":    nullableBase64,
		"octets": nullableBase64,
		"blobs":  map[string]any{"type": "array", "items": base64, "nullable": true},
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
		"tags":  map[string]any{"type": "array", "items": map[string]any{"type": "integer", "format": "int64"}, "nullable": true},
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

// marshalled is what encoding/json writes for value, decoded back into plain JSON values.
func marshalled(t *testing.T, value any) any {
	t.Helper()

	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var decoded any
	err = json.Unmarshal(data, &decoded)
	if err != nil {
		t.Fatal(err)
	}

	return decoded
}

// assertConforms fails the test where value, as encoding/json wrote it, breaks schema.
func assertConforms(t *testing.T, builder *schemaBuilder, schema map[string]any, value any) {
	t.Helper()

	for _, problem := range nonConformities(builder.schemas, schema, value, "$") {
		t.Error(problem)
	}
}

// nonConformities lists where a decoded JSON value breaks a schema, for the OpenAPI 3.0
// keywords the generator writes: $ref, allOf, type, nullable, properties, required, items and
// additionalProperties.
func nonConformities(schemas map[string]map[string]any, schema map[string]any, value any, at string) []string {
	ref, isRef := schema["$ref"].(string)
	if isRef {
		component, ok := schemas[strings.TrimPrefix(ref, schemaRefPrefix)]
		if !ok {
			return []string{at + ": no component " + ref}
		}
		return nonConformities(schemas, component, value, at)
	}
	if value == nil {
		if len(schema) == 0 || schema["nullable"] == true {
			return nil
		}
		return []string{at + ": null where the schema is not nullable"}
	}

	var problems []string
	allOf, _ := schema["allOf"].([]any)
	for _, entry := range allOf {
		member, _ := entry.(map[string]any)
		problems = append(problems, nonConformities(schemas, member, value, at)...)
	}

	kind, _ := schema["type"].(string)
	switch kind {
	case "string":
		_, ok := value.(string)
		if !ok {
			problems = append(problems, at+": not a string")
		}
	case "boolean":
		_, ok := value.(bool)
		if !ok {
			problems = append(problems, at+": not a boolean")
		}
	case "integer", "number":
		number, ok := value.(float64)
		if !ok || (kind == "integer" && number != float64(int64(number))) {
			problems = append(problems, at+": not an "+kind)
		}
	case "array":
		items, ok := value.([]any)
		if !ok {
			problems = append(problems, at+": not an array")
			break
		}
		itemSchema, _ := schema["items"].(map[string]any)
		for i, item := range items {
			problems = append(problems, nonConformities(schemas, itemSchema, item, at+"["+strconv.Itoa(i)+"]")...)
		}
	case "object":
		object, ok := value.(map[string]any)
		if !ok {
			problems = append(problems, at+": not an object")
			break
		}
		required, _ := schema["required"].([]string)
		for _, name := range required {
			_, present := object[name]
			if !present {
				problems = append(problems, at+": required "+name+" is missing")
			}
		}
		properties, _ := schema["properties"].(map[string]any)
		additional, _ := schema["additionalProperties"].(map[string]any)
		for name, nested := range object {
			property, declared := properties[name].(map[string]any)
			if !declared {
				property = additional
			}
			if property != nil {
				problems = append(problems, nonConformities(schemas, property, nested, at+"."+name)...)
			}
		}
	}

	return problems
}

type textLevel int

func (l textLevel) MarshalText() ([]byte, error) {
	if l > 1 {
		return []byte("high"), nil
	}
	return []byte("low"), nil
}

type textPoint struct {
	X int
	Y int
}

func (p *textPoint) MarshalText() ([]byte, error) {
	return []byte(strconv.Itoa(p.X) + "," + strconv.Itoa(p.Y)), nil
}

type textBoth int

func (b textBoth) MarshalJSON() ([]byte, error) { return []byte(strconv.Itoa(int(b))), nil }

func (b textBoth) MarshalText() ([]byte, error) { return []byte("text"), nil }

type textThing struct {
	Level   textLevel         `json:"level"`
	Point   textPoint         `json:"point"`
	Ptr     *textPoint        `json:"ptr"`
	Points  []textPoint       `json:"points"`
	ByLevel map[textLevel]int `json:"by_level"`
	Both    textBoth          `json:"both"`
}

// A type with MarshalText and without MarshalJSON is the string MarshalText writes, whatever its
// kind and receiver; MarshalJSON wins over it; a map keyed by it is still an object.
func TestMarshalTextTypesAreStrings(t *testing.T) {
	builder := buildThings(t, `package things

import "strconv"

type Level int

func (l Level) MarshalText() ([]byte, error) { return []byte("high"), nil }

type Point struct {
	X int
	Y int
}

func (p *Point) MarshalText() ([]byte, error) { return []byte(strconv.Itoa(p.X)), nil }

type Both int

func (b Both) MarshalJSON() ([]byte, error) { return []byte("1"), nil }

func (b Both) MarshalText() ([]byte, error) { return []byte("text"), nil }

type Thing struct {
	Level   Level         `+"`json:\"level\"`"+`
	Point   Point         `+"`json:\"point\"`"+`
	Ptr     *Point        `+"`json:\"ptr\"`"+`
	Points  []Point       `+"`json:\"points\"`"+`
	ByLevel map[Level]int `+"`json:\"by_level\"`"+`
	Both    Both          `+"`json:\"both\"`"+`
}
`, "Thing")

	err := builder.unresolvedError()
	if err != nil {
		t.Fatal(err)
	}

	stringSchema := map[string]any{"type": "string"}
	if !reflect.DeepEqual(builder.schemas["things_Level"], stringSchema) {
		t.Errorf("expected Level a string, got %v", builder.schemas["things_Level"])
	}
	if !reflect.DeepEqual(builder.schemas["things_Point"], stringSchema) {
		t.Errorf("expected Point a string, got %v", builder.schemas["things_Point"])
	}
	both := map[string]any{"type": "integer", "format": "int64"}
	if !reflect.DeepEqual(builder.schemas["things_Both"], both) {
		t.Errorf("expected MarshalJSON to win over MarshalText, got %v", builder.schemas["things_Both"])
	}
	required := builder.schemas["things_Thing"]["required"]
	if !reflect.DeepEqual(required, []string{"level", "point", "both"}) {
		t.Errorf("expected the value-typed fields required, got %v", required)
	}

	thing := &textThing{
		Level:   2,
		Point:   textPoint{X: 1, Y: 2},
		Ptr:     &textPoint{X: 3, Y: 4},
		Points:  []textPoint{{X: 5, Y: 6}},
		ByLevel: map[textLevel]int{2: 1},
		Both:    7,
	}
	value := marshalled(t, thing)
	want := map[string]any{
		"level":    "high",
		"point":    "1,2",
		"ptr":      "3,4",
		"points":   []any{"5,6"},
		"by_level": map[string]any{"high": float64(1)},
		"both":     float64(7),
	}
	if !reflect.DeepEqual(value, want) {
		t.Fatalf("expected encoding/json to write %v, got %v", want, value)
	}
	assertConforms(t, builder, builder.schemas["things_Thing"], value)
}

// A struct that embeds a type with MarshalText marshals as that type's text alone, so the
// generator refuses to guess, unless the struct defines its own MarshalText.
func TestEmbeddedMarshalTextIsUnresolved(t *testing.T) {
	builder := buildThings(t, `package things

type Level int

func (l Level) MarshalText() ([]byte, error) { return []byte("high"), nil }

type Stamped struct {
	Level
	Note string `+"`json:\"note\"`"+`
}
`, "Stamped")

	err := builder.unresolvedError()
	if err == nil || !strings.Contains(err.Error(), "type Stamped embeds Level, which defines MarshalText") {
		t.Fatalf("expected the promoted MarshalText reported, got %v", err)
	}

	value := marshalled(t, struct {
		textLevel
		Note string `json:"note"`
	}{textLevel: 2, Note: "x"})
	if value != "high" {
		t.Fatalf("expected encoding/json to write the embedded text alone, got %v", value)
	}

	own := buildThings(t, `package things

type Level int

func (l Level) MarshalText() ([]byte, error) { return []byte("high"), nil }

type Own struct {
	Level
	Note string `+"`json:\"note\"`"+`
}

func (o Own) MarshalText() ([]byte, error) { return []byte(o.Note), nil }
`, "Own")
	err = own.unresolvedError()
	if err != nil {
		t.Fatalf("expected the own MarshalText to shadow the embedded one, got %v", err)
	}
	if !reflect.DeepEqual(own.schemas["things_Own"], map[string]any{"type": "string"}) {
		t.Fatalf("expected Own a string, got %v", own.schemas["things_Own"])
	}
}

type sqlNullThing struct {
	Bool    sql.NullBool    `json:"bool"`
	Byte    sql.NullByte    `json:"byte"`
	Float64 sql.NullFloat64 `json:"float64"`
	Int16   sql.NullInt16   `json:"int16"`
	Int32   sql.NullInt32   `json:"int32"`
	Int64   sql.NullInt64   `json:"int64"`
	String  sql.NullString  `json:"string"`
	Time    sql.NullTime    `json:"time"`
	Generic sql.Null[int64] `json:"generic"`
	Ptr     *sql.NullString `json:"ptr"`
}

// database/sql's Null types define no MarshalJSON, so encoding/json writes them as the structs
// they are, {"String": "", "Valid": false}, never null.
func TestSQLNullTypesAreTheirStructs(t *testing.T) {
	builder := buildThings(t, `package things

import "database/sql"

type Thing struct {
	Bool    sql.NullBool    `+"`json:\"bool\"`"+`
	Byte    sql.NullByte    `+"`json:\"byte\"`"+`
	Float64 sql.NullFloat64 `+"`json:\"float64\"`"+`
	Int16   sql.NullInt16   `+"`json:\"int16\"`"+`
	Int32   sql.NullInt32   `+"`json:\"int32\"`"+`
	Int64   sql.NullInt64   `+"`json:\"int64\"`"+`
	String  sql.NullString  `+"`json:\"string\"`"+`
	Time    sql.NullTime    `+"`json:\"time\"`"+`
	Generic sql.Null[int64] `+"`json:\"generic\"`"+`
	Ptr     *sql.NullString `+"`json:\"ptr\"`"+`
}
`, "Thing")

	err := builder.unresolvedError()
	if err != nil {
		t.Fatal(err)
	}

	wantString := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"String": map[string]any{"type": "string"},
			"Valid":  map[string]any{"type": "boolean"},
		},
		"required": []string{"String", "Valid"},
	}
	if !reflect.DeepEqual(builder.schemas["database_sql_NullString"], wantString) {
		t.Errorf("expected sql.NullString as its struct, got %v", builder.schemas["database_sql_NullString"])
	}
	wantGeneric := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"V":     map[string]any{"type": "integer", "format": "int64"},
			"Valid": map[string]any{"type": "boolean"},
		},
		"required": []string{"V", "Valid"},
	}
	properties := componentProperties(t, builder, "things_Thing")
	if !reflect.DeepEqual(properties["generic"], wantGeneric) {
		t.Errorf("expected sql.Null[int64] as its struct, got %v", properties["generic"])
	}
	wantPtr := map[string]any{"allOf": []any{map[string]any{"$ref": "#/components/schemas/database_sql_NullString"}}, "nullable": true}
	if !reflect.DeepEqual(properties["ptr"], wantPtr) {
		t.Errorf("expected a pointer to sql.NullString nullable, got %v", properties["ptr"])
	}
	required := builder.schemas["things_Thing"]["required"]
	wantRequired := []string{"bool", "byte", "float64", "int16", "int32", "int64", "string", "time", "generic"}
	if !reflect.DeepEqual(required, wantRequired) {
		t.Errorf("expected every sql.Null value field required, got %v", required)
	}

	empty := marshalled(t, &sqlNullThing{})
	if !reflect.DeepEqual(empty.(map[string]any)["string"], map[string]any{"String": "", "Valid": false}) {
		t.Fatalf("expected encoding/json to write the struct, got %v", empty)
	}
	assertConforms(t, builder, builder.schemas["things_Thing"], empty)

	set := &sqlNullThing{
		Bool:    sql.NullBool{Bool: true, Valid: true},
		Byte:    sql.NullByte{Byte: 7, Valid: true},
		Float64: sql.NullFloat64{Float64: 1.5, Valid: true},
		Int16:   sql.NullInt16{Int16: 16, Valid: true},
		Int32:   sql.NullInt32{Int32: 32, Valid: true},
		Int64:   sql.NullInt64{Int64: 64, Valid: true},
		String:  sql.NullString{String: "x", Valid: true},
		Time:    sql.NullTime{Time: time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC), Valid: true},
		Generic: sql.Null[int64]{V: 9, Valid: true},
		Ptr:     &sql.NullString{String: "y", Valid: true},
	}
	assertConforms(t, builder, builder.schemas["things_Thing"], marshalled(t, set))
}

type embedInner struct {
	Name string `json:"name"`
	Code string `json:"code"`
}

type embedShadow struct {
	embedInner
	Name int `json:"name"`
}

type EmbedA struct {
	X string `json:"x"`
	Y string `json:"y"`
}

type EmbedB struct {
	X    int `json:"x"`
	Name int
}

type EmbedC struct {
	Name string `json:"Name"`
}

// embedTie is struct{EmbedA; EmbedB; EmbedC}, built through reflect because go vet refuses a
// declared struct whose embedded fields repeat a json tag, the collision under test.
func embedTie(a EmbedA, b EmbedB, c EmbedC) any {
	tie := reflect.StructOf([]reflect.StructField{
		{Name: "EmbedA", Type: reflect.TypeOf(a), Anonymous: true},
		{Name: "EmbedB", Type: reflect.TypeOf(b), Anonymous: true},
		{Name: "EmbedC", Type: reflect.TypeOf(c), Anonymous: true},
	})
	value := reflect.New(tie)
	value.Elem().Field(0).Set(reflect.ValueOf(a))
	value.Elem().Field(1).Set(reflect.ValueOf(b))
	value.Elem().Field(2).Set(reflect.ValueOf(c))

	return value.Interface()
}

// embedOuter is struct{Tie; Z string `json:"z"`} around an embedTie value.
func embedOuter(tie any, z string) any {
	outer := reflect.StructOf([]reflect.StructField{
		{Name: "Tie", Type: reflect.TypeOf(tie).Elem(), Anonymous: true},
		{Name: "Z", Type: reflect.TypeOf(z), Tag: `json:"z"`},
	})
	value := reflect.New(outer)
	value.Elem().Field(0).Set(reflect.ValueOf(tie).Elem())
	value.Elem().Field(1).SetString(z)

	return value.Interface()
}

type embedPointer struct {
	*embedInner
	Extra string `json:"extra"`
}

// Embedded structs resolve the way encoding/json resolves them: the shallowest name wins, a tie
// at one depth goes to the only tagged field or drops the name, and the fields of an embedded
// pointer are left out when it is nil.
func TestEmbeddedCollisionsFollowEncodingJSON(t *testing.T) {
	builder := buildThings(t, `package things

type Inner struct {
	Name string `+"`json:\"name\"`"+`
	Code string `+"`json:\"code\"`"+`
}

type Shadow struct {
	Inner
	Name int `+"`json:\"name\"`"+`
}

type A struct {
	X string `+"`json:\"x\"`"+`
	Y string `+"`json:\"y\"`"+`
}

type B struct {
	X    int `+"`json:\"x\"`"+`
	Name int
}

type C struct {
	Name string `+"`json:\"Name\"`"+`
}

type Tie struct {
	A
	B
	C
}

type Pointer struct {
	*Inner
	Extra string `+"`json:\"extra\"`"+`
}

type Outer struct {
	Tie
	Z string `+"`json:\"z\"`"+`
}
`, "Shadow", "Tie", "Pointer", "Outer")

	err := builder.unresolvedError()
	if err != nil {
		t.Fatal(err)
	}

	stringSchema := map[string]any{"type": "string"}
	integer := map[string]any{"type": "integer", "format": "int64"}

	// The outer name shadows the embedded one.
	wantShadow := map[string]any{
		"type":       "object",
		"properties": map[string]any{"name": integer, "code": stringSchema},
		"required":   []string{"code", "name"},
	}
	if !reflect.DeepEqual(builder.schemas["things_Shadow"], wantShadow) {
		t.Errorf("expected the outer name to win, got %v", builder.schemas["things_Shadow"])
	}
	shadow := marshalled(t, &embedShadow{embedInner: embedInner{Name: "inner", Code: "c"}, Name: 1})
	if !reflect.DeepEqual(shadow, map[string]any{"name": float64(1), "code": "c"}) {
		t.Fatalf("expected encoding/json to write the outer name, got %v", shadow)
	}
	assertConforms(t, builder, builder.schemas["things_Shadow"], shadow)

	// x is tagged in A and in B, so it is dropped; Name is tagged only in C, which wins.
	wantTie := map[string]any{
		"type":       "object",
		"properties": map[string]any{"y": stringSchema, "Name": stringSchema},
		"required":   []string{"y", "Name"},
	}
	if !reflect.DeepEqual(builder.schemas["things_Tie"], wantTie) {
		t.Errorf("expected x dropped and the tagged Name kept, got %v", builder.schemas["things_Tie"])
	}
	tieValue := embedTie(EmbedA{X: "a", Y: "y"}, EmbedB{X: 1, Name: 2}, EmbedC{Name: "c"})
	tie := marshalled(t, tieValue)
	if !reflect.DeepEqual(tie, map[string]any{"y": "y", "Name": "c"}) {
		t.Fatalf("expected encoding/json to drop x, got %v", tie)
	}
	assertConforms(t, builder, builder.schemas["things_Tie"], tie)

	// A nil embedded pointer leaves its fields out, so none of them is required.
	wantPointer := map[string]any{
		"type":       "object",
		"properties": map[string]any{"name": stringSchema, "code": stringSchema, "extra": stringSchema},
		"required":   []string{"extra"},
	}
	if !reflect.DeepEqual(builder.schemas["things_Pointer"], wantPointer) {
		t.Errorf("expected the embedded pointer's fields optional, got %v", builder.schemas["things_Pointer"])
	}
	nilPointer := marshalled(t, &embedPointer{Extra: "e"})
	if !reflect.DeepEqual(nilPointer, map[string]any{"extra": "e"}) {
		t.Fatalf("expected encoding/json to leave out a nil pointer's fields, got %v", nilPointer)
	}
	assertConforms(t, builder, builder.schemas["things_Pointer"], nilPointer)
	assertConforms(t, builder, builder.schemas["things_Pointer"], marshalled(t, &embedPointer{embedInner: &embedInner{Name: "n", Code: "c"}, Extra: "e"}))

	// A collision inside an embedded struct is resolved by that struct's own schema; the
	// embedding struct keeps its allOf.
	wantOuter := map[string]any{
		"allOf": []any{
			map[string]any{"$ref": "#/components/schemas/things_Tie"},
			map[string]any{
				"type":       "object",
				"properties": map[string]any{"z": stringSchema},
				"required":   []string{"z"},
			},
		},
	}
	if !reflect.DeepEqual(builder.schemas["things_Outer"], wantOuter) {
		t.Errorf("expected Outer to keep its allOf, got %v", builder.schemas["things_Outer"])
	}
	assertConforms(t, builder, builder.schemas["things_Outer"], marshalled(t, embedOuter(tieValue, "z")))
}

type nilLabels []string

type nilIndex map[string]int

type nilCustom []string

func (c nilCustom) MarshalJSON() ([]byte, error) {
	return json.Marshal([]string(append(nilCustom{}, c...)))
}

type nilThing struct {
	Tags     []string          `json:"tags"`
	M        map[string]string `json:"m"`
	Omitted  []string          `json:"omitted,omitempty"`
	Zero     map[string]int    `json:"zero,omitzero"`
	Fixed    [2]int            `json:"fixed"`
	Raw      []byte            `json:"raw"`
	Labels   nilLabels         `json:"labels"`
	Index    nilIndex          `json:"index"`
	Custom   nilCustom         `json:"custom"`
	Untagged []string
}

// encoding/json writes a nil slice or map as null, so a slice or map field without omitempty or
// omitzero is nullable; a fixed-size array never is, and a type with a marshaler writes what it
// writes. Whether a field is required does not change.
func TestNilSlicesAndMapsAreNullable(t *testing.T) {
	builder := buildThings(t, `package things

import "encoding/json"

type Labels []string

type Index map[string]int

type Custom []string

func (c Custom) MarshalJSON() ([]byte, error) { return json.Marshal([]string(c)) }

type Thing struct {
	Tags     []string          `+"`json:\"tags\"`"+`
	M        map[string]string `+"`json:\"m\"`"+`
	Omitted  []string          `+"`json:\"omitted,omitempty\"`"+`
	Zero     map[string]int    `+"`json:\"zero,omitzero\"`"+`
	Fixed    [2]int            `+"`json:\"fixed\"`"+`
	Raw      []byte            `+"`json:\"raw\"`"+`
	Labels   Labels            `+"`json:\"labels\"`"+`
	Index    Index             `+"`json:\"index\"`"+`
	Custom   Custom            `+"`json:\"custom\"`"+`
	Untagged []string
}
`, "Thing")

	err := builder.unresolvedError()
	if err != nil {
		t.Fatal(err)
	}

	stringItems := map[string]any{"type": "string"}
	integer := map[string]any{"type": "integer", "format": "int64"}
	want := map[string]any{
		"tags":     map[string]any{"type": "array", "items": stringItems, "nullable": true},
		"m":        map[string]any{"type": "object", "additionalProperties": stringItems, "nullable": true},
		"omitted":  map[string]any{"type": "array", "items": stringItems},
		"zero":     map[string]any{"type": "object", "additionalProperties": integer},
		"fixed":    map[string]any{"type": "array", "items": integer, "minItems": 2, "maxItems": 2},
		"raw":      map[string]any{"type": "string", "format": "byte", "nullable": true},
		"labels":   map[string]any{"allOf": []any{map[string]any{"$ref": "#/components/schemas/things_Labels"}}, "nullable": true},
		"index":    map[string]any{"allOf": []any{map[string]any{"$ref": "#/components/schemas/things_Index"}}, "nullable": true},
		"custom":   map[string]any{"$ref": "#/components/schemas/things_Custom"},
		"Untagged": map[string]any{"type": "array", "items": stringItems, "nullable": true},
	}
	properties := componentProperties(t, builder, "things_Thing")
	if !reflect.DeepEqual(properties, want) {
		t.Fatalf("expected %v, got %v", want, properties)
	}
	required := builder.schemas["things_Thing"]["required"]
	if !reflect.DeepEqual(required, []string{"fixed"}) {
		t.Fatalf("expected only the fixed array required, got %v", required)
	}

	// The components the nullable wrappers reference keep their own type, which
	// fillNullableTypes later copies beside nullable.
	fillNullableTypes(builder.schemas, builder.schemas)
	labels := builder.schemas["things_Thing"]["properties"].(map[string]any)["labels"].(map[string]any)
	if labels["type"] != "array" {
		t.Fatalf("expected the nullable named slice typed array, got %v", labels)
	}

	empty := marshalled(t, &nilThing{})
	wantEmpty := map[string]any{
		"tags":     nil,
		"m":        nil,
		"fixed":    []any{float64(0), float64(0)},
		"raw":      nil,
		"labels":   nil,
		"index":    nil,
		"custom":   []any{},
		"Untagged": nil,
	}
	if !reflect.DeepEqual(empty, wantEmpty) {
		t.Fatalf("expected encoding/json to write nil slices and maps as null, got %v", empty)
	}
	assertConforms(t, builder, builder.schemas["things_Thing"], empty)

	full := &nilThing{
		Tags:     []string{"a"},
		M:        map[string]string{"k": "v"},
		Omitted:  []string{"o"},
		Zero:     map[string]int{"z": 1},
		Raw:      []byte("raw"),
		Labels:   nilLabels{"l"},
		Index:    nilIndex{"i": 1},
		Custom:   nilCustom{"c"},
		Untagged: []string{"u"},
	}
	assertConforms(t, builder, builder.schemas["things_Thing"], marshalled(t, full))
}

type declPriority textLevel

type declArea textPoint

type declCSV []string

func (c declCSV) MarshalText() ([]byte, error) { return []byte(strings.Join(c, ",")), nil }

type declTags declCSV

type declJSONPoint struct {
	X int
	Y int
}

func (p declJSONPoint) MarshalJSON() ([]byte, error) { return []byte(`"custom"`), nil }

type declJSONArea declJSONPoint

type declAlias = textLevel

type declThing struct {
	Priority declPriority `json:"priority"`
	Quoted   declPriority `json:"quoted,string"`
	Area     declArea     `json:"area"`
	Tags     declTags     `json:"tags"`
	JSONArea declJSONArea `json:"json_area"`
	Alias    declAlias    `json:"alias"`
}

// A type declared from another named type does not inherit its methods, so it is written as the
// other type's underlying type, whatever marshalers that type has; an alias is the type itself.
func TestDeclaredFromMarshalingTypesAreTheirKind(t *testing.T) {
	builder := buildThings(t, `package things

import "strings"

type Level int

func (l Level) MarshalText() ([]byte, error) { return []byte("high"), nil }

type Priority Level

type Point struct {
	X int
	Y int
}

func (p *Point) MarshalText() ([]byte, error) { return []byte("1,2"), nil }

type Area Point

type CSV []string

func (c CSV) MarshalText() ([]byte, error) { return []byte(strings.Join(c, ",")), nil }

type Tags CSV

type JSONPoint struct {
	X int
	Y int
}

func (p JSONPoint) MarshalJSON() ([]byte, error) { return []byte("\"custom\""), nil }

type JSONArea JSONPoint

type Alias = Level

type Thing struct {
	Priority Priority `+"`json:\"priority\"`"+`
	Quoted   Priority `+"`json:\"quoted,string\"`"+`
	Area     Area     `+"`json:\"area\"`"+`
	Tags     Tags     `+"`json:\"tags\"`"+`
	JSONArea JSONArea `+"`json:\"json_area\"`"+`
	Alias    Alias    `+"`json:\"alias\"`"+`
}
`, "Thing")

	err := builder.unresolvedError()
	if err != nil {
		t.Fatal(err)
	}

	integer := map[string]any{"type": "integer", "format": "int64"}
	point := map[string]any{
		"type":       "object",
		"properties": map[string]any{"X": integer, "Y": integer},
		"required":   []string{"X", "Y"},
	}
	want := map[string]map[string]any{
		"things_Priority": integer,
		"things_Area":     point,
		"things_Tags":     {"type": "array", "items": map[string]any{"type": "string"}},
		"things_JSONArea": point,
	}
	for name, schema := range want {
		if !reflect.DeepEqual(builder.schemas[name], schema) {
			t.Errorf("%s: expected %v, got %v", name, schema, builder.schemas[name])
		}
	}
	properties := componentProperties(t, builder, "things_Thing")
	if !reflect.DeepEqual(properties["quoted"], map[string]any{"type": "string"}) {
		t.Errorf("expected the ,string option to quote a type declared from a marshaling type, got %v", properties["quoted"])
	}
	if !reflect.DeepEqual(builder.schemas["things_Alias"], map[string]any{"$ref": "#/components/schemas/things_Level"}) {
		t.Errorf("expected the alias to be Level, got %v", builder.schemas["things_Alias"])
	}

	value := marshalled(t, &declThing{
		Priority: 4,
		Quoted:   5,
		Area:     declArea{X: 1, Y: 2},
		Tags:     declTags{"a", "b"},
		JSONArea: declJSONArea{X: 3, Y: 4},
		Alias:    2,
	})
	wantValue := map[string]any{
		"priority":  float64(4),
		"quoted":    "5",
		"area":      map[string]any{"X": float64(1), "Y": float64(2)},
		"tags":      []any{"a", "b"},
		"json_area": map[string]any{"X": float64(3), "Y": float64(4)},
		"alias":     "high",
	}
	if !reflect.DeepEqual(value, wantValue) {
		t.Fatalf("expected encoding/json to write %v, got %v", wantValue, value)
	}
	assertConforms(t, builder, builder.schemas["things_Thing"], value)
}

type AmbiguousLevel int

func (l AmbiguousLevel) MarshalText() ([]byte, error) { return []byte("high"), nil }

type AmbiguousOther string

func (o AmbiguousOther) MarshalText() ([]byte, error) { return []byte("other"), nil }

type ambiguousBoth struct {
	AmbiguousLevel
	AmbiguousOther
}

// Two embedded types that both define MarshalText at the same depth make the method ambiguous:
// Go promotes neither, and the struct is written as its fields.
func TestAmbiguousEmbeddedMarshalTextIsNotPromoted(t *testing.T) {
	builder := buildThings(t, `package things

type Level int

func (l Level) MarshalText() ([]byte, error) { return []byte("high"), nil }

type Other string

func (o Other) MarshalText() ([]byte, error) { return []byte("other"), nil }

type Both struct {
	Level
	Other
}
`, "Both")

	err := builder.unresolvedError()
	if err != nil {
		t.Fatalf("expected an ambiguous MarshalText not to be promoted, got %v", err)
	}

	value := marshalled(t, &ambiguousBoth{AmbiguousLevel: 2, AmbiguousOther: "o"})
	if !reflect.DeepEqual(value, map[string]any{"AmbiguousLevel": "high", "AmbiguousOther": "other"}) {
		t.Fatalf("expected encoding/json to write the fields, got %v", value)
	}
	want := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"Level": map[string]any{"$ref": "#/components/schemas/things_Level"},
			"Other": map[string]any{"$ref": "#/components/schemas/things_Other"},
		},
		"required": []string{"Level", "Other"},
	}
	if !reflect.DeepEqual(builder.schemas["things_Both"], want) {
		t.Fatalf("expected Both as its fields, got %v", builder.schemas["things_Both"])
	}
	renamed := map[string]any{"Level": value.(map[string]any)["AmbiguousLevel"], "Other": value.(map[string]any)["AmbiguousOther"]}
	assertConforms(t, builder, builder.schemas["things_Both"], renamed)
}

type TextStructA struct {
	A int
}

func (t TextStructA) MarshalText() ([]byte, error) { return []byte("a"), nil }

type TextStructB struct {
	B int
}

func (t TextStructB) MarshalText() ([]byte, error) { return []byte("b"), nil }

type TextStructMid struct {
	TextStructA
	TextStructB
}

type textStructOuter struct {
	TextStructMid
	X int
}

type JSONStructA struct {
	A int
}

func (j JSONStructA) MarshalJSON() ([]byte, error) { return []byte(`"a"`), nil }

type JSONStructB struct {
	B int
}

func (j JSONStructB) MarshalJSON() ([]byte, error) { return []byte(`"b"`), nil }

type jsonStructBoth struct {
	JSONStructA
	JSONStructB
	X int
}

// Two embedded structs that each declare the marshaler make it ambiguous, so Go promotes
// neither and writes the fields of both, which the components of the two, a string each or
// none at all, do not describe.
func TestAmbiguousEmbeddedMarshalingStructsAreTheirFields(t *testing.T) {
	builder := buildThings(t, `package things

type TextStructA struct {
	A int
}

func (t TextStructA) MarshalText() ([]byte, error) { return []byte("a"), nil }

type TextStructB struct {
	B int
}

func (t TextStructB) MarshalText() ([]byte, error) { return []byte("b"), nil }

type TextStructMid struct {
	TextStructA
	TextStructB
}

type TextStructOuter struct {
	TextStructMid
	X int
}

type JSONStructA struct {
	A int
}

func (j JSONStructA) MarshalJSON() ([]byte, error) { return []byte("1"), nil }

type JSONStructB struct {
	B int
}

func (j JSONStructB) MarshalJSON() ([]byte, error) { return []byte("2"), nil }

type JSONStructBoth struct {
	JSONStructA
	JSONStructB
	X int
}
`, "TextStructMid", "TextStructOuter", "JSONStructBoth")

	err := builder.unresolvedError()
	if err != nil {
		t.Fatalf("expected ambiguous marshalers not to be promoted, got %v", err)
	}

	mid := marshalled(t, &TextStructMid{TextStructA: TextStructA{A: 1}, TextStructB: TextStructB{B: 2}})
	if !reflect.DeepEqual(mid, map[string]any{"A": float64(1), "B": float64(2)}) {
		t.Fatalf("expected encoding/json to write the fields of both, got %v", mid)
	}
	wantMid := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"A": map[string]any{"type": "integer", "format": "int64"},
			"B": map[string]any{"type": "integer", "format": "int64"},
		},
		"required": []string{"A", "B"},
	}
	if !reflect.DeepEqual(builder.schemas["things_TextStructMid"], wantMid) {
		t.Fatalf("expected TextStructMid as the fields of both, got %v", builder.schemas["things_TextStructMid"])
	}
	assertConforms(t, builder, builder.schemas["things_TextStructMid"], mid)

	outer := marshalled(t, &textStructOuter{X: 3})
	if !reflect.DeepEqual(outer, map[string]any{"A": float64(0), "B": float64(0), "X": float64(3)}) {
		t.Fatalf("expected encoding/json to write the fields of all three, got %v", outer)
	}
	assertConforms(t, builder, builder.schemas["things_TextStructOuter"], outer)
	if len(nonConformities(builder.schemas, builder.schemas["things_TextStructOuter"], map[string]any{"X": float64(3)}, "$")) == 0 {
		t.Fatalf("expected TextStructOuter to require the promoted fields, got %v", builder.schemas["things_TextStructOuter"])
	}

	both := marshalled(t, &jsonStructBoth{X: 3})
	if !reflect.DeepEqual(both, map[string]any{"A": float64(0), "B": float64(0), "X": float64(3)}) {
		t.Fatalf("expected encoding/json to write the fields of all three, got %v", both)
	}
	assertConforms(t, builder, builder.schemas["things_JSONStructBoth"], both)
	if len(nonConformities(builder.schemas, builder.schemas["things_JSONStructBoth"], map[string]any{"X": float64(3)}, "$")) == 0 {
		t.Fatalf("expected JSONStructBoth to require the promoted fields, got %v", builder.schemas["things_JSONStructBoth"])
	}
}

type JInner struct {
	A int `json:"a"`
}

func (j JInner) MarshalJSON() ([]byte, error) { return []byte(`"custom"`), nil }

type JMid struct {
	JInner
}

type JOther struct {
	A int `json:"a"`
}

type jOuter struct {
	*JMid
	X int `json:"x"`
}

type jOuter2 struct {
	JMid
	JOther
}

type jAnon struct {
	F struct {
		JInner
		*JOther
	} `json:"f"`
}

type TLevel int

func (l TLevel) MarshalText() ([]byte, error) { return []byte("high"), nil }

type TMid struct {
	TLevel
}

type tOuter struct {
	*TMid
	X int `json:"x"`
}

type PInner struct {
	A int `json:"a"`
}

func (p *PInner) MarshalText() ([]byte, error) { return []byte("inner"), nil }

type PMid struct {
	PInner
}

type pDeep struct {
	*PMid
	B int `json:"b"`
}

// A struct into whose method set an embedded type promotes MarshalJSON or MarshalText, at any
// depth, is written as that type alone, so the generator refuses it even where an embedded
// pointer or a collision makes it merge the embedded fields.
func TestMergedStructsWithPromotedMarshalersAreUnresolved(t *testing.T) {
	source := `package things

type JInner struct {
	A int ` + "`json:\"a\"`" + `
}

func (j JInner) MarshalJSON() ([]byte, error) { return []byte("\"custom\""), nil }

type JMid struct {
	JInner
}

type JOther struct {
	A int ` + "`json:\"a\"`" + `
}

type JOuter struct {
	*JMid
	X int ` + "`json:\"x\"`" + `
}

type JOuter2 struct {
	JMid
	JOther
}

type JAnon struct {
	F struct {
		JInner
		*JOther
	} ` + "`json:\"f\"`" + `
}

type Level int

func (l Level) MarshalText() ([]byte, error) { return []byte("high"), nil }

type TMid struct {
	Level
}

type TOuter struct {
	*TMid
	X int ` + "`json:\"x\"`" + `
}

type Inner struct {
	A int ` + "`json:\"a\"`" + `
}

func (i *Inner) MarshalText() ([]byte, error) { return []byte("inner"), nil }

type Mid struct {
	Inner
}

type Deep struct {
	*Mid
	B int ` + "`json:\"b\"`" + `
}
`
	cases := []struct {
		name   string
		value  any
		writes any
		reason string
	}{
		{"JOuter", &jOuter{JMid: &JMid{}, X: 1}, "custom", "type JOuter embeds JInner, which defines MarshalJSON"},
		{"JOuter2", &jOuter2{}, "custom", "type JOuter2 embeds JInner, which defines MarshalJSON"},
		{"JAnon", &jAnon{}, map[string]any{"f": "custom"}, "struct{JInner; *JOther}: anonymous struct embeds JInner, which defines MarshalJSON"},
		{"TOuter", &tOuter{TMid: &TMid{}}, "high", "type TOuter embeds Level, which defines MarshalText"},
		{"Deep", &pDeep{PMid: &PMid{}}, "inner", "type Deep embeds Inner, which defines MarshalText"},
	}
	for _, c := range cases {
		value := marshalled(t, c.value)
		if !reflect.DeepEqual(value, c.writes) {
			t.Fatalf("%s: expected encoding/json to write %v, got %v", c.name, c.writes, value)
		}

		builder := buildThings(t, source, c.name)
		err := builder.unresolvedError()
		if err == nil || !strings.Contains(err.Error(), c.reason) {
			t.Errorf("%s: expected %q, got %v (schema %v)", c.name, c.reason, err, builder.schemas["things_"+c.name])
		}
	}
}

type Ch1 struct{ Ch2 }
type Ch2 struct{ Ch3 }
type Ch3 struct{ Ch4 }
type Ch4 struct{ Ch5 }
type Ch5 struct{ Ch6 }
type Ch6 struct{ Ch7 }
type Ch7 struct{ Ch8 }
type Ch8 struct{ Ch9 }
type Ch9 struct {
	Deep int `json:"deep"`
}

type Shared struct {
	S int `json:"s"`
}

type ch0 struct {
	Ch1
	*Shared
}

type Node struct {
	*Node
	X int `json:"x"`
}

// Embedding is followed to any depth, as encoding/json follows it, and a recursive embedding
// ends.
func TestDeepAndRecursiveEmbeddingFollowEncodingJSON(t *testing.T) {
	source := "package things\n\n"
	for i := 1; i <= 8; i++ {
		source += "type Ch" + strconv.Itoa(i) + " struct{ Ch" + strconv.Itoa(i+1) + " }\n\n"
	}
	source += "type Ch9 struct {\n\tDeep int `json:\"deep\"`\n}\n\n" +
		"type Shared struct {\n\tS int `json:\"s\"`\n}\n\n" +
		"type Ch0 struct {\n\tCh1\n\t*Shared\n}\n\n" +
		"type Node struct {\n\t*Node\n\tX int `json:\"x\"`\n}\n"
	builder := buildThings(t, source, "Ch0", "Node")

	err := builder.unresolvedError()
	if err != nil {
		t.Fatal(err)
	}

	integer := map[string]any{"type": "integer", "format": "int64"}
	wantCh0 := map[string]any{
		"type":       "object",
		"properties": map[string]any{"deep": integer, "s": integer},
		"required":   []string{"deep"},
	}
	if !reflect.DeepEqual(builder.schemas["things_Ch0"], wantCh0) {
		t.Errorf("expected the field nine embeddings down, got %v", builder.schemas["things_Ch0"])
	}
	value := marshalled(t, &ch0{})
	if !reflect.DeepEqual(value, map[string]any{"deep": float64(0)}) {
		t.Fatalf("expected encoding/json to write deep, got %v", value)
	}
	assertConforms(t, builder, builder.schemas["things_Ch0"], value)

	wantNode := map[string]any{
		"type":       "object",
		"properties": map[string]any{"x": integer},
		"required":   []string{"x"},
	}
	if !reflect.DeepEqual(builder.schemas["things_Node"], wantNode) {
		t.Errorf("expected Node's own x, got %v", builder.schemas["things_Node"])
	}
	node := marshalled(t, &Node{Node: &Node{X: 2}, X: 1})
	if !reflect.DeepEqual(node, map[string]any{"x": float64(1)}) {
		t.Fatalf("expected encoding/json to write the outer x, got %v", node)
	}
	assertConforms(t, builder, builder.schemas["things_Node"], node)
}

type LatticeLeaf struct {
	Y int `json:"y"`
}

type LatticeA struct{ LatticeLeaf }

type LatticeB struct{ LatticeLeaf }

type lattice struct {
	LatticeA
	*LatticeB
	X int `json:"x"`
}

// A struct embedded more than once at one depth has its fields cancel out, as encoding/json
// counts it; each struct type is expanded once in the whole walk, so a lattice of embeddings
// stays fast.
func TestEmbeddingLatticeFollowsEncodingJSON(t *testing.T) {
	builder := buildThings(t, `package things

type Leaf struct {
	Y int `+"`json:\"y\"`"+`
}

type A struct{ Leaf }

type B struct{ Leaf }

type Lattice struct {
	A
	*B
	X int `+"`json:\"x\"`"+`
}
`, "Lattice")

	err := builder.unresolvedError()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"type":       "object",
		"properties": map[string]any{"x": map[string]any{"type": "integer", "format": "int64"}},
		"required":   []string{"x"},
	}
	if !reflect.DeepEqual(builder.schemas["things_Lattice"], want) {
		t.Errorf("expected y to cancel out, got %v", builder.schemas["things_Lattice"])
	}
	value := marshalled(t, &lattice{LatticeB: &LatticeB{}, X: 1})
	if !reflect.DeepEqual(value, map[string]any{"x": float64(1)}) {
		t.Fatalf("expected encoding/json to drop y, got %v", value)
	}
	assertConforms(t, builder, builder.schemas["things_Lattice"], value)

	// Four types per level, each embedding the four of the next, eight levels deep.
	source := "package things\n\n"
	for level := 1; level <= 8; level++ {
		for i := range 4 {
			source += "type L" + strconv.Itoa(level) + "x" + strconv.Itoa(i) + " struct {\n"
			if level == 8 {
				source += "\tV" + strconv.Itoa(i) + " int `json:\"v" + strconv.Itoa(i) + "\"`\n"
			} else {
				for j := range 4 {
					source += "\tL" + strconv.Itoa(level+1) + "x" + strconv.Itoa(j) + "\n"
				}
			}
			source += "}\n\n"
		}
	}
	source += "type Root struct {\n\t*L1x0\n\tL1x1\n\tL1x2\n\tL1x3\n\tX int `json:\"x\"`\n}\n"
	started := time.Now()
	wide := buildThings(t, source, "Root")
	elapsed := time.Since(started)
	if elapsed > 2*time.Second {
		t.Fatalf("expected the lattice walked once per type, took %v", elapsed)
	}
	// Every type below the first level is embedded four times at its depth, so its fields
	// cancel out.
	if !reflect.DeepEqual(wide.schemas["things_Root"], want) {
		t.Errorf("expected only x, got %v", wide.schemas["things_Root"])
	}
}

type sqlEmbeddedPointer struct {
	*sql.NullInt64
	X int `json:"x"`
}

// An embedded pointer to one of database/sql's Null types leaves its fields out when nil, so
// they are not required.
func TestEmbeddedSQLNullPointerIsOptional(t *testing.T) {
	builder := buildThings(t, `package things

import "database/sql"

type SqlEmbPtr struct {
	*sql.NullInt64
	X int `+"`json:\"x\"`"+`
}
`, "SqlEmbPtr")

	err := builder.unresolvedError()
	if err != nil {
		t.Fatal(err)
	}
	integer := map[string]any{"type": "integer", "format": "int64"}
	want := map[string]any{
		"type":       "object",
		"properties": map[string]any{"Int64": integer, "Valid": map[string]any{"type": "boolean"}, "x": integer},
		"required":   []string{"x"},
	}
	if !reflect.DeepEqual(builder.schemas["things_SqlEmbPtr"], want) {
		t.Errorf("expected the Null fields optional, got %v", builder.schemas["things_SqlEmbPtr"])
	}
	empty := marshalled(t, &sqlEmbeddedPointer{})
	if !reflect.DeepEqual(empty, map[string]any{"x": float64(0)}) {
		t.Fatalf("expected encoding/json to leave out a nil pointer's fields, got %v", empty)
	}
	assertConforms(t, builder, builder.schemas["things_SqlEmbPtr"], empty)
	assertConforms(t, builder, builder.schemas["things_SqlEmbPtr"], marshalled(t, &sqlEmbeddedPointer{NullInt64: &sql.NullInt64{Int64: 1, Valid: true}}))
}

// Two names of one declaration with one json name tie, and encoding/json drops the name.
func TestOneDeclarationWithTwoNamesForOneKey(t *testing.T) {
	builder := buildThings(t, `package things

type Dup struct {
	A, B int `+"`json:\"ab\"`"+`
	C    int `+"`json:\"c\"`"+`
}
`, "Dup")

	integer := map[string]any{"type": "integer", "format": "int64"}
	want := map[string]any{
		"type":       "object",
		"properties": map[string]any{"c": integer},
		"required":   []string{"c"},
	}
	if !reflect.DeepEqual(builder.schemas["things_Dup"], want) {
		t.Errorf("expected ab dropped, got %v", builder.schemas["things_Dup"])
	}

	// Built through reflect: go vet refuses a declared struct that repeats a json tag.
	dup := reflect.StructOf([]reflect.StructField{
		{Name: "A", Type: reflect.TypeOf(0), Tag: `json:"ab"`},
		{Name: "B", Type: reflect.TypeOf(0), Tag: `json:"ab"`},
		{Name: "C", Type: reflect.TypeOf(0), Tag: `json:"c"`},
	})
	value := marshalled(t, reflect.New(dup).Interface())
	if !reflect.DeepEqual(value, map[string]any{"c": float64(0)}) {
		t.Fatalf("expected encoding/json to drop ab, got %v", value)
	}
	assertConforms(t, builder, builder.schemas["things_Dup"], value)
}

type nilCollectionsThing struct {
	Strings sql.Null[[]string]       `json:"strings"`
	Counts  sql.Null[map[string]int] `json:"counts"`
	Bytes   sql.Null[[]byte]         `json:"bytes"`
	Query   url.Values               `json:"query"`
	Header  http.Header              `json:"header"`
	Tags    declTags                 `json:"tags"`
	CSV     declCSVAlias             `json:"csv"`
}

type declCSVAlias = declCSV

// The field rule for nil slices and maps also holds for sql.Null[T]'s V, for the standard
// library's named maps, and for a type declared from a marshaling slice type, which does not
// inherit its marshaler; an alias of that type keeps it.
func TestNilCollectionsBehindNamesAreNullable(t *testing.T) {
	builder := buildThings(t, `package things

import (
	"database/sql"
	"net/http"
	"net/url"
	"strings"
)

type CSV []string

func (c CSV) MarshalText() ([]byte, error) { return []byte(strings.Join(c, ",")), nil }

type Tags CSV

type CSVAlias = CSV

type Thing struct {
	Strings sql.Null[[]string]       `+"`json:\"strings\"`"+`
	Counts  sql.Null[map[string]int] `+"`json:\"counts\"`"+`
	Bytes   sql.Null[[]byte]         `+"`json:\"bytes\"`"+`
	Query   url.Values               `+"`json:\"query\"`"+`
	Header  http.Header              `+"`json:\"header\"`"+`
	Tags    Tags                     `+"`json:\"tags\"`"+`
	CSV     CSVAlias                 `+"`json:\"csv\"`"+`
}
`, "Thing")

	err := builder.unresolvedError()
	if err != nil {
		t.Fatal(err)
	}
	properties := componentProperties(t, builder, "things_Thing")
	values := map[string]map[string]any{
		"strings": {"type": "array", "items": map[string]any{"type": "string"}, "nullable": true},
		"counts":  {"type": "object", "additionalProperties": map[string]any{"type": "integer", "format": "int64"}, "nullable": true},
		"bytes":   {"type": "string", "format": "byte", "nullable": true},
	}
	for name, value := range values {
		got := properties[name].(map[string]any)["properties"].(map[string]any)["V"]
		if !reflect.DeepEqual(got, value) {
			t.Errorf("%s: expected V %v, got %v", name, value, got)
		}
	}
	nullable := map[string]string{"query": "net_url_Values", "header": "net_http_Header", "tags": "things_Tags"}
	for name, component := range nullable {
		want := map[string]any{"allOf": []any{map[string]any{"$ref": "#/components/schemas/" + component}}, "nullable": true}
		if !reflect.DeepEqual(properties[name], want) {
			t.Errorf("%s: expected %v, got %v", name, want, properties[name])
		}
	}
	if !reflect.DeepEqual(properties["csv"], map[string]any{"$ref": "#/components/schemas/things_CSVAlias"}) {
		t.Errorf("expected the alias of a MarshalText type not nullable, got %v", properties["csv"])
	}

	empty := marshalled(t, &nilCollectionsThing{})
	wantEmpty := map[string]any{
		"strings": map[string]any{"V": nil, "Valid": false},
		"counts":  map[string]any{"V": nil, "Valid": false},
		"bytes":   map[string]any{"V": nil, "Valid": false},
		"query":   nil,
		"header":  nil,
		"tags":    nil,
		"csv":     "",
	}
	if !reflect.DeepEqual(empty, wantEmpty) {
		t.Fatalf("expected encoding/json to write %v, got %v", wantEmpty, empty)
	}
	assertConforms(t, builder, builder.schemas["things_Thing"], empty)
}

type AliasedLevel = textLevel

type AliasedLevelAlias = AliasedLevel

type aliasEmbed struct {
	AliasedLevel
	X int `json:"x"`
}

type AliasMid struct {
	AliasedLevel
	Y int `json:"y"`
}

type aliasOuter struct {
	AliasMid
	X int `json:"x"`
}

type AliasTS struct {
	A int
}

func (a AliasTS) MarshalText() ([]byte, error) { return []byte("ts"), nil }

type AliasTSAlias = AliasTS

type aliasPtrTS struct {
	*AliasTSAlias
	A string `json:"a"`
}

type AliasJInt int

func (j AliasJInt) MarshalJSON() ([]byte, error) { return []byte("1"), nil }

type AliasJA = AliasJInt

type aliasJEmbed struct {
	AliasJA
	X int `json:"x"`
}

type aliasChainEmbed struct {
	AliasedLevelAlias
	X int `json:"x"`
}

// An embedded alias has the methods of the type it names, so a marshaler it promotes is refused
// as the named type's would be: one level down, merged, through an alias of an alias, and for an
// alias declared in, or naming a type of, another package.
func TestEmbeddedAliasesOfMarshalingTypesAreUnresolved(t *testing.T) {
	repoRoot := t.TempDir()
	sources := map[string]string{
		"other": `package other

type Level int

func (l Level) MarshalText() ([]byte, error) { return []byte("high"), nil }

type LevelAlias = Level
`,
		"things": `package things

import "example.com/app/other"

type Level int

func (l Level) MarshalText() ([]byte, error) { return []byte("high"), nil }

type LevelAlias = Level

type LevelAliasAlias = LevelAlias

type Embed struct {
	LevelAlias
	X int ` + "`json:\"x\"`" + `
}

type Mid struct {
	LevelAlias
	Y int ` + "`json:\"y\"`" + `
}

type Outer struct {
	Mid
	X int ` + "`json:\"x\"`" + `
}

type TS struct {
	A int
}

func (t TS) MarshalText() ([]byte, error) { return []byte("ts"), nil }

type TSAlias = TS

type PtrTS struct {
	*TSAlias
	A string ` + "`json:\"a\"`" + `
}

type JInt int

func (j JInt) MarshalJSON() ([]byte, error) { return []byte("1"), nil }

type JA = JInt

type JEmbed struct {
	JA
	X int ` + "`json:\"x\"`" + `
}

type Chain struct {
	LevelAliasAlias
	X int ` + "`json:\"x\"`" + `
}

type Remote struct {
	other.LevelAlias
	X int ` + "`json:\"x\"`" + `
}

type RemoteLevel = other.Level

type ToRemote struct {
	RemoteLevel
	X int ` + "`json:\"x\"`" + `
}
`,
	}
	for dir, source := range sources {
		err := os.MkdirAll(filepath.Join(repoRoot, dir), 0o755)
		if err != nil {
			t.Fatal(err)
		}
		err = os.WriteFile(filepath.Join(repoRoot, dir, dir+".go"), []byte(source), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name   string
		value  any
		writes any
		reason string
	}{
		{"Embed", &aliasEmbed{AliasedLevel: 2}, "high", "type Embed embeds LevelAlias, which defines MarshalText"},
		{"Outer", &aliasOuter{AliasMid: AliasMid{AliasedLevel: 2}}, "high", "type Outer embeds LevelAlias, which defines MarshalText"},
		{"PtrTS", &aliasPtrTS{AliasTSAlias: &AliasTSAlias{}}, "ts", "type PtrTS embeds TSAlias, which defines MarshalText"},
		{"JEmbed", &aliasJEmbed{}, float64(1), "type JEmbed embeds JA, which defines MarshalJSON"},
		{"Chain", &aliasChainEmbed{AliasedLevelAlias: 2}, "high", "type Chain embeds LevelAliasAlias, which defines MarshalText"},
		// Go has one package here: the alias semantics are those of Embed.
		{"Remote", &aliasEmbed{AliasedLevel: 2}, "high", "type Remote embeds LevelAlias, which defines MarshalText"},
		{"ToRemote", &aliasEmbed{AliasedLevel: 2}, "high", "type ToRemote embeds RemoteLevel, which defines MarshalText"},
	}
	for _, c := range cases {
		value := marshalled(t, c.value)
		if !reflect.DeepEqual(value, c.writes) {
			t.Fatalf("%s: expected encoding/json to write %v, got %v", c.name, c.writes, value)
		}

		builder := newTestSchemaBuilder(t, repoRoot)
		builder.schemaRef(typeRef{ImportPath: "example.com/app/things", Name: c.name})
		err := builder.unresolvedError()
		if err == nil || !strings.Contains(err.Error(), c.reason) {
			t.Errorf("%s: expected %q, got %v (schema %v)", c.name, c.reason, err, builder.schemas["things_"+c.name])
		}
	}
}

type sqlPtrGeneric struct {
	*sql.Null[int]
	X int `json:"x"`
}

type sqlShadow struct {
	sql.Null[int]
	V string
}

type SQLNullAlias = sql.NullInt64

type SQLNullDeclared sql.NullInt64

type SQLGenericAlias = sql.Null[int]

type SQLGenericDeclared sql.Null[string]

type sqlPtrAlias struct {
	*SQLNullAlias
	X int `json:"x"`
}

type sqlPtrDeclared struct {
	*SQLNullDeclared
	X int `json:"x"`
}

type sqlPtrGenericAlias struct {
	*SQLGenericAlias
	X int `json:"x"`
}

type sqlPtrGenericDeclared struct {
	*SQLGenericDeclared
	X int `json:"x"`
}

type sqlPtrSlice struct {
	*sql.Null[[]string]
	X int `json:"x"`
}

type sqlPlain struct {
	sql.Null[int]
	X int `json:"x"`
}

// An embedded sql.Null[T], and an alias of or a type declared from one of database/sql's Null
// types, is read as the struct it is: its fields are optional behind a pointer, and a name of
// the embedding struct wins over them.
func TestEmbeddedSQLNullsAreTheirStructs(t *testing.T) {
	builder := buildThings(t, `package things

import "database/sql"

type PtrGeneric struct {
	*sql.Null[int]
	X int `+"`json:\"x\"`"+`
}

type Shadow struct {
	sql.Null[int]
	V string
}

type NullAlias = sql.NullInt64

type NullDeclared sql.NullInt64

type GenericAlias = sql.Null[int]

type GenericDeclared sql.Null[string]

type PtrAlias struct {
	*NullAlias
	X int `+"`json:\"x\"`"+`
}

type PtrDeclared struct {
	*NullDeclared
	X int `+"`json:\"x\"`"+`
}

type PtrGenericAlias struct {
	*GenericAlias
	X int `+"`json:\"x\"`"+`
}

type PtrGenericDeclared struct {
	*GenericDeclared
	X int `+"`json:\"x\"`"+`
}

type PtrSlice struct {
	*sql.Null[[]string]
	X int `+"`json:\"x\"`"+`
}

type Plain struct {
	sql.Null[int]
	X int `+"`json:\"x\"`"+`
}
`, "PtrGeneric", "Shadow", "PtrAlias", "PtrDeclared", "PtrGenericAlias", "PtrGenericDeclared", "PtrSlice", "Plain")

	err := builder.unresolvedError()
	if err != nil {
		t.Fatal(err)
	}

	integer := map[string]any{"type": "integer", "format": "int64"}
	boolean := map[string]any{"type": "boolean"}
	wantPtrGeneric := map[string]any{
		"type":       "object",
		"properties": map[string]any{"V": integer, "Valid": boolean, "x": integer},
		"required":   []string{"x"},
	}
	if !reflect.DeepEqual(builder.schemas["things_PtrGeneric"], wantPtrGeneric) {
		t.Errorf("expected the fields of an embedded *sql.Null[int] optional, got %v", builder.schemas["things_PtrGeneric"])
	}
	wantShadow := map[string]any{
		"type":       "object",
		"properties": map[string]any{"V": map[string]any{"type": "string"}, "Valid": boolean},
		"required":   []string{"Valid", "V"},
	}
	if !reflect.DeepEqual(builder.schemas["things_Shadow"], wantShadow) {
		t.Errorf("expected the outer V to win, got %v", builder.schemas["things_Shadow"])
	}

	cases := []struct {
		name   string
		empty  any
		writes any
		set    any
	}{
		{"PtrGeneric", &sqlPtrGeneric{}, map[string]any{"x": float64(0)}, &sqlPtrGeneric{Null: &sql.Null[int]{V: 1, Valid: true}}},
		{"Shadow", &sqlShadow{}, map[string]any{"V": "", "Valid": false}, &sqlShadow{Null: sql.Null[int]{V: 1, Valid: true}, V: "v"}},
		{"PtrAlias", &sqlPtrAlias{}, map[string]any{"x": float64(0)}, &sqlPtrAlias{SQLNullAlias: &SQLNullAlias{Int64: 1, Valid: true}}},
		{"PtrDeclared", &sqlPtrDeclared{}, map[string]any{"x": float64(0)}, &sqlPtrDeclared{SQLNullDeclared: &SQLNullDeclared{Int64: 1, Valid: true}}},
		{"PtrGenericAlias", &sqlPtrGenericAlias{}, map[string]any{"x": float64(0)}, &sqlPtrGenericAlias{SQLGenericAlias: &SQLGenericAlias{V: 1, Valid: true}}},
		{"PtrGenericDeclared", &sqlPtrGenericDeclared{}, map[string]any{"x": float64(0)}, &sqlPtrGenericDeclared{SQLGenericDeclared: &SQLGenericDeclared{V: "v", Valid: true}}},
		{"PtrSlice", &sqlPtrSlice{}, map[string]any{"x": float64(0)}, &sqlPtrSlice{Null: &sql.Null[[]string]{Valid: true}}},
		{"Plain", &sqlPlain{}, map[string]any{"V": float64(0), "Valid": false, "x": float64(0)}, &sqlPlain{Null: sql.Null[int]{V: 1, Valid: true}, X: 1}},
	}
	for _, c := range cases {
		empty := marshalled(t, c.empty)
		if !reflect.DeepEqual(empty, c.writes) {
			t.Fatalf("%s: expected encoding/json to write %v, got %v", c.name, c.writes, empty)
		}
		schema := builder.schemas["things_"+c.name]
		for _, problem := range nonConformities(builder.schemas, schema, empty, c.name) {
			t.Error(problem)
		}
		for _, problem := range nonConformities(builder.schemas, schema, marshalled(t, c.set), c.name) {
			t.Error(problem)
		}
	}
	if len(nonConformities(builder.schemas, builder.schemas["things_Plain"], map[string]any{"x": float64(0)}, "$")) == 0 {
		t.Errorf("expected Plain to require the fields of its embedded sql.Null[int], got %v", builder.schemas["things_Plain"])
	}
}

type TieLevel int

func (l TieLevel) MarshalText() ([]byte, error) { return []byte("high"), nil }

type TieLevel2 int

func (l TieLevel2) MarshalText() ([]byte, error) { return []byte("low"), nil }

type TieMid1 struct {
	TieLevel
	Y int `json:"y"`
}

type TieMidL2 struct {
	TieLevel2
}

type tieT2 struct {
	TieMid1
	TieMidL2
	X int `json:"x"`
}

// Two MarshalText methods two embeddings down, through named structs that each promote one,
// tie: Go promotes neither and writes the fields, while each named struct standing alone is
// still written as its text.
func TestMarshalTextTieThroughNamedStructs(t *testing.T) {
	source := `package things

type TieLevel int

func (l TieLevel) MarshalText() ([]byte, error) { return []byte("high"), nil }

type TieLevel2 int

func (l TieLevel2) MarshalText() ([]byte, error) { return []byte("low"), nil }

type Mid1 struct {
	TieLevel
	Y int ` + "`json:\"y\"`" + `
}

type MidL2 struct {
	TieLevel2
}

type T2 struct {
	Mid1
	MidL2
	X int ` + "`json:\"x\"`" + `
}
`
	builder := buildThings(t, source, "T2")
	err := builder.unresolvedError()
	if err != nil {
		t.Fatalf("expected the tie to leave T2 its fields, got %v", err)
	}

	value := marshalled(t, &tieT2{})
	want := map[string]any{"TieLevel": "high", "TieLevel2": "low", "x": float64(0), "y": float64(0)}
	if !reflect.DeepEqual(value, want) {
		t.Fatalf("expected encoding/json to write %v, got %v", want, value)
	}
	assertConforms(t, builder, builder.schemas["things_T2"], value)
	if len(nonConformities(builder.schemas, builder.schemas["things_T2"], map[string]any{"x": float64(0)}, "$")) == 0 {
		t.Errorf("expected T2 to require the promoted fields, got %v", builder.schemas["things_T2"])
	}

	standing := []struct {
		name   string
		value  any
		writes any
		reason string
	}{
		{"Mid1", &TieMid1{}, "high", "type Mid1 embeds TieLevel, which defines MarshalText"},
		{"MidL2", &TieMidL2{}, "low", "type MidL2 embeds TieLevel2, which defines MarshalText"},
	}
	for _, c := range standing {
		value := marshalled(t, c.value)
		if !reflect.DeepEqual(value, c.writes) {
			t.Fatalf("%s: expected encoding/json to write %v, got %v", c.name, c.writes, value)
		}
		alone := buildThings(t, source, c.name)
		err := alone.unresolvedError()
		if err == nil || !strings.Contains(err.Error(), c.reason) {
			t.Errorf("%s: expected %q, got %v", c.name, c.reason, err)
		}
	}
}

type anonField struct {
	Inner struct {
		textLevel
		X int `json:"x"`
	} `json:"inner"`
}

type AnonTS struct {
	A int
}

func (a AnonTS) MarshalText() ([]byte, error) { return []byte("ts"), nil }

type anonMerged struct {
	F struct {
		AnonTS
		A string `json:"a"`
	} `json:"f"`
}

type AnonIn struct {
	B int `json:"b"`
}

type anonPointer struct {
	F struct {
		*AnonIn
		textLevel
	} `json:"f"`
}

// An anonymous struct into which an embedded type promotes a marshaler is written as that type
// alone; it has no component, so it is refused where it is written, and a named one is refused
// once, through its component.
func TestAnonymousStructsWithPromotedMarshalersAreUnresolved(t *testing.T) {
	source := `package things

type Level int

func (l Level) MarshalText() ([]byte, error) { return []byte("high"), nil }

type TS struct {
	A int
}

func (t TS) MarshalText() ([]byte, error) { return []byte("ts"), nil }

type In struct {
	B int ` + "`json:\"b\"`" + `
}

type Field struct {
	Inner struct {
		Level
		X int ` + "`json:\"x\"`" + `
	} ` + "`json:\"inner\"`" + `
}

type Merged struct {
	F struct {
		TS
		A string ` + "`json:\"a\"`" + `
	} ` + "`json:\"f\"`" + `
}

type Pointer struct {
	F struct {
		*In
		Level
	} ` + "`json:\"f\"`" + `
}

type Named struct {
	Level
	X int ` + "`json:\"x\"`" + `
}
`
	cases := []struct {
		name   string
		value  any
		writes any
		reason string
	}{
		{"Field", &anonField{}, map[string]any{"inner": "low"}, "things.struct{Level; X int}: anonymous struct embeds Level, which defines MarshalText"},
		{"Merged", &anonMerged{}, map[string]any{"f": "ts"}, "things.struct{TS; A string}: anonymous struct embeds TS, which defines MarshalText"},
		{"Pointer", &anonPointer{}, map[string]any{"f": "low"}, "things.struct{*In; Level}: anonymous struct embeds Level, which defines MarshalText"},
	}
	for _, c := range cases {
		value := marshalled(t, c.value)
		if !reflect.DeepEqual(value, c.writes) {
			t.Fatalf("%s: expected encoding/json to write %v, got %v", c.name, c.writes, value)
		}

		builder := buildThings(t, source, c.name)
		err := builder.unresolvedError()
		if err == nil || !strings.Contains(err.Error(), c.reason) {
			t.Errorf("%s: expected %q, got %v (schema %v)", c.name, c.reason, err, builder.schemas["things_"+c.name])
		}
	}

	named := buildThings(t, source, "Named")
	if len(named.unresolved) != 1 || !strings.Contains(named.unresolved[0], "type Named embeds Level, which defines MarshalText") {
		t.Errorf("expected Named refused once, got %v", named.unresolved)
	}
}

type AliasRecv int

type AliasRecvA = AliasRecv

func (AliasRecvA) MarshalText() ([]byte, error) { return []byte("recv"), nil }

type aliasRecvThing struct {
	R AliasRecv  `json:"r"`
	A AliasRecvA `json:"a"`
}

// A method declared on an alias receiver is the aliased type's.
func TestMarshalTextOnAliasReceiver(t *testing.T) {
	builder := buildThings(t, `package things

type Recv int

type RecvA = Recv

func (RecvA) MarshalText() ([]byte, error) { return []byte("recv"), nil }

type Thing struct {
	R Recv  `+"`json:\"r\"`"+`
	A RecvA `+"`json:\"a\"`"+`
}
`, "Thing")

	err := builder.unresolvedError()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(builder.schemas["things_Recv"], map[string]any{"type": "string"}) {
		t.Errorf("expected Recv a string, got %v", builder.schemas["things_Recv"])
	}
	value := marshalled(t, &aliasRecvThing{})
	if !reflect.DeepEqual(value, map[string]any{"r": "recv", "a": "recv"}) {
		t.Fatalf("expected encoding/json to write the text, got %v", value)
	}
	assertConforms(t, builder, builder.schemas["things_Thing"], value)

	embeds := buildThings(t, `package things

type Recv int

type RecvA = Recv

func (RecvA) MarshalText() ([]byte, error) { return []byte("recv"), nil }

type Embeds struct {
	Recv
	X int `+"`json:\"x\"`"+`
}
`, "Embeds")
	err = embeds.unresolvedError()
	if err == nil || !strings.Contains(err.Error(), "type Embeds embeds Recv, which defines MarshalText") {
		t.Errorf("expected the promoted MarshalText reported, got %v", err)
	}
}

// buildDescribed is buildThings with Config.TypeSchemas set before the types are built.
func buildDescribed(t *testing.T, source string, described map[string]map[string]any, names ...string) *schemaBuilder {
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
	builder.typeSchemas = described
	for _, name := range names {
		builder.schemaRef(typeRef{ImportPath: "example.com/app/things", Name: name})
	}

	return builder
}

var textMarshalerType = reflect.TypeFor[encoding.TextMarshaler]()

var jsonMarshalerType = reflect.TypeFor[json.Marshaler]()

// promotesMarshaler reports what Go's method sets say: a value of value's type, or a pointer to
// one, has MarshalJSON or MarshalText.
func promotesMarshaler(value any) bool {
	typ := reflect.TypeOf(value)
	if typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	pointer := reflect.PointerTo(typ)

	return typ.Implements(textMarshalerType) || typ.Implements(jsonMarshalerType) || pointer.Implements(textMarshalerType) || pointer.Implements(jsonMarshalerType)
}

type PathText struct {
	A int
}

func (PathText) MarshalText() ([]byte, error) { return []byte("t"), nil }

type PathMid struct {
	PathText
}

type PathMidM struct {
	PathText
	M int
}

type PathP struct {
	PathMid
	PX int `json:"p"`
}

type PathQ struct {
	PathMid
	QX int `json:"q"`
}

type PathPM struct {
	PathMidM
	PX int `json:"p"`
}

type PathQM struct {
	PathMidM
	QX int `json:"q"`
}

type pathRoot struct {
	PathP
	PathQ
}

type pathRootM struct {
	PathPM
	PathQM
}

type pathRootPtr struct {
	*PathP
	*PathQ
}

type pathAnon struct {
	F struct {
		PathP
		PathQ
	} `json:"f"`
}

type PathJSON struct {
	A int
}

func (PathJSON) MarshalJSON() ([]byte, error) { return []byte(`"j"`), nil }

type PathJMid struct {
	PathJSON
}

type PathJP struct {
	PathJMid
	PX int `json:"p"`
}

type PathJQ struct {
	PathJMid
	QX int `json:"q"`
}

type pathJRoot struct {
	PathJP
	PathJQ
}

type PathTMid struct {
	time.Time
}

type PathTP struct {
	PathTMid
	PX int `json:"p"`
}

type PathTQ struct {
	PathTMid
	QX int `json:"q"`
}

type pathTRoot struct {
	PathTP
	PathTQ
}

// Two paths to one type that declares a marshaler, at one depth, make the method ambiguous, as
// Go's selector rule says: neither path promotes it, and the struct is written as its fields.
func TestTwoPathsToOneMarshalerTie(t *testing.T) {
	source := `package things

import "time"

type PathText struct {
	A int
}

func (PathText) MarshalText() ([]byte, error) { return []byte("t"), nil }

type PathMid struct {
	PathText
}

type PathMidM struct {
	PathText
	M int
}

type PathP struct {
	PathMid
	PX int ` + "`json:\"p\"`" + `
}

type PathQ struct {
	PathMid
	QX int ` + "`json:\"q\"`" + `
}

type PathPM struct {
	PathMidM
	PX int ` + "`json:\"p\"`" + `
}

type PathQM struct {
	PathMidM
	QX int ` + "`json:\"q\"`" + `
}

type Root struct {
	PathP
	PathQ
}

type RootM struct {
	PathPM
	PathQM
}

type RootPtr struct {
	*PathP
	*PathQ
}

type Anon struct {
	F struct {
		PathP
		PathQ
	} ` + "`json:\"f\"`" + `
}

type PathJSON struct {
	A int
}

func (PathJSON) MarshalJSON() ([]byte, error) { return []byte("\"j\""), nil }

type PathJMid struct {
	PathJSON
}

type PathJP struct {
	PathJMid
	PX int ` + "`json:\"p\"`" + `
}

type PathJQ struct {
	PathJMid
	QX int ` + "`json:\"q\"`" + `
}

type JRoot struct {
	PathJP
	PathJQ
}

type PathTMid struct {
	time.Time
}

type PathTP struct {
	PathTMid
	PX int ` + "`json:\"p\"`" + `
}

type PathTQ struct {
	PathTMid
	QX int ` + "`json:\"q\"`" + `
}

type TRoot struct {
	PathTP
	PathTQ
}
`
	cases := []struct {
		name   string
		value  any
		writes any
	}{
		{"Root", &pathRoot{PathP: PathP{PX: 7}, PathQ: PathQ{QX: 7}}, map[string]any{"A": float64(0), "p": float64(7), "q": float64(7)}},
		{"RootM", &pathRootM{}, map[string]any{"A": float64(0), "p": float64(0), "q": float64(0)}},
		{"RootPtr", &pathRootPtr{PathP: &PathP{}, PathQ: &PathQ{}}, map[string]any{"A": float64(0), "p": float64(0), "q": float64(0)}},
		{"Anon", &pathAnon{}, map[string]any{"f": map[string]any{"A": float64(0), "p": float64(0), "q": float64(0)}}},
		{"JRoot", &pathJRoot{}, map[string]any{"A": float64(0), "p": float64(0), "q": float64(0)}},
		{"TRoot", &pathTRoot{}, map[string]any{"p": float64(0), "q": float64(0)}},
	}
	for _, c := range cases {
		if c.name != "Anon" && promotesMarshaler(c.value) {
			t.Fatalf("%s: expected Go to promote no marshaler", c.name)
		}
		value := marshalled(t, c.value)
		if !reflect.DeepEqual(value, c.writes) {
			t.Fatalf("%s: expected encoding/json to write %v, got %v", c.name, c.writes, value)
		}

		builder := buildThings(t, source, c.name)
		err := builder.unresolvedError()
		if err != nil {
			t.Errorf("%s: expected its fields, got %v", c.name, err)
			continue
		}
		assertConforms(t, builder, builder.schemas["things_"+c.name], value)
	}
	anonymous := pathAnon{}
	if promotesMarshaler(&anonymous.F) {
		t.Fatal("Anon.F: expected Go to promote no marshaler")
	}

	// One path promotes the method: PathP standing alone is written as its text.
	if !promotesMarshaler(&PathP{}) {
		t.Fatal("PathP: expected Go to promote MarshalText")
	}
	alone := buildThings(t, source, "PathP")
	err := alone.unresolvedError()
	if err == nil || !strings.Contains(err.Error(), "type PathP embeds PathText, which defines MarshalText") {
		t.Errorf("expected PathP refused, got %v", err)
	}
}

type SkipTextStruct struct {
	A int
	B string
}

func (SkipTextStruct) MarshalText() ([]byte, error) { return []byte("s"), nil }

type SkipDescribed struct {
	C int
}

func (SkipDescribed) MarshalText() ([]byte, error) { return []byte("d"), nil }

type SkipTextIface interface {
	MarshalText() ([]byte, error)
}

type skipNetipTie struct {
	netip.Addr
	SkipTextStruct
	X int `json:"x"`
}

type skipDescribedTie struct {
	SkipDescribed
	SkipTextStruct
	X int `json:"x"`
}

type skipIfaceTie struct {
	SkipTextIface
	SkipTextStruct
	X int `json:"x"`
}

// An embedded type the walk does not expand still counts as a declaring type at its depth: a
// standard-library type knownMarshalers lists, a described one, and an interface.
func TestSkippedEmbeddedTypesTie(t *testing.T) {
	source := `package things

import "net/netip"

type SkipTextStruct struct {
	A int
	B string
}

func (SkipTextStruct) MarshalText() ([]byte, error) { return []byte("s"), nil }

type SkipDescribed struct {
	C int
}

func (SkipDescribed) MarshalText() ([]byte, error) { return []byte("d"), nil }

type SkipTextIface interface {
	MarshalText() ([]byte, error)
}

type NetipTie struct {
	netip.Addr
	SkipTextStruct
	X int ` + "`json:\"x\"`" + `
}

type DescribedTie struct {
	SkipDescribed
	SkipTextStruct
	X int ` + "`json:\"x\"`" + `
}

type IfaceTie struct {
	SkipTextIface
	SkipTextStruct
	X int ` + "`json:\"x\"`" + `
}
`
	described := map[string]map[string]any{
		"example.com/app/things.SkipDescribed": {"type": "string"},
	}
	cases := []struct {
		name   string
		value  any
		writes any
	}{
		{"NetipTie", &skipNetipTie{SkipTextStruct: SkipTextStruct{A: 7, B: "s"}, X: 7}, map[string]any{"A": float64(7), "B": "s", "x": float64(7)}},
		{"DescribedTie", &skipDescribedTie{SkipDescribed: SkipDescribed{C: 1}, X: 7}, map[string]any{"A": float64(0), "B": "", "C": float64(1), "x": float64(7)}},
		{"IfaceTie", &skipIfaceTie{SkipTextIface: SkipTextStruct{}, X: 7}, map[string]any{"A": float64(0), "B": "", "SkipTextIface": "s", "x": float64(7)}},
	}
	for _, c := range cases {
		if promotesMarshaler(c.value) {
			t.Fatalf("%s: expected Go to promote no marshaler", c.name)
		}
		value := marshalled(t, c.value)
		if !reflect.DeepEqual(value, c.writes) {
			t.Fatalf("%s: expected encoding/json to write %v, got %v", c.name, c.writes, value)
		}

		builder := buildDescribed(t, source, described, c.name)
		err := builder.unresolvedError()
		if err != nil {
			t.Errorf("%s: expected its fields, got %v", c.name, err)
			continue
		}
		assertConforms(t, builder, builder.schemas["things_"+c.name], value)
	}
}

type SeenDescribedObj struct {
	A int
}

func (SeenDescribedObj) MarshalJSON() ([]byte, error) { return []byte(`{"custom":1}`), nil }

type SeenTextIface interface {
	MarshalText() ([]byte, error)
}

type SeenJSONIface interface {
	json.Marshaler
}

type seenDescribed struct {
	SeenDescribedObj
	X int `json:"x"`
}

type seenNetip struct {
	netip.Addr
	X int `json:"x"`
}

type seenBigFloat struct {
	big.Float
	X int `json:"x"`
}

type seenJSONMarshaler struct {
	json.Marshaler
	X int `json:"x"`
}

type seenTextMarshaler struct {
	encoding.TextMarshaler
	X int `json:"x"`
}

type seenIface struct {
	SeenTextIface
	X int `json:"x"`
}

type seenJSONIfaceEmbed struct {
	SeenJSONIface
	X int `json:"x"`
}

// A marshaler promoted from an embedded type the walk does not expand is refused like any other:
// a described type, a standard-library type knownMarshalers lists, a marshaler interface of the
// standard library or of source.
func TestSkippedEmbeddedMarshalersAreUnresolved(t *testing.T) {
	source := `package things

import (
	"encoding"
	"encoding/json"
	"math/big"
	"net/netip"
)

type SeenDescribedObj struct {
	A int
}

func (SeenDescribedObj) MarshalJSON() ([]byte, error) { return []byte("{}"), nil }

type SeenTextIface interface {
	MarshalText() ([]byte, error)
}

type SeenJSONIface interface {
	json.Marshaler
}

type Described struct {
	SeenDescribedObj
	X int ` + "`json:\"x\"`" + `
}

type Netip struct {
	netip.Addr
	X int ` + "`json:\"x\"`" + `
}

type BigFloat struct {
	big.Float
	X int ` + "`json:\"x\"`" + `
}

type JSONMarshaler struct {
	json.Marshaler
	X int ` + "`json:\"x\"`" + `
}

type TextMarshaler struct {
	encoding.TextMarshaler
	X int ` + "`json:\"x\"`" + `
}

type Iface struct {
	SeenTextIface
	X int ` + "`json:\"x\"`" + `
}

type JSONIface struct {
	SeenJSONIface
	X int ` + "`json:\"x\"`" + `
}

`
	described := map[string]map[string]any{
		"example.com/app/things.SeenDescribedObj": {"type": "object", "properties": map[string]any{"custom": map[string]any{"type": "integer"}}},
	}
	bigFloat := &seenBigFloat{}
	bigFloat.SetFloat64(1.5)
	cases := []struct {
		name   string
		value  any
		writes any
		reason string
	}{
		{"Described", &seenDescribed{X: 7}, map[string]any{"custom": float64(1)}, "type Described embeds SeenDescribedObj, which defines MarshalJSON, so it marshals as SeenDescribedObj alone; Config.TypeSchemas describes SeenDescribedObj, not the types that embed it; describe it with Config.TypeSchemas"},
		{"Netip", &seenNetip{X: 7}, "", "type Netip embeds Addr, which defines MarshalText"},
		{"BigFloat", bigFloat, "1.5", "type BigFloat embeds Float, which defines MarshalText"},
		{"JSONMarshaler", &seenJSONMarshaler{Marshaler: SeenDescribedObj{}}, map[string]any{"custom": float64(1)}, "type JSONMarshaler embeds Marshaler, which defines MarshalJSON"},
		{"TextMarshaler", &seenTextMarshaler{TextMarshaler: SkipTextStruct{}}, "s", "type TextMarshaler embeds TextMarshaler, which defines MarshalText"},
		{"Iface", &seenIface{SeenTextIface: SkipTextStruct{}}, "s", "type Iface embeds SeenTextIface, which defines MarshalText"},
		{"JSONIface", &seenJSONIfaceEmbed{SeenJSONIface: SeenDescribedObj{}}, map[string]any{"custom": float64(1)}, "type JSONIface embeds SeenJSONIface, which defines MarshalJSON"},
	}
	for _, c := range cases {
		value := marshalled(t, c.value)
		if !reflect.DeepEqual(value, c.writes) {
			t.Fatalf("%s: expected encoding/json to write %v, got %v", c.name, c.writes, value)
		}

		builder := buildDescribed(t, source, described, c.name)
		err := builder.unresolvedError()
		if err == nil || !strings.Contains(err.Error(), c.reason) {
			t.Errorf("%s: expected %q, got %v (schema %v)", c.name, c.reason, err, builder.schemas["things_"+c.name])
		}
	}
}

// An alias cycle does not compile, so encoding/json has nothing to say about it; the generator
// must end, and fail generation, wherever it follows the alias: a field, a field with the
// `,string` option or omitempty, an embedded alias and a type declared from one.
func TestAliasCyclesFailGeneration(t *testing.T) {
	source := `package things

type A = B

type B = A

type Self = Self

type Field struct {
	F A ` + "`json:\"f\"`" + `
}

type SelfField struct {
	F Self ` + "`json:\"f\"`" + `
}

type Quoted struct {
	F A ` + "`json:\"f,string\"`" + `
}

type Omitted struct {
	F A ` + "`json:\"f,omitempty\"`" + `
}

type Embedded struct {
	A
	X int ` + "`json:\"x\"`" + `
}

type Declared B

type Holder struct {
	D Declared ` + "`json:\"d\"`" + `
}
`
	for _, name := range []string{"Field", "SelfField", "Quoted", "Omitted", "Embedded", "Holder"} {
		builder := buildThings(t, source, name)
		err := builder.unresolvedError()
		if err == nil || !strings.Contains(err.Error(), "alias chain longer than 8 aliases, or a cycle") {
			t.Errorf("%s: expected the alias cycle reported, got %v", name, err)
		}
	}
}

type ChainText int

func (ChainText) MarshalText() ([]byte, error) { return []byte("t"), nil }

type ChainT1 = ChainText

type ChainT2 = ChainT1

type ChainT3 = ChainT2

type ChainT4 = ChainT3

type ChainT5 = ChainT4

type ChainT6 = ChainT5

type ChainT7 = ChainT6

type ChainT8 = ChainT7

type ChainT9 = ChainT8

type chainD ChainT5

type chainDS ChainT3

type ChainS1 = []int

type ChainS2 = ChainS1

type ChainS3 = ChainS2

type ChainS4 = ChainS3

type chainDSlice ChainS4

type ChainStruct struct {
	A int
}

func (ChainStruct) MarshalText() ([]byte, error) { return []byte("c"), nil }

type ChainQ1 = ChainStruct

type ChainQ2 = ChainQ1

type ChainQ3 = ChainQ2

type ChainQ4 = ChainQ3

type ChainQ5 = ChainQ4

type chainDQ ChainQ5

type chainHolder struct {
	D     chainD      `json:"d"`
	S     chainDS     `json:"s,string"`
	Slice chainDSlice `json:"slice"`
	Q     chainDQ     `json:"q"`
}

type chainEmbed8 struct {
	ChainT8
	X int `json:"x"`
}

type chainEmbed9 struct {
	ChainT9
	X int `json:"x"`
}

// An alias chain takes no step of a walk's depth limit: a type declared from the fifth alias of
// a MarshalText type is its kind, is quoted by `,string` and, for a slice, is nullable. A chain
// of 8 aliases is followed; a longer one fails generation.
func TestLongAliasChains(t *testing.T) {
	source := `package things

type ChainText int

func (ChainText) MarshalText() ([]byte, error) { return []byte("t"), nil }

type ChainT1 = ChainText

type ChainT2 = ChainT1

type ChainT3 = ChainT2

type ChainT4 = ChainT3

type ChainT5 = ChainT4

type ChainT6 = ChainT5

type ChainT7 = ChainT6

type ChainT8 = ChainT7

type ChainT9 = ChainT8

type D ChainT5

type DS ChainT3

type ChainS1 = []int

type ChainS2 = ChainS1

type ChainS3 = ChainS2

type ChainS4 = ChainS3

type DSlice ChainS4

type ChainStruct struct {
	A int
}

func (ChainStruct) MarshalText() ([]byte, error) { return []byte("c"), nil }

type ChainQ1 = ChainStruct

type ChainQ2 = ChainQ1

type ChainQ3 = ChainQ2

type ChainQ4 = ChainQ3

type ChainQ5 = ChainQ4

type DQ ChainQ5

type Holder struct {
	D     D      ` + "`json:\"d\"`" + `
	S     DS     ` + "`json:\"s,string\"`" + `
	Slice DSlice ` + "`json:\"slice\"`" + `
	Q     DQ     ` + "`json:\"q\"`" + `
}

type Embed8 struct {
	ChainT8
	X int ` + "`json:\"x\"`" + `
}

type Embed9 struct {
	ChainT9
	X int ` + "`json:\"x\"`" + `
}
`
	builder := buildThings(t, source, "Holder")
	err := builder.unresolvedError()
	if err != nil {
		t.Fatal(err)
	}
	value := marshalled(t, &chainHolder{D: 7, S: 7, Q: chainDQ{A: 7}})
	want := map[string]any{"d": float64(7), "s": "7", "slice": nil, "q": map[string]any{"A": float64(7)}}
	if !reflect.DeepEqual(value, want) {
		t.Fatalf("expected encoding/json to write %v, got %v", want, value)
	}
	assertConforms(t, builder, builder.schemas["things_Holder"], value)
	assertConforms(t, builder, builder.schemas["things_Holder"], marshalled(t, &chainHolder{Slice: chainDSlice{1}}))

	embedded := []struct {
		name   string
		value  any
		reason string
	}{
		{"Embed8", &chainEmbed8{}, "type Embed8 embeds ChainT8, which defines MarshalText"},
		{"Embed9", &chainEmbed9{}, "things.ChainT9: alias chain longer than 8 aliases, or a cycle"},
	}
	for _, c := range embedded {
		value := marshalled(t, c.value)
		if value != "t" {
			t.Fatalf("%s: expected encoding/json to write the text, got %v", c.name, value)
		}
		builder := buildThings(t, source, c.name)
		err := builder.unresolvedError()
		if err == nil || !strings.Contains(err.Error(), c.reason) {
			t.Errorf("%s: expected %q, got %v", c.name, c.reason, err)
		}
	}
}

type DescPoint struct {
	X int
	Y int
}

func (DescPoint) MarshalJSON() ([]byte, error) { return []byte(`"p"`), nil }

type descArea DescPoint

type DescLevel int

func (DescLevel) MarshalText() ([]byte, error) { return []byte("high"), nil }

type descPriority DescLevel

type DescCSV []string

func (c DescCSV) MarshalText() ([]byte, error) { return []byte(strings.Join(c, ",")), nil }

type descTags DescCSV

type DescKind string

type descSub DescKind

type descHolder struct {
	Area     descArea     `json:"area"`
	Priority descPriority `json:"priority"`
	Quoted   descPriority `json:"quoted,string"`
	Tags     descTags     `json:"tags"`
	Sub      descSub      `json:"sub"`
}

// A type declared from a described type gets none of its methods. When the described type has a
// marshaler its description is of that method's output, so the declared type is described by its
// kind; without one the description refines the kind and the reference stays.
func TestDeclaredFromDescribedMarshalersAreTheirKind(t *testing.T) {
	source := `package things

import "strings"

type DescPoint struct {
	X int
	Y int
}

func (DescPoint) MarshalJSON() ([]byte, error) { return []byte("\"p\""), nil }

type Area DescPoint

type DescLevel int

func (DescLevel) MarshalText() ([]byte, error) { return []byte("high"), nil }

type Priority DescLevel

type DescCSV []string

func (c DescCSV) MarshalText() ([]byte, error) { return []byte(strings.Join(c, ",")), nil }

type Tags DescCSV

type DescKind string

type Sub DescKind

type Holder struct {
	Area     Area     ` + "`json:\"area\"`" + `
	Priority Priority ` + "`json:\"priority\"`" + `
	Quoted   Priority ` + "`json:\"quoted,string\"`" + `
	Tags     Tags     ` + "`json:\"tags\"`" + `
	Sub      Sub      ` + "`json:\"sub\"`" + `
}
`
	described := map[string]map[string]any{
		"example.com/app/things.DescPoint": {"type": "string"},
		"example.com/app/things.DescLevel": {"type": "string", "enum": []any{"high"}},
		"example.com/app/things.DescCSV":   {"type": "string"},
		"example.com/app/things.DescKind":  {"type": "string", "enum": []any{"a", "b"}},
	}
	builder := buildDescribed(t, source, described, "Holder")
	err := builder.unresolvedError()
	if err != nil {
		t.Fatal(err)
	}

	value := marshalled(t, &descHolder{Area: descArea{X: 1, Y: 2}, Priority: 3, Quoted: 3, Sub: "a"})
	want := map[string]any{"area": map[string]any{"X": float64(1), "Y": float64(2)}, "priority": float64(3), "quoted": "3", "tags": nil, "sub": "a"}
	if !reflect.DeepEqual(value, want) {
		t.Fatalf("expected encoding/json to write %v, got %v", want, value)
	}
	assertConforms(t, builder, builder.schemas["things_Holder"], value)
	assertConforms(t, builder, builder.schemas["things_Holder"], marshalled(t, &descHolder{Tags: descTags{"a"}, Sub: "b"}))
	if !reflect.DeepEqual(builder.schemas["things_Sub"], map[string]any{"$ref": "#/components/schemas/things_DescKind"}) {
		t.Errorf("expected Sub to keep the described DescKind, got %v", builder.schemas["things_Sub"])
	}
}

type numberEmbed struct {
	json.Number
	X int `json:"x"`
}

type numberField struct {
	N json.Number `json:"n"`
	Q json.Number `json:"q,string"`
}

// encoding/json writes a json.Number as a JSON number, also as the field an embedded one is.
func TestJSONNumberIsANumber(t *testing.T) {
	builder := buildThings(t, `package things

import "encoding/json"

type Embed struct {
	json.Number
	X int `+"`json:\"x\"`"+`
}

type Field struct {
	N json.Number `+"`json:\"n\"`"+`
	Q json.Number `+"`json:\"q,string\"`"+`
}
`, "Embed", "Field")
	err := builder.unresolvedError()
	if err != nil {
		t.Fatal(err)
	}

	embed := marshalled(t, &numberEmbed{Number: "7.5"})
	if !reflect.DeepEqual(embed, map[string]any{"Number": 7.5, "x": float64(0)}) {
		t.Fatalf("expected encoding/json to write the Number field, got %v", embed)
	}
	assertConforms(t, builder, builder.schemas["things_Embed"], embed)
	assertConforms(t, builder, builder.schemas["things_Embed"], marshalled(t, &numberEmbed{}))
	if len(nonConformities(builder.schemas, builder.schemas["things_Embed"], map[string]any{"x": float64(0)}, "$")) == 0 {
		t.Errorf("expected Number required, got %v", builder.schemas["things_Embed"])
	}

	field := marshalled(t, &numberField{N: "1.5", Q: "2"})
	if !reflect.DeepEqual(field, map[string]any{"n": 1.5, "q": "2"}) {
		t.Fatalf("expected encoding/json to write a number and a quoted number, got %v", field)
	}
	assertConforms(t, builder, builder.schemas["things_Field"], field)
}

type GenLevel int

func (GenLevel) MarshalText() ([]byte, error) { return []byte("high"), nil }

type GenBox[T any] struct {
	V T `json:"v"`
	GenLevel
}

type GenBoxInt = GenBox[int]

type GenBoxD GenBox[int]

type genRoot struct {
	GenBoxInt
	Name string `json:"name"`
}

type genRootD struct {
	GenBoxD
	Name string `json:"name"`
}

type GenJBox[T any] struct {
	V T `json:"v"`
}

func (GenJBox[T]) MarshalJSON() ([]byte, error) { return []byte(`"box"`), nil }

type GenJBoxInt = GenJBox[int]

type genJRoot struct {
	GenJBoxInt
	Name string `json:"name"`
}

type GenPair[K comparable, V any] struct {
	K K `json:"k"`
	V V `json:"v"`
}

func (GenPair[K, V]) MarshalJSON() ([]byte, error) { return []byte(`"pair"`), nil }

type genPRoot struct {
	GenPair[string, int]
	Name string `json:"name"`
}

type genPAnon struct {
	F struct {
		GenPair[string, int]
		Name string `json:"name"`
	} `json:"f"`
}

// A marshaler reached through an alias of a generic instantiation, a type declared from one, or
// declared on a receiver with two type parameters is promoted as Go promotes it.
func TestGenericInstantiationsPromoteMarshalers(t *testing.T) {
	source := `package things

type Level int

func (Level) MarshalText() ([]byte, error) { return []byte("high"), nil }

type Box[T any] struct {
	V T ` + "`json:\"v\"`" + `
	Level
}

type BoxInt = Box[int]

type BoxD Box[int]

type Root struct {
	BoxInt
	Name string ` + "`json:\"name\"`" + `
}

type RootD struct {
	BoxD
	Name string ` + "`json:\"name\"`" + `
}

type JBox[T any] struct {
	V T ` + "`json:\"v\"`" + `
}

func (JBox[T]) MarshalJSON() ([]byte, error) { return []byte("\"box\""), nil }

type JBoxInt = JBox[int]

type JRoot struct {
	JBoxInt
	Name string ` + "`json:\"name\"`" + `
}

type Pair[K comparable, V any] struct {
	K K ` + "`json:\"k\"`" + `
	V V ` + "`json:\"v\"`" + `
}

func (Pair[K, V]) MarshalJSON() ([]byte, error) { return []byte("\"pair\""), nil }

type PRoot struct {
	Pair[string, int]
	Name string ` + "`json:\"name\"`" + `
}

type PAnon struct {
	F struct {
		Pair[string, int]
		Name string ` + "`json:\"name\"`" + `
	} ` + "`json:\"f\"`" + `
}
`
	described := map[string]map[string]any{
		"example.com/app/things.Box":  {"type": "object"},
		"example.com/app/things.JBox": {"type": "string"},
		"example.com/app/things.Pair": {"type": "string"},
	}
	anonymous := genPAnon{}
	cases := []struct {
		name     string
		value    any
		promoter any
		writes   any
		reason   string
	}{
		{"Root", &genRoot{}, &genRoot{}, "high", "type Root embeds Level, which defines MarshalText"},
		{"RootD", &genRootD{}, &genRootD{}, "high", "type RootD embeds Level, which defines MarshalText"},
		{"JRoot", &genJRoot{}, &genJRoot{}, "box", "type JRoot embeds JBoxInt, which defines MarshalJSON"},
		{"PRoot", &genPRoot{}, &genPRoot{}, "pair", "type PRoot embeds Pair, which defines MarshalJSON"},
		{"PAnon", &anonymous, &anonymous.F, map[string]any{"f": "pair"}, "anonymous struct embeds Pair, which defines MarshalJSON"},
	}
	for _, c := range cases {
		if !promotesMarshaler(c.promoter) {
			t.Fatalf("%s: expected Go to promote a marshaler", c.name)
		}
		value := marshalled(t, c.value)
		if !reflect.DeepEqual(value, c.writes) {
			t.Fatalf("%s: expected encoding/json to write %v, got %v", c.name, c.writes, value)
		}

		builder := buildDescribed(t, source, described, c.name)
		err := builder.unresolvedError()
		if err == nil || !strings.Contains(err.Error(), c.reason) {
			t.Errorf("%s: expected %q, got %v (schema %v)", c.name, c.reason, err, builder.schemas["things_"+c.name])
		}
	}
}

type IfaceJM interface {
	MarshalJSON() ([]byte, error)
}

type IfaceTM interface {
	MarshalText() ([]byte, error)
}

type IfaceShape interface {
	Area() float64
}

type ifaceMarshalers struct {
	json.Marshaler
	IfaceJM
	Name string `json:"name"`
}

type ifaceTexts struct {
	encoding.TextMarshaler
	IfaceTM
	Name string `json:"name"`
}

type ifaceLevel struct {
	slog.Level
	IfaceJM
	IfaceTM
	Name string `json:"name"`
}

type ifaceUUID struct {
	uuid.UUID
	IfaceTM
	Name string `json:"name"`
}

// An embedded interface or non-struct whose marshal method Go does not promote, as another type
// at its depth declares it too, is a field named after its type: an interface one written as the
// value it holds or null, slog.Level as its name and uuid.UUID, a [16]byte, as its text.
func TestEmbeddedNonStructsInATieAreFields(t *testing.T) {
	source := `package things

import (
	"encoding"
	"encoding/json"
	"log/slog"

	"github.com/google/uuid"
)

type IfaceJM interface {
	MarshalJSON() ([]byte, error)
}

type IfaceTM interface {
	MarshalText() ([]byte, error)
}

type Marshalers struct {
	json.Marshaler
	IfaceJM
	Name string ` + "`json:\"name\"`" + `
}

type Texts struct {
	encoding.TextMarshaler
	IfaceTM
	Name string ` + "`json:\"name\"`" + `
}

type Level struct {
	slog.Level
	IfaceJM
	IfaceTM
	Name string ` + "`json:\"name\"`" + `
}

type UUID struct {
	uuid.UUID
	IfaceTM
	Name string ` + "`json:\"name\"`" + `
}
`
	cases := []struct {
		name     string
		values   []any
		writes   any
		required string
	}{
		{"Marshalers", []any{&ifaceMarshalers{}, &ifaceMarshalers{Marshaler: SeenDescribedObj{}, IfaceJM: SeenDescribedObj{}}}, map[string]any{"Marshaler": nil, "IfaceJM": nil, "name": ""}, ""},
		{"Texts", []any{&ifaceTexts{}, &ifaceTexts{TextMarshaler: SkipTextStruct{}, IfaceTM: SkipTextStruct{}}}, map[string]any{"TextMarshaler": nil, "IfaceTM": nil, "name": ""}, ""},
		{"Level", []any{&ifaceLevel{}, &ifaceLevel{Level: slog.LevelWarn, IfaceTM: SkipTextStruct{}}}, map[string]any{"Level": "INFO", "IfaceJM": nil, "IfaceTM": nil, "name": ""}, "Level"},
		{"UUID", []any{&ifaceUUID{}, &ifaceUUID{UUID: uuid.MustParse("6ba7b810-9dad-11d1-80b4-00c04fd430c8")}}, map[string]any{"UUID": "00000000-0000-0000-0000-000000000000", "IfaceTM": nil, "name": ""}, "UUID"},
	}
	for _, c := range cases {
		if promotesMarshaler(c.values[0]) {
			t.Fatalf("%s: expected Go to promote no marshaler", c.name)
		}
		zero := marshalled(t, c.values[0])
		if !reflect.DeepEqual(zero, c.writes) {
			t.Fatalf("%s: expected encoding/json to write %v, got %v", c.name, c.writes, zero)
		}

		builder := buildThings(t, source, c.name)
		err := builder.unresolvedError()
		if err != nil {
			t.Errorf("%s: expected its fields, got %v", c.name, err)
			continue
		}
		schema := builder.schemas["things_"+c.name]
		for _, value := range c.values {
			assertConforms(t, builder, schema, marshalled(t, value))
		}
		if c.required != "" {
			without := map[string]any{"name": ""}
			if len(nonConformities(builder.schemas, schema, without, "$")) == 0 {
				t.Errorf("%s: expected %s required, got %v", c.name, c.required, schema)
			}
		}
	}
}

type ifaceFields struct {
	M  json.Marshaler         `json:"m"`
	T  encoding.TextMarshaler `json:"t"`
	C  context.Context        `json:"c"`
	R  io.Reader              `json:"r"`
	S  fmt.Stringer           `json:"s"`
	E  error                  `json:"e"`
	A  any                    `json:"a"`
	J  IfaceJM                `json:"j"`
	Sh IfaceShape             `json:"sh"`
}

// A field of an interface type is written as the value it holds, whatever it is, or null: its
// schema admits any value. The predeclared error is one.
func TestInterfaceFieldsAdmitAnyValue(t *testing.T) {
	builder := buildThings(t, `package things

import (
	"context"
	"encoding"
	"encoding/json"
	"fmt"
	"io"
)

type IfaceJM interface {
	MarshalJSON() ([]byte, error)
}

type IfaceShape interface {
	Area() float64
}

type Fields struct {
	M  json.Marshaler         `+"`json:\"m\"`"+`
	T  encoding.TextMarshaler `+"`json:\"t\"`"+`
	C  context.Context        `+"`json:\"c\"`"+`
	R  io.Reader              `+"`json:\"r\"`"+`
	S  fmt.Stringer           `+"`json:\"s\"`"+`
	E  error                  `+"`json:\"e\"`"+`
	A  any                    `+"`json:\"a\"`"+`
	J  IfaceJM                `+"`json:\"j\"`"+`
	Sh IfaceShape             `+"`json:\"sh\"`"+`
}
`, "Fields")
	err := builder.unresolvedError()
	if err != nil {
		t.Fatal(err)
	}

	schema := builder.schemas["things_Fields"]
	empty := marshalled(t, &ifaceFields{})
	full := marshalled(t, &ifaceFields{
		M: SeenDescribedObj{},
		T: SkipTextStruct{},
		C: context.Background(),
		R: strings.NewReader("x"),
		S: time.March,
		E: errors.New("x"),
		A: 5,
		J: SeenDescribedObj{},
	})
	want := map[string]any{"m": map[string]any{"custom": float64(1)}, "t": "s", "c": map[string]any{}, "r": map[string]any{}, "s": float64(3), "e": map[string]any{}, "a": float64(5), "j": map[string]any{"custom": float64(1)}, "sh": nil}
	if !reflect.DeepEqual(full, want) {
		t.Fatalf("expected encoding/json to write %v, got %v", want, full)
	}
	assertConforms(t, builder, schema, empty)
	assertConforms(t, builder, schema, full)

	properties, _ := schema["properties"].(map[string]any)
	for name := range want {
		property, _ := properties[name].(map[string]any)
		for _, value := range []any{nil, true, float64(1), "x", []any{}, map[string]any{"k": "v"}} {
			problems := nonConformities(builder.schemas, property, value, "$."+name)
			if len(problems) > 0 {
				t.Errorf("%s: expected any value admitted, got %v (schema %v)", name, problems, property)
			}
		}
	}
}

type listedFields struct {
	A  netip.Addr     `json:"a"`
	P  netip.AddrPort `json:"p"`
	X  netip.Prefix   `json:"x"`
	F  *big.Float     `json:"f"`
	FV big.Float      `json:"fv"`
	R  *big.Rat       `json:"r"`
	L  slog.Level     `json:"l"`
	V  *slog.LevelVar `json:"v"`
	I  *big.Int       `json:"i"`
}

// A standard-library type the generator lists with MarshalText and without MarshalJSON is the
// string MarshalText returns, as a type read from source is, and slog.Level, whose MarshalJSON
// writes its name, is a string too.
func TestListedStandardLibraryFieldsAreTheirJSON(t *testing.T) {
	builder := buildThings(t, `package things

import (
	"log/slog"
	"math/big"
	"net/netip"
)

type Fields struct {
	A  netip.Addr     `+"`json:\"a\"`"+`
	P  netip.AddrPort `+"`json:\"p\"`"+`
	X  netip.Prefix   `+"`json:\"x\"`"+`
	F  *big.Float     `+"`json:\"f\"`"+`
	FV big.Float      `+"`json:\"fv\"`"+`
	R  *big.Rat       `+"`json:\"r\"`"+`
	L  slog.Level     `+"`json:\"l\"`"+`
	V  *slog.LevelVar `+"`json:\"v\"`"+`
	I  *big.Int       `+"`json:\"i\"`"+`
}
`, "Fields")
	err := builder.unresolvedError()
	if err != nil {
		t.Fatal(err)
	}

	zero := marshalled(t, &listedFields{})
	want := map[string]any{"a": "", "p": "", "x": "", "f": nil, "fv": "0", "r": nil, "l": "INFO", "v": nil, "i": nil}
	if !reflect.DeepEqual(zero, want) {
		t.Fatalf("expected encoding/json to write %v, got %v", want, zero)
	}
	full := &listedFields{
		A: netip.MustParseAddr("10.0.0.1"),
		F: big.NewFloat(1.5),
		R: big.NewRat(1, 2),
		L: slog.LevelWarn,
		V: &slog.LevelVar{},
		I: big.NewInt(7),
	}
	full.FV.SetFloat64(2.5)
	written := marshalled(t, full)
	if written.(map[string]any)["f"] != "1.5" || written.(map[string]any)["v"] != "INFO" {
		t.Fatalf("expected encoding/json to write the texts, got %v", written)
	}

	schema := builder.schemas["things_Fields"]
	assertConforms(t, builder, schema, zero)
	assertConforms(t, builder, schema, written)
	for _, name := range []string{"a", "l"} {
		without := map[string]any{"p": "", "x": "", "fv": "0"}
		if name == "a" {
			without["l"] = "INFO"
		} else {
			without["a"] = ""
		}
		if len(nonConformities(builder.schemas, schema, without, "$")) == 0 {
			t.Errorf("expected %s required, got %v", name, schema)
		}
	}
}

type DeclTime time.Time

type DeclLevel slog.Level

type DeclAddr netip.Addr

type DeclAmount json.Number

type DeclIP net.IP

type DeclID uuid.UUID

type DeclRaw json.RawMessage

type DeclInt big.Int

type DeclLevelVar slog.LevelVar

type declListed struct {
	T   DeclTime     `json:"t"`
	L   DeclLevel    `json:"l"`
	LS  DeclLevel    `json:"ls,string"`
	A   DeclAddr     `json:"a"`
	N   DeclAmount   `json:"n"`
	IP  DeclIP       `json:"ip"`
	ID  DeclID       `json:"id"`
	Raw DeclRaw      `json:"raw"`
	Int DeclInt      `json:"int"`
	LV  DeclLevelVar `json:"lv"`
}

type declEmbTime struct {
	DeclTime
	Name string `json:"name"`
}

type declEmbLevel struct {
	DeclLevel
	Name string `json:"name"`
}

// A type declared from a standard-library or well-known type with a marshal method gets none of
// its methods, and one declared from json.Number loses the exception encoding/json makes for that
// exact type: each is written by the type it is declared as.
func TestDeclaredFromListedTypesAreTheirKind(t *testing.T) {
	source := `package things

import (
	"encoding/json"
	"log/slog"
	"math/big"
	"net"
	"net/netip"
	"time"

	"github.com/google/uuid"
)

type DeclTime time.Time

type DeclLevel slog.Level

type DeclAddr netip.Addr

type DeclAmount json.Number

type DeclIP net.IP

type DeclID uuid.UUID

type DeclRaw json.RawMessage

type DeclInt big.Int

type DeclLevelVar slog.LevelVar

type Holder struct {
	T   DeclTime     ` + "`json:\"t\"`" + `
	L   DeclLevel    ` + "`json:\"l\"`" + `
	LS  DeclLevel    ` + "`json:\"ls,string\"`" + `
	A   DeclAddr     ` + "`json:\"a\"`" + `
	N   DeclAmount   ` + "`json:\"n\"`" + `
	IP  DeclIP       ` + "`json:\"ip\"`" + `
	ID  DeclID       ` + "`json:\"id\"`" + `
	Raw DeclRaw      ` + "`json:\"raw\"`" + `
	Int DeclInt      ` + "`json:\"int\"`" + `
	LV  DeclLevelVar ` + "`json:\"lv\"`" + `
}

type EmbTime struct {
	DeclTime
	Name string ` + "`json:\"name\"`" + `
}

type EmbLevel struct {
	DeclLevel
	Name string ` + "`json:\"name\"`" + `
}
`
	for _, value := range []any{DeclTime{}, DeclAddr{}, DeclAmount(""), DeclIP{}, DeclID{}, DeclRaw{}, &DeclInt{}, &DeclLevelVar{}, DeclLevel(0)} {
		if promotesMarshaler(value) {
			t.Fatalf("%T: expected Go to give it no marshaler", value)
		}
	}

	builder := buildThings(t, source, "Holder", "EmbTime", "EmbLevel")
	err := builder.unresolvedError()
	if err != nil {
		t.Fatal(err)
	}

	zero := marshalled(t, &declListed{})
	id := make([]any, 16)
	for i := range id {
		id[i] = float64(0)
	}
	want := map[string]any{"t": map[string]any{}, "l": float64(0), "ls": "0", "a": map[string]any{}, "n": "", "ip": nil, "id": id, "raw": nil, "int": map[string]any{}, "lv": map[string]any{}}
	if !reflect.DeepEqual(zero, want) {
		t.Fatalf("expected encoding/json to write %v, got %v", want, zero)
	}
	full := marshalled(t, &declListed{L: 4, LS: 4, N: "1.5", IP: DeclIP{1, 2, 3, 4}, Raw: DeclRaw(`{"a":1}`)})
	if full.(map[string]any)["n"] != "1.5" || full.(map[string]any)["ip"] != "AQIDBA==" || full.(map[string]any)["ls"] != "4" {
		t.Fatalf("expected encoding/json to write a string, base64 and a quoted int, got %v", full)
	}
	assertConforms(t, builder, builder.schemas["things_Holder"], zero)
	assertConforms(t, builder, builder.schemas["things_Holder"], full)

	embTime := marshalled(t, &declEmbTime{})
	if !reflect.DeepEqual(embTime, map[string]any{"name": ""}) {
		t.Fatalf("expected encoding/json to write the name alone, got %v", embTime)
	}
	assertConforms(t, builder, builder.schemas["things_EmbTime"], embTime)
	embLevel := marshalled(t, &declEmbLevel{DeclLevel: 4})
	if !reflect.DeepEqual(embLevel, map[string]any{"DeclLevel": float64(4), "name": ""}) {
		t.Fatalf("expected encoding/json to write the DeclLevel field, got %v", embLevel)
	}
	assertConforms(t, builder, builder.schemas["things_EmbLevel"], embLevel)
}

type DBase struct {
	B string `json:"b"`
}

type DWrapped DBase

func (DWrapped) MarshalText() ([]byte, error) { return []byte("w"), nil }

type dRoot struct {
	DWrapped
	IfaceTM
	Name string `json:"name"`
}

type DMoney struct {
	Cents int
}

func (DMoney) MarshalJSON() ([]byte, error) { return []byte(`"$0"`), nil }

type DPrice DMoney

type dPriceRoot struct {
	*DPrice
	Name string `json:"name"`
}

type dPriceHolder struct {
	P DPrice `json:"p"`
}

// A type declared from a described struct has that struct's fields, which a struct embedding it
// gets from source whatever the description says: a tie leaves its own MarshalText unpromoted,
// and a nil embedded pointer to it leaves them out. A field of such a type is described by those
// fields when the described struct has a marshaler.
func TestDeclaredFromDescribedStructsEmbedTheirFields(t *testing.T) {
	source := `package things

type Base struct {
	B string ` + "`json:\"b\"`" + `
}

type Wrapped Base

func (Wrapped) MarshalText() ([]byte, error) { return []byte("w"), nil }

type IfaceTM interface {
	MarshalText() ([]byte, error)
}

type Root struct {
	Wrapped
	IfaceTM
	Name string ` + "`json:\"name\"`" + `
}

type Money struct {
	Cents int
}

func (Money) MarshalJSON() ([]byte, error) { return []byte("\"$0\""), nil }

type Price Money

type PriceRoot struct {
	*Price
	Name string ` + "`json:\"name\"`" + `
}

type PriceHolder struct {
	P Price ` + "`json:\"p\"`" + `
}
`
	described := map[string]map[string]any{
		"example.com/app/things.Base":  {"type": "object", "properties": map[string]any{"b": map[string]any{"type": "string"}}},
		"example.com/app/things.Money": {"type": "string"},
	}
	cases := []struct {
		name   string
		values []any
		writes any
	}{
		{"Root", []any{&dRoot{}}, map[string]any{"IfaceTM": nil, "b": "", "name": ""}},
		{"PriceRoot", []any{&dPriceRoot{}, &dPriceRoot{DPrice: &DPrice{Cents: 5}}}, map[string]any{"name": ""}},
		{"PriceHolder", []any{&dPriceHolder{}}, map[string]any{"p": map[string]any{"Cents": float64(0)}}},
	}
	for _, c := range cases {
		if promotesMarshaler(c.values[0]) {
			t.Fatalf("%s: expected Go to promote no marshaler", c.name)
		}
		value := marshalled(t, c.values[0])
		if !reflect.DeepEqual(value, c.writes) {
			t.Fatalf("%s: expected encoding/json to write %v, got %v", c.name, c.writes, value)
		}

		builder := buildDescribed(t, source, described, c.name)
		err := builder.unresolvedError()
		if err != nil {
			t.Errorf("%s: expected its fields, got %v", c.name, err)
			continue
		}
		for _, value := range c.values {
			assertConforms(t, builder, builder.schemas["things_"+c.name], marshalled(t, value))
		}
	}
}

type StdD strings.Builder

type StdS interface {
	fmt.Stringer
}

type stdOnce struct {
	sync.Once
	Name string `json:"name"`
}

type stdWait struct {
	sync.WaitGroup
	Name string `json:"name"`
}

type stdContext struct {
	context.Context
	Name string `json:"name"`
}

type stdReader struct {
	io.ReadCloser
	Name string `json:"name"`
}

type stdStringer struct {
	fmt.Stringer
	Name string `json:"name"`
}

type stdBuilder struct {
	strings.Builder
	Name string `json:"name"`
}

type stdBuffer struct {
	bytes.Buffer
	Name string `json:"name"`
}

type stdDeclared struct {
	StdD
	Name string `json:"name"`
}

type stdIface struct {
	StdS
	Name string `json:"name"`
}

type stdRegexp struct {
	*regexp.Regexp
	Name string `json:"name"`
}

// The methods and embedded types of a standard-library type the generator does not list are read
// from GOROOT: a struct embedding one is refused only when Go promotes a marshaler from it, and an
// embedded interface is a field named after it.
func TestEmbeddedStandardLibraryTypesAreRead(t *testing.T) {
	source := `package things

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"sync"
)

type StdD strings.Builder

type StdS interface {
	fmt.Stringer
}

type Once struct {
	sync.Once
	Name string ` + "`json:\"name\"`" + `
}

type Wait struct {
	sync.WaitGroup
	Name string ` + "`json:\"name\"`" + `
}

type Context struct {
	context.Context
	Name string ` + "`json:\"name\"`" + `
}

type Reader struct {
	io.ReadCloser
	Name string ` + "`json:\"name\"`" + `
}

type Stringer struct {
	fmt.Stringer
	Name string ` + "`json:\"name\"`" + `
}

type Builder struct {
	strings.Builder
	Name string ` + "`json:\"name\"`" + `
}

type Buffer struct {
	bytes.Buffer
	Name string ` + "`json:\"name\"`" + `
}

type Declared struct {
	StdD
	Name string ` + "`json:\"name\"`" + `
}

type Iface struct {
	StdS
	Name string ` + "`json:\"name\"`" + `
}

type Regexp struct {
	*regexp.Regexp
	Name string ` + "`json:\"name\"`" + `
}
`
	cases := []struct {
		name   string
		values []any
		writes any
	}{
		{"Once", []any{&stdOnce{}}, map[string]any{"name": ""}},
		{"Wait", []any{&stdWait{}}, map[string]any{"name": ""}},
		{"Context", []any{&stdContext{}, &stdContext{Context: context.Background()}}, map[string]any{"Context": nil, "name": ""}},
		{"Reader", []any{&stdReader{}, &stdReader{ReadCloser: io.NopCloser(strings.NewReader("x"))}}, map[string]any{"ReadCloser": nil, "name": ""}},
		{"Stringer", []any{&stdStringer{}, &stdStringer{Stringer: time.March}}, map[string]any{"Stringer": nil, "name": ""}},
		{"Builder", []any{&stdBuilder{}}, map[string]any{"name": ""}},
		{"Buffer", []any{&stdBuffer{}}, map[string]any{"name": ""}},
		{"Declared", []any{&stdDeclared{}}, map[string]any{"name": ""}},
		{"Iface", []any{&stdIface{}, &stdIface{StdS: time.March}}, map[string]any{"StdS": nil, "name": ""}},
	}
	for _, c := range cases {
		if promotesMarshaler(c.values[0]) {
			t.Fatalf("%s: expected Go to promote no marshaler", c.name)
		}
		value := marshalled(t, c.values[0])
		if !reflect.DeepEqual(value, c.writes) {
			t.Fatalf("%s: expected encoding/json to write %v, got %v", c.name, c.writes, value)
		}

		builder := buildThings(t, source, c.name)
		err := builder.unresolvedError()
		if err != nil {
			t.Errorf("%s: expected its fields, got %v", c.name, err)
			continue
		}
		for _, value := range c.values {
			assertConforms(t, builder, builder.schemas["things_"+c.name], marshalled(t, value))
		}
	}

	promoted := &stdRegexp{Regexp: regexp.MustCompile("a+")}
	if !promotesMarshaler(promoted) || marshalled(t, promoted) != "a+" {
		t.Fatalf("expected Go to promote Regexp's MarshalText, got %v", marshalled(t, promoted))
	}
	builder := buildThings(t, source, "Regexp")
	err := builder.unresolvedError()
	if err == nil || !strings.Contains(err.Error(), "type Regexp embeds Regexp, which defines MarshalText") {
		t.Errorf("expected Regexp refused, got %v", err)
	}
}

// A struct embedding a type whose source cannot be read, where that type could decide whether a
// marshaler is promoted, is refused with a reason naming that type and the embedded types the
// struct reaches it through. Such source does not compile here, so there is no encoding/json
// output to compare with.
func TestUnreadableEmbeddedSourceIsNamed(t *testing.T) {
	source := `package things

import (
	"strings"

	"example.com/dep/gone"
)

type D gone.T

type Root struct {
	D
	X int ` + "`json:\"x\"`" + `
}

type Mid struct {
	gone.T
}

type Deep struct {
	Mid
	X int ` + "`json:\"x\"`" + `
}

type Builder struct {
	strings.Builder
	X int ` + "`json:\"x\"`" + `
}
`
	cases := []struct {
		name   string
		goroot bool
		reason string
	}{
		{"Root", false, "type Root embeds D, which comes from example.com/dep/gone.T, whose source the generator cannot read (package source not found at "},
		{"Deep", false, "type Deep embeds Mid, which embeds gone.T, whose source the generator cannot read (package source not found at "},
		{"Builder", true, "type Builder embeds strings.Builder, whose source the generator cannot read (GOROOT's source of strings not found at "},
	}
	for _, c := range cases {
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
		if c.goroot {
			builder.goroot = t.TempDir()
		}
		builder.schemaRef(typeRef{ImportPath: "example.com/app/things", Name: c.name})

		err = builder.unresolvedError()
		if err == nil || !strings.Contains(err.Error(), c.reason) {
			t.Errorf("%s: expected %q, got %v", c.name, c.reason, err)
		}
	}
}

type Ch8I1 Ch8I2
type Ch8I2 Ch8I3
type Ch8I3 Ch8I4
type Ch8I4 Ch8I5
type Ch8I5 Ch8I6
type Ch8I6 Ch8I7
type Ch8I7 Ch8I8
type Ch8I8 int

type Ch8L1 Ch8L2
type Ch8L2 Ch8L3
type Ch8L3 Ch8L4
type Ch8L4 Ch8L5
type Ch8L5 Ch8L6
type Ch8L6 Ch8L7
type Ch8L7 Ch8L8
type Ch8L8 []int

type Ch8S1 Ch8S2
type Ch8S2 Ch8S3
type Ch8S3 Ch8S4
type Ch8S4 Ch8S5
type Ch8S5 Ch8S6
type Ch8S6 Ch8S7
type Ch8S7 Ch8S8
type Ch8S8 struct {
	A int
}

type declChainHolder struct {
	E Ch8I1 `json:"e,string"`
	L Ch8L1 `json:"l"`
}

type declChainEmbed struct {
	Ch8I1
	*Ch8S1
	X int `json:"x"`
}

// A chain of 8 named types declared from one another is followed: the int at its end is quoted by
// `,string`, the slice is nullable, an embedded one is a field and the fields of an embedded
// pointer to the struct at its end are optional. A ninth type fails generation wherever the chain
// is followed.
func TestLongDeclarationChains(t *testing.T) {
	source := `package things

type Ch8I1 Ch8I2
type Ch8I2 Ch8I3
type Ch8I3 Ch8I4
type Ch8I4 Ch8I5
type Ch8I5 Ch8I6
type Ch8I6 Ch8I7
type Ch8I7 Ch8I8
type Ch8I8 int

type Ch8L1 Ch8L2
type Ch8L2 Ch8L3
type Ch8L3 Ch8L4
type Ch8L4 Ch8L5
type Ch8L5 Ch8L6
type Ch8L6 Ch8L7
type Ch8L7 Ch8L8
type Ch8L8 []int

type Ch8S1 Ch8S2
type Ch8S2 Ch8S3
type Ch8S3 Ch8S4
type Ch8S4 Ch8S5
type Ch8S5 Ch8S6
type Ch8S6 Ch8S7
type Ch8S7 Ch8S8
type Ch8S8 struct {
	A int
}

type Holder struct {
	E Ch8I1 ` + "`json:\"e,string\"`" + `
	L Ch8L1 ` + "`json:\"l\"`" + `
}

type Embed struct {
	Ch8I1
	*Ch8S1
	X int ` + "`json:\"x\"`" + `
}

type Ch9I1 Ch8I1

type Ch9L1 Ch8L1

type Ch9S1 Ch8S1

type Quoted9 struct {
	E Ch9I1 ` + "`json:\"e,string\"`" + `
}

type Nullable9 struct {
	L Ch9L1 ` + "`json:\"l\"`" + `
}

type EmbedInt9 struct {
	Ch9I1
	X int ` + "`json:\"x\"`" + `
}

type EmbedPointer9 struct {
	*Ch9S1
	X int ` + "`json:\"x\"`" + `
}
`
	builder := buildThings(t, source, "Holder", "Embed")
	err := builder.unresolvedError()
	if err != nil {
		t.Fatal(err)
	}
	holder := marshalled(t, &declChainHolder{})
	if !reflect.DeepEqual(holder, map[string]any{"e": "0", "l": nil}) {
		t.Fatalf("expected encoding/json to write a quoted int and null, got %v", holder)
	}
	assertConforms(t, builder, builder.schemas["things_Holder"], holder)
	assertConforms(t, builder, builder.schemas["things_Holder"], marshalled(t, &declChainHolder{L: Ch8L1{1}}))
	embed := marshalled(t, &declChainEmbed{})
	if !reflect.DeepEqual(embed, map[string]any{"Ch8I1": float64(0), "x": float64(0)}) {
		t.Fatalf("expected encoding/json to leave out the nil pointer's fields, got %v", embed)
	}
	assertConforms(t, builder, builder.schemas["things_Embed"], embed)
	assertConforms(t, builder, builder.schemas["things_Embed"], marshalled(t, &declChainEmbed{Ch8S1: &Ch8S1{A: 7}}))

	for _, name := range []string{"Quoted9", "Nullable9", "EmbedInt9", "EmbedPointer9"} {
		builder := buildThings(t, source, name)
		err := builder.unresolvedError()
		if err == nil || !strings.Contains(err.Error(), "reached through a chain of more than 8 types declared from one another, or a cycle") {
			t.Errorf("%s: expected the chain reported, got %v", name, err)
		}
	}
}

type GiBox[T any] struct {
	Val T `json:"val"`
}

func (GiBox[T]) MarshalJSON() ([]byte, error) { return []byte(`"box"`), nil }

type GiBoxInt = GiBox[int]

type GiBoxD GiBox[int]

type GiOther struct {
	O int
}

func (GiOther) MarshalJSON() ([]byte, error) { return []byte("1"), nil }

type giTie struct {
	GiBoxInt
	GiOther
	X int `json:"x"`
}

type giTieDirect struct {
	GiBox[int]
	GiOther
	X int `json:"x"`
}

type giHold struct {
	B GiBoxD `json:"b"`
}

type giEmbedPointer struct {
	*GiBoxD
}

type GiPlain[T any] struct {
	Val T `json:"val"`
}

type GiD GiPlain[int]

type giShared struct {
	GiD
	Val string `json:"val"`
}

type giPointer struct {
	*GiD
	X int `json:"x"`
}

type giByValue struct {
	GiD
	X int `json:"x"`
}

type giDirect struct {
	GiPlain[int]
	X int `json:"x"`
}

// The generator does not substitute a generic type's arguments into its fields, so where a
// schema needs an instantiation's fields it fails generation naming the type to describe: a type
// declared from an instantiation of a generic that has a marshal method, which it does not get,
// and a struct embedding an instantiation, an alias of one or a type declared from one where a
// tie leaves the instantiation's marshal method unpromoted, one of its field names is shared or
// it is embedded through a pointer. An instantiation embedded by value without any of these is
// the description of its generic.
func TestGenericFieldsAreNotInstantiated(t *testing.T) {
	source := `package things

type Box[T any] struct {
	Val T ` + "`json:\"val\"`" + `
}

func (Box[T]) MarshalJSON() ([]byte, error) { return []byte("\"box\""), nil }

type BoxInt = Box[int]

type BoxD Box[int]

type Other struct {
	O int
}

func (Other) MarshalJSON() ([]byte, error) { return []byte("1"), nil }

type Tie struct {
	BoxInt
	Other
	X int ` + "`json:\"x\"`" + `
}

type TieDirect struct {
	Box[int]
	Other
	X int ` + "`json:\"x\"`" + `
}

type Hold struct {
	B BoxD ` + "`json:\"b\"`" + `
}

type EmbedPointer struct {
	*BoxD
}

type Plain[T any] struct {
	Val T ` + "`json:\"val\"`" + `
}

type D Plain[int]

type Shared struct {
	D
	Val string ` + "`json:\"val\"`" + `
}

type Pointer struct {
	*D
	X int ` + "`json:\"x\"`" + `
}

type ByValue struct {
	D
	X int ` + "`json:\"x\"`" + `
}

type Direct struct {
	Plain[int]
	X int ` + "`json:\"x\"`" + `
}
`
	described := map[string]map[string]any{
		"example.com/app/things.Box":   {"type": "string"},
		"example.com/app/things.Plain": {"type": "object", "required": []any{"val"}, "properties": map[string]any{"val": map[string]any{"type": "integer"}}},
	}
	refused := []struct {
		name   string
		value  any
		writes any
		reason string
	}{
		{"BoxD", &GiBoxD{}, map[string]any{"val": float64(0)}, `type BoxD is declared from Box[int] and does not get its MarshalJSON, so encoding/json writes it as the fields of Box, whose types the generator does not instantiate; describe it with Config.TypeSchemas["example.com/app/things.BoxD"]`},
		{"Hold", &giHold{}, map[string]any{"b": map[string]any{"val": float64(0)}}, "type BoxD is declared from Box[int] and does not get its MarshalJSON"},
		{"Tie", &giTie{}, map[string]any{"val": float64(0), "O": float64(0), "x": float64(0)}, "type Tie embeds BoxInt, an instantiation of a generic type whose fields' types the generator does not instantiate, so it can only be the allOf member referencing its description, and that is not what encoding/json writes: it has MarshalJSON, which is not promoted here, so encoding/json writes its fields while its description is of MarshalJSON's output"},
		{"TieDirect", &giTieDirect{}, map[string]any{"val": float64(0), "O": float64(0), "x": float64(0)}, "type TieDirect embeds Box[int], an instantiation of a generic type"},
		{"EmbedPointer", &giEmbedPointer{}, map[string]any{}, "type EmbedPointer embeds BoxD, an instantiation of a generic type whose fields' types the generator does not instantiate, so it can only be the allOf member referencing its description, and that is not what encoding/json writes: it is embedded through a pointer, and a nil one leaves its fields out"},
		{"Shared", &giShared{}, map[string]any{"val": ""}, "type Shared embeds D, an instantiation of a generic type whose fields' types the generator does not instantiate, so it can only be the allOf member referencing its description, and that is not what encoding/json writes: its field \"val\" shares its name with another field of the struct, so encoding/json writes that other field or neither"},
		{"Pointer", &giPointer{}, map[string]any{"x": float64(0)}, "type Pointer embeds D, an instantiation of a generic type whose fields' types the generator does not instantiate, so it can only be the allOf member referencing its description, and that is not what encoding/json writes: it is embedded through a pointer, and a nil one leaves its fields out"},
	}
	for _, c := range refused {
		if promotesMarshaler(c.value) {
			t.Fatalf("%s: expected Go to promote no marshaler", c.name)
		}
		value := marshalled(t, c.value)
		if !reflect.DeepEqual(value, c.writes) {
			t.Fatalf("%s: expected encoding/json to write %v, got %v", c.name, c.writes, value)
		}

		builder := buildDescribed(t, source, described, c.name)
		err := builder.unresolvedError()
		if err == nil || !strings.Contains(err.Error(), c.reason) {
			t.Errorf("%s: expected %q, got %v (schemas %v)", c.name, c.reason, err, builder.schemas)
		}
	}

	kept := []struct {
		name   string
		values []any
		ref    string
	}{
		{"ByValue", []any{&giByValue{}, &giByValue{GiD: GiD{Val: 3}, X: 4}}, "things_D"},
		{"Direct", []any{&giDirect{}, &giDirect{GiPlain: GiPlain[int]{Val: 3}, X: 4}}, "things_Plain"},
	}
	for _, c := range kept {
		builder := buildDescribed(t, source, described, c.name)
		err := builder.unresolvedError()
		if err != nil {
			t.Errorf("%s: expected the description kept, got %v", c.name, err)
			continue
		}
		schema := builder.schemas["things_"+c.name]
		allOf, _ := schema["allOf"].([]any)
		if len(allOf) == 0 || !reflect.DeepEqual(allOf[0], map[string]any{"$ref": schemaRefPrefix + c.ref}) {
			t.Errorf("%s: expected a $ref to %s, got %v", c.name, c.ref, schema)
		}
		for _, value := range c.values {
			assertConforms(t, builder, schema, marshalled(t, value))
		}
	}
}

type h2Reflect struct {
	reflect.Value
	X int `json:"x"`
}

type h2Errno struct {
	syscall.Errno
	X int `json:"x"`
}

type H2Handle uintptr

type h2Ints struct {
	A uintptr  `json:"a"`
	H H2Handle `json:"h"`
}

type h2Complex struct {
	C complex128 `json:"c"`
}

// uintptr is an integer wherever a predeclared type is, so a standard-library type declared from
// one, embedded or not, is an integer too. complex64 and complex128, which encoding/json cannot
// marshal, fail generation saying so.
func TestUintptrAndComplexFields(t *testing.T) {
	source := `package things

import (
	"reflect"
	"syscall"
)

type Reflect struct {
	reflect.Value
	X int ` + "`json:\"x\"`" + `
}

type Errno struct {
	syscall.Errno
	X int ` + "`json:\"x\"`" + `
}

type Handle uintptr

type Ints struct {
	A uintptr ` + "`json:\"a\"`" + `
	H Handle  ` + "`json:\"h\"`" + `
}

type Complex struct {
	C complex128 ` + "`json:\"c\"`" + `
}

type Complex64 struct {
	C complex64 ` + "`json:\"c\"`" + `
}
`
	cases := []struct {
		name   string
		values []any
		writes any
	}{
		{"Reflect", []any{&h2Reflect{}, &h2Reflect{Value: reflect.ValueOf(1), X: 2}}, map[string]any{"x": float64(0)}},
		{"Errno", []any{&h2Errno{}, &h2Errno{Errno: syscall.ENOENT, X: 2}}, map[string]any{"Errno": float64(0), "x": float64(0)}},
		{"Ints", []any{&h2Ints{}, &h2Ints{A: 7, H: 9}}, map[string]any{"a": float64(0), "h": float64(0)}},
	}
	for _, c := range cases {
		value := marshalled(t, c.values[0])
		if !reflect.DeepEqual(value, c.writes) {
			t.Fatalf("%s: expected encoding/json to write %v, got %v", c.name, c.writes, value)
		}

		builder := buildThings(t, source, c.name)
		err := builder.unresolvedError()
		if err != nil {
			t.Errorf("%s: expected its fields, got %v", c.name, err)
			continue
		}
		schema := builder.schemas["things_"+c.name]
		for _, value := range c.values {
			assertConforms(t, builder, schema, marshalled(t, value))
		}
		wrong := map[string]any{}
		for key := range c.writes.(map[string]any) {
			wrong[key] = "x"
		}
		if len(nonConformities(builder.schemas, schema, wrong, "$")) == 0 {
			t.Errorf("%s: expected strings refused, got %v", c.name, schema)
		}
	}

	_, err := json.Marshal(&h2Complex{})
	if err == nil || !strings.Contains(err.Error(), "json: unsupported type: complex128") {
		t.Fatalf("expected encoding/json to refuse a complex128, got %v", err)
	}
	for _, name := range []string{"Complex", "Complex64"} {
		builder := buildThings(t, source, name)
		err := builder.unresolvedError()
		if err == nil || !strings.Contains(err.Error(), "encoding/json cannot marshal a complex") || strings.Contains(err.Error(), "not declared") {
			t.Errorf("%s: expected the complex field refused, got %v", name, err)
		}
	}
}

// The document is the same on every machine: a standard-library type the generator does not list,
// as a field or embedded, fails generation where GOROOT's source cannot be read, with a reason
// saying where it looked and why it failed. Types it lists need no GOROOT.
func TestUnreadableGOROOTFailsGeneration(t *testing.T) {
	source := `package things

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"
)

type Context struct {
	C context.Context ` + "`json:\"c\"`" + `
}

type Reader struct {
	R io.Reader ` + "`json:\"r\"`" + `
}

type Month struct {
	M time.Month ` + "`json:\"m\"`" + `
}

type Builder struct {
	strings.Builder
	X int ` + "`json:\"x\"`" + `
}

type Listed struct {
	T  time.Time       ` + "`json:\"t\"`" + `
	D  time.Duration   ` + "`json:\"d\"`" + `
	ID uuid.UUID       ` + "`json:\"id\"`" + `
	J  json.RawMessage ` + "`json:\"j\"`" + `
	N  json.Number     ` + "`json:\"n\"`" + `
	S  sql.NullString  ` + "`json:\"s\"`" + `
	V  url.Values      ` + "`json:\"v\"`" + `
	H  http.Header     ` + "`json:\"h\"`" + `
	A  netip.Addr      ` + "`json:\"a\"`" + `
	B  *big.Int        ` + "`json:\"b\"`" + `
	U  *url.URL        ` + "`json:\"u\"`" + `
	E  error           ` + "`json:\"e\"`" + `
}
`
	empty := t.TempDir()
	missing := []struct {
		name   string
		reason string
	}{
		{"Context", "example.com/app/things.Context: the generator reads this standard-library type from GOROOT's source, which it cannot read: GOROOT's source of context not found at " + filepath.Join(empty, "src", "context")},
		{"Reader", "GOROOT's source of io not found at " + filepath.Join(empty, "src", "io")},
		{"Month", "GOROOT's source of time not found at " + filepath.Join(empty, "src", "time")},
		{"Builder", "GOROOT's source of strings not found at " + filepath.Join(empty, "src", "strings")},
	}
	for _, c := range missing {
		repoRoot := t.TempDir()
		writeThings(t, repoRoot, source)
		builder := newTestSchemaBuilder(t, repoRoot)
		builder.goroot = empty
		builder.schemaRef(typeRef{ImportPath: "example.com/app/things", Name: c.name})

		err := builder.unresolvedError()
		if err == nil || !strings.Contains(err.Error(), strings.TrimPrefix(c.reason, "example.com/app/things.Context: ")) || strings.Contains(err.Error(), "An internal error has occurred") {
			t.Errorf("%s: expected %q, got %v", c.name, c.reason, err)
		}
	}

	// Without GOROOT set and without go on the PATH, the reason is go's own failure.
	t.Setenv("GOROOT", "")
	t.Setenv("PATH", "")
	repoRoot := t.TempDir()
	writeThings(t, repoRoot, source)
	builder := newTestSchemaBuilder(t, repoRoot)
	builder.schemaRef(typeRef{ImportPath: "example.com/app/things", Name: "Month"})
	err := builder.unresolvedError()
	want := "which it cannot read: GOROOT is not set and `go env GOROOT` failed: exec: \"go\": executable file not found in $PATH"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("expected %q, got %v", want, err)
	}

	builder = newTestSchemaBuilder(t, repoRoot)
	builder.schemaRef(typeRef{ImportPath: "example.com/app/things", Name: "Listed"})
	err = builder.unresolvedError()
	if err != nil {
		t.Errorf("expected the listed types described without GOROOT, got %v", err)
	}
}

// writeThings writes source as the package example.com/app/things under repoRoot.
func writeThings(t *testing.T, repoRoot string, source string) {
	t.Helper()

	pkgDir := filepath.Join(repoRoot, "things")
	err := os.MkdirAll(pkgDir, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(filepath.Join(pkgDir, "things.go"), []byte(source), 0o600)
	if err != nil {
		t.Fatal(err)
	}
}

type H4Token json.Token

type H4Any any

type h4Fields struct {
	J json.Token           `json:"j"`
	X xml.Token            `json:"x"`
	P crypto.PublicKey     `json:"p"`
	K crypto.PrivateKey    `json:"k"`
	O crypto.DecrypterOpts `json:"o"`
	V driver.Value         `json:"v"`
	D H4Token              `json:"d"`
	A H4Any                `json:"a"`
}

// A standard-library interface declared as `type X any`, a type declared from one and a source
// `type X any` are interfaces: each is written as the value it holds, whatever it is, or null.
func TestAnyInterfacesAdmitAnyValue(t *testing.T) {
	builder := buildThings(t, `package things

import (
	"crypto"
	"database/sql/driver"
	"encoding/json"
	"encoding/xml"
)

type Token json.Token

type Any any

type Fields struct {
	J json.Token          `+"`json:\"j\"`"+`
	X xml.Token           `+"`json:\"x\"`"+`
	P crypto.PublicKey    `+"`json:\"p\"`"+`
	K crypto.PrivateKey   `+"`json:\"k\"`"+`
	O crypto.DecrypterOpts `+"`json:\"o\"`"+`
	V driver.Value        `+"`json:\"v\"`"+`
	D Token               `+"`json:\"d\"`"+`
	A Any                 `+"`json:\"a\"`"+`
}
`, "Fields")
	err := builder.unresolvedError()
	if err != nil {
		t.Fatal(err)
	}

	schema := builder.schemas["things_Fields"]
	empty := marshalled(t, &h4Fields{})
	full := marshalled(t, &h4Fields{J: "s", X: xml.CharData("c"), P: 3, K: true, O: []int{1}, V: int64(4), D: json.Delim('['), A: map[string]int{"k": 1}})
	want := map[string]any{"j": "s", "x": "Yw==", "p": float64(3), "k": true, "o": []any{float64(1)}, "v": float64(4), "d": float64('['), "a": map[string]any{"k": float64(1)}}
	if !reflect.DeepEqual(full, want) {
		t.Fatalf("expected encoding/json to write %v, got %v", want, full)
	}
	assertConforms(t, builder, schema, empty)
	assertConforms(t, builder, schema, full)

	properties, _ := schema["properties"].(map[string]any)
	for name := range want {
		property, _ := properties[name].(map[string]any)
		for _, value := range []any{nil, "str", float64(3), true, []any{}, map[string]any{"k": "v"}} {
			problems := nonConformities(builder.schemas, property, value, "$."+name)
			if len(problems) > 0 {
				t.Errorf("%s: expected any value admitted, got %v (schema %v)", name, problems, property)
			}
		}
	}
}

type H5Month time.Month

type H5MonthAgain H5Month

type H5Regexp regexp.Regexp

type h5Fields struct {
	Mo  time.Month            `json:"mo"`
	MoS time.Month            `json:"mos,string"`
	W   time.Weekday          `json:"w"`
	OM  os.FileMode           `json:"om"`
	FM  fs.FileMode           `json:"fm"`
	CS  http.ConnState        `json:"cs"`
	K   reflect.Kind          `json:"k"`
	H   template.HTML         `json:"h"`
	ST  reflect.StructTag     `json:"st"`
	HD  http.Dir              `json:"hd"`
	RE  regexp.Regexp         `json:"re"`
	REP *regexp.Regexp        `json:"rep"`
	OID x509.OID              `json:"oid"`
	IM  net.IPMask            `json:"im"`
	HA  net.HardwareAddr      `json:"ha"`
	RB  sql.RawBytes          `json:"rb"`
	IS  sort.IntSlice         `json:"is"`
	AO  asn1.ObjectIdentifier `json:"ao"`
	XM  H5Month               `json:"xm"`
	YM  H5MonthAgain          `json:"ym"`
	XMS H5MonthAgain          `json:"xms,string"`
	RR  *H5Regexp             `json:"rr"`
}

type h5Tagged struct {
	time.Month `json:"emb"`
	X          int `json:"x"`
}

// A standard-library type the generator does not list is described from its declaration in
// GOROOT by the rules for a type read from source: by its kind, as the string MarshalText writes
// where it has no MarshalJSON, quoted by `,string`, nullable where nil, and the same for a type
// declared from it and for one embedded with a name in its tag. A standard-library struct without
// a marshal method is an object without fields.
func TestUnlistedStandardLibraryTypesFollowTheirDeclaration(t *testing.T) {
	builder := buildThings(t, `package things

import (
	"crypto/x509"
	"database/sql"
	"encoding/asn1"
	"html/template"
	"io/fs"
	"net"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"time"
)

type Month time.Month

type MonthAgain Month

type Regexp regexp.Regexp

type Fields struct {
	Mo  time.Month            `+"`json:\"mo\"`"+`
	MoS time.Month            `+"`json:\"mos,string\"`"+`
	W   time.Weekday          `+"`json:\"w\"`"+`
	OM  os.FileMode           `+"`json:\"om\"`"+`
	FM  fs.FileMode           `+"`json:\"fm\"`"+`
	CS  http.ConnState        `+"`json:\"cs\"`"+`
	K   reflect.Kind          `+"`json:\"k\"`"+`
	H   template.HTML         `+"`json:\"h\"`"+`
	ST  reflect.StructTag     `+"`json:\"st\"`"+`
	HD  http.Dir              `+"`json:\"hd\"`"+`
	RE  regexp.Regexp         `+"`json:\"re\"`"+`
	REP *regexp.Regexp        `+"`json:\"rep\"`"+`
	OID x509.OID              `+"`json:\"oid\"`"+`
	IM  net.IPMask            `+"`json:\"im\"`"+`
	HA  net.HardwareAddr      `+"`json:\"ha\"`"+`
	RB  sql.RawBytes          `+"`json:\"rb\"`"+`
	IS  sort.IntSlice         `+"`json:\"is\"`"+`
	AO  asn1.ObjectIdentifier `+"`json:\"ao\"`"+`
	XM  Month                 `+"`json:\"xm\"`"+`
	YM  MonthAgain            `+"`json:\"ym\"`"+`
	XMS MonthAgain            `+"`json:\"xms,string\"`"+`
	RR  *Regexp               `+"`json:\"rr\"`"+`
}

type Tagged struct {
	time.Month `+"`json:\"emb\"`"+`
	X          int `+"`json:\"x\"`"+`
}
`, "Fields", "Tagged")
	err := builder.unresolvedError()
	if err != nil {
		t.Fatal(err)
	}

	oid, err := x509.ParseOID("1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	full := &h5Fields{
		Mo:  time.March,
		MoS: time.April,
		W:   time.Friday,
		OM:  0o755,
		FM:  fs.ModeDir,
		CS:  http.StateIdle,
		K:   reflect.Int,
		H:   "<b>",
		ST:  `json:"x"`,
		HD:  "/tmp",
		RE:  *regexp.MustCompile("a+"),
		REP: regexp.MustCompile("b"),
		OID: oid,
		IM:  net.CIDRMask(24, 32),
		HA:  net.HardwareAddr{1, 2, 3, 4, 5, 6},
		RB:  sql.RawBytes("x"),
		IS:  sort.IntSlice{1},
		AO:  asn1.ObjectIdentifier{1, 2},
		XM:  5,
		YM:  6,
		XMS: 7,
		RR:  &H5Regexp{},
	}
	value := marshalled(t, full)
	want := map[string]any{
		"mo": float64(3), "mos": "4", "w": float64(5), "om": float64(0o755), "fm": float64(fs.ModeDir), "cs": float64(http.StateIdle), "k": float64(reflect.Int),
		"h": "<b>", "st": `json:"x"`, "hd": "/tmp", "re": "a+", "rep": "b", "oid": "1.2.3", "im": "////AA==", "ha": "AQIDBAUG", "rb": "eA==",
		"is": []any{float64(1)}, "ao": []any{float64(1), float64(2)}, "xm": float64(5), "ym": float64(6), "xms": "7", "rr": map[string]any{},
	}
	if !reflect.DeepEqual(value, want) {
		t.Fatalf("expected encoding/json to write %v, got %v", want, value)
	}
	schema := builder.schemas["things_Fields"]
	assertConforms(t, builder, schema, value)
	assertConforms(t, builder, schema, marshalled(t, &h5Fields{OID: oid}))

	wrong := map[string]any{
		"mo": "x", "mos": float64(4), "w": "x", "om": "x", "fm": "x", "cs": "x", "k": "x", "h": float64(1), "st": float64(1), "hd": float64(1), "re": float64(1),
		"rep": float64(1), "oid": float64(1), "im": float64(1), "ha": float64(1), "rb": float64(1), "is": "x", "ao": "x", "xm": "x", "ym": "x", "xms": float64(7), "rr": "x",
	}
	for name, bad := range wrong {
		properties, _ := schema["properties"].(map[string]any)
		property, _ := properties[name].(map[string]any)
		if len(nonConformities(builder.schemas, property, bad, "$."+name)) == 0 {
			t.Errorf("%s: expected %v refused, got %v", name, bad, property)
		}
	}

	tagged := marshalled(t, &h5Tagged{Month: time.May})
	if !reflect.DeepEqual(tagged, map[string]any{"emb": float64(5), "x": float64(0)}) {
		t.Fatalf("expected encoding/json to write the tagged embed as a field, got %v", tagged)
	}
	assertConforms(t, builder, builder.schemas["things_Tagged"], tagged)
	if len(nonConformities(builder.schemas, builder.schemas["things_Tagged"], map[string]any{"emb": "May", "x": float64(0)}, "$")) == 0 {
		t.Errorf("expected the tagged month an integer, got %v", builder.schemas["things_Tagged"])
	}
}

// A standard-library type with MarshalJSON that the generator does not list is the empty schema,
// as what it writes is not read; one with MarshalText is a string, and a struct declared from
// either gets neither method and is an object whose fields are not read, as for any
// standard-library struct. The package is a stand-in written to GOROOT, as
// the standard library's own such types are all listed.
func TestUnlistedStandardLibraryMarshalers(t *testing.T) {
	goroot := t.TempDir()
	pkgDir := filepath.Join(goroot, "src", "stdfake")
	err := os.MkdirAll(pkgDir, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(filepath.Join(pkgDir, "fake.go"), []byte(`package stdfake

type J struct {
	A int
}

func (J) MarshalJSON() ([]byte, error) { return []byte("1"), nil }

type T struct {
	A int
}

func (*T) MarshalText() ([]byte, error) { return []byte("t"), nil }

type Alias = J
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	repoRoot := t.TempDir()
	writeThings(t, repoRoot, `package things

import "stdfake"

type FromJ stdfake.J

type Fields struct {
	J stdfake.J     `+"`json:\"j\"`"+`
	T stdfake.T     `+"`json:\"t\"`"+`
	A stdfake.Alias `+"`json:\"a\"`"+`
	D FromJ         `+"`json:\"d\"`"+`
}
`)
	builder := newTestSchemaBuilder(t, repoRoot)
	builder.goroot = goroot
	builder.schemaRef(typeRef{ImportPath: "example.com/app/things", Name: "Fields"})
	err = builder.unresolvedError()
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]map[string]any{
		"stdfake_J":    {},
		"stdfake_T":    {"type": "string"},
		"things_FromJ": {"type": "object", "properties": map[string]any{}},
	}
	for name, schema := range want {
		if !reflect.DeepEqual(builder.schemas[name], schema) {
			t.Errorf("%s: expected %v, got %v", name, schema, builder.schemas[name])
		}
	}
}

type K1Box[T any] struct {
	V    T      `json:"v"`
	Name string `json:"name"`
}

type K1BoxInt = K1Box[int]

type K1D K1Box[int]

type K1Other struct {
	Q int `json:"q"`
}

type K1CA struct {
	Q int
}

type K1CB struct {
	Q string
}

type K1M1 struct {
	A int
}

func (K1M1) MarshalJSON() ([]byte, error) { return []byte("1"), nil }

type K1M2 struct {
	B int
}

func (K1M2) MarshalJSON() ([]byte, error) { return []byte("2"), nil }

type k1PointerBeside struct {
	*K1Other
	K1Box[int]
}

type k1TieBeside struct {
	K1CA
	K1CB
	K1Box[int]
}

type k1MarshalersBeside struct {
	K1Box[int]
	K1M1
	K1M2
}

type k1AliasBeside struct {
	*K1Other
	K1BoxInt
}

type k1DeclaredBeside struct {
	*K1Other
	K1D
}

type K1Mid struct {
	K1Box[int]
}

type k1UnderPointer struct {
	*K1Mid
	X int `json:"x"`
}

type k1SharedOwn struct {
	*K1Other
	K1Box[int]
	Name int `json:"name"`
}

type K1Inner struct {
	I int `json:"i"`
}

type K1Wrap[T any] struct {
	K1Inner
	V T `json:"v"`
}

type k1EmbedsInside struct {
	*K1Other
	K1Wrap[int]
}

// A struct merged for a reason that does not involve an embedded generic instantiation keeps it
// as the allOf member referencing the generic type's description, as the instantiation's field
// names, which the walk reads, do not depend on its type arguments: beside an embedded pointer,
// a tie between other embedded types' names, or a tie between other embedded types' marshalers.
// It is refused when the generic type is involved: embedded through a pointer above it, one of
// its field names shared with the struct's own, or its declaration embedding a type.
func TestGenericBesideMergedFields(t *testing.T) {
	source := `package things

type Box[T any] struct {
	V    T      ` + "`json:\"v\"`" + `
	Name string ` + "`json:\"name\"`" + `
}

type BoxInt = Box[int]

type D Box[int]

type Other struct {
	Q int ` + "`json:\"q\"`" + `
}

type CA struct {
	Q int
}

type CB struct {
	Q string
}

type M1 struct {
	A int
}

func (M1) MarshalJSON() ([]byte, error) { return []byte("1"), nil }

type M2 struct {
	B int
}

func (M2) MarshalJSON() ([]byte, error) { return []byte("2"), nil }

type PointerBeside struct {
	*Other
	Box[int]
}

type TieBeside struct {
	CA
	CB
	Box[int]
}

type MarshalersBeside struct {
	Box[int]
	M1
	M2
}

type AliasBeside struct {
	*Other
	BoxInt
}

type DeclaredBeside struct {
	*Other
	D
}

type Mid struct {
	Box[int]
}

type UnderPointer struct {
	*Mid
	X int ` + "`json:\"x\"`" + `
}

type SharedOwn struct {
	*Other
	Box[int]
	Name int ` + "`json:\"name\"`" + `
}

type Inner struct {
	I int ` + "`json:\"i\"`" + `
}

type Wrap[T any] struct {
	Inner
	V T ` + "`json:\"v\"`" + `
}

type EmbedsInside struct {
	*Other
	Wrap[int]
}
`
	box := map[string]any{"type": "object", "required": []string{"v", "name"}, "properties": map[string]any{"v": map[string]any{"type": "integer"}, "name": map[string]any{"type": "string"}}}
	described := map[string]map[string]any{
		"example.com/app/things.Box":  box,
		"example.com/app/things.Wrap": {"type": "object", "properties": map[string]any{"v": map[string]any{"type": "integer"}}},
	}
	kept := []struct {
		name   string
		values []any
		writes any
		ref    string
	}{
		{"PointerBeside", []any{&k1PointerBeside{}, &k1PointerBeside{K1Other: &K1Other{Q: 1}, K1Box: K1Box[int]{V: 2, Name: "n"}}}, map[string]any{"v": float64(0), "name": ""}, "things_Box"},
		{"TieBeside", []any{&k1TieBeside{}, &k1TieBeside{K1Box: K1Box[int]{V: 7, Name: "x"}}}, map[string]any{"v": float64(0), "name": ""}, "things_Box"},
		{"MarshalersBeside", []any{&k1MarshalersBeside{}}, map[string]any{"v": float64(0), "name": "", "A": float64(0), "B": float64(0)}, "things_Box"},
		{"AliasBeside", []any{&k1AliasBeside{}, &k1AliasBeside{K1Other: &K1Other{Q: 1}}}, map[string]any{"v": float64(0), "name": ""}, "things_BoxInt"},
		{"DeclaredBeside", []any{&k1DeclaredBeside{}, &k1DeclaredBeside{K1Other: &K1Other{Q: 1}}}, map[string]any{"v": float64(0), "name": ""}, "things_D"},
	}
	for _, c := range kept {
		if promotesMarshaler(c.values[0]) {
			t.Fatalf("%s: expected Go to promote no marshaler", c.name)
		}
		value := marshalled(t, c.values[0])
		if !reflect.DeepEqual(value, c.writes) {
			t.Fatalf("%s: expected encoding/json to write %v, got %v", c.name, c.writes, value)
		}

		builder := buildDescribed(t, source, described, c.name)
		err := builder.unresolvedError()
		if err != nil {
			t.Errorf("%s: expected the description kept, got %v", c.name, err)
			continue
		}
		schema := builder.schemas["things_"+c.name]
		allOf, _ := schema["allOf"].([]any)
		if !slices.ContainsFunc(allOf, func(member any) bool {
			return reflect.DeepEqual(member, map[string]any{"$ref": schemaRefPrefix + c.ref})
		}) {
			t.Errorf("%s: expected an allOf member referencing %s, got %v", c.name, c.ref, schema)
		}
		for _, value := range c.values {
			assertConforms(t, builder, schema, marshalled(t, value))
		}
		if len(nonConformities(builder.schemas, schema, map[string]any{"name": ""}, "$")) == 0 {
			t.Errorf("%s: expected the description's required v kept, got %v", c.name, schema)
		}
	}

	refused := []struct {
		name   string
		value  any
		writes any
		reason string
	}{
		{"UnderPointer", &k1UnderPointer{}, map[string]any{"x": float64(0)}, "type UnderPointer embeds Box[int], an instantiation of a generic type whose fields' types the generator does not instantiate, so it can only be the allOf member referencing its description, and that is not what encoding/json writes: it is embedded through a pointer, and a nil one leaves its fields out"},
		{"SharedOwn", &k1SharedOwn{}, map[string]any{"v": float64(0), "name": float64(0)}, `type SharedOwn embeds Box[int], an instantiation of a generic type whose fields' types the generator does not instantiate, so it can only be the allOf member referencing its description, and that is not what encoding/json writes: its field "name" shares its name with another field of the struct, so encoding/json writes that other field or neither`},
		{"EmbedsInside", &k1EmbedsInside{}, map[string]any{"i": float64(0), "v": float64(0)}, "type EmbedsInside embeds Wrap[int], an instantiation of a generic type whose fields' types the generator does not instantiate, so it can only be the allOf member referencing its description, and that is not what encoding/json writes: its declaration embeds Inner, whose promoted fields the generator does not resolve in a generic type"},
	}
	for _, c := range refused {
		value := marshalled(t, c.value)
		if !reflect.DeepEqual(value, c.writes) {
			t.Fatalf("%s: expected encoding/json to write %v, got %v", c.name, c.writes, value)
		}

		builder := buildDescribed(t, source, described, c.name)
		err := builder.unresolvedError()
		if err == nil || !strings.Contains(err.Error(), c.reason) {
			t.Errorf("%s: expected %q, got %v (schemas %v)", c.name, c.reason, err, builder.schemas)
		}
	}
}

// A standard-library package whose files cannot all be read fails generation with the file and
// the cause, field or embedded: a file that does not parse, one that cannot be opened and one
// whose build constraints cannot be evaluated. Leaving such a file out would change a type's
// schema, as the one here holds Month's MarshalText. A type declared only for other platforms is
// not declared, and the reason says files are selected for the platform the generator runs on.
func TestUnreadableGOROOTFilesFailGeneration(t *testing.T) {
	month := "package time\n\ntype Month int\n"
	text := "package time\n\nfunc (m Month) MarshalText() ([]byte, error) { return []byte(\"m\"), nil }\n"
	other := "//go:build plan9\n\npackage time\n\ntype Plan9Only int\n"
	source := `package things

import "time"

type Field struct {
	M time.Month ` + "`json:\"m\"`" + `
}

type Embedded struct {
	time.Month
	X int ` + "`json:\"x\"`" + `
}

type Other struct {
	P time.Plan9Only ` + "`json:\"p\"`" + `
}
`
	build := func(t *testing.T, files map[string]string, name string, prepare func(dir string)) *schemaBuilder {
		t.Helper()

		goroot := t.TempDir()
		dir := filepath.Join(goroot, "src", "time")
		err := os.MkdirAll(dir, 0o755)
		if err != nil {
			t.Fatal(err)
		}
		for file, content := range files {
			err = os.WriteFile(filepath.Join(dir, file), []byte(content), 0o600)
			if err != nil {
				t.Fatal(err)
			}
		}
		if prepare != nil {
			prepare(dir)
		}
		repoRoot := t.TempDir()
		writeThings(t, repoRoot, source)
		builder := newTestSchemaBuilder(t, repoRoot)
		builder.goroot = goroot
		builder.schemaRef(typeRef{ImportPath: "example.com/app/things", Name: name})

		return builder
	}

	builder := build(t, map[string]string{"month.go": month, "text.go": text, "other.go": other}, "Field", nil)
	err := builder.unresolvedError()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(builder.schemas["time_Month"], map[string]any{"type": "string"}) {
		t.Fatalf("expected the stand-in Month a string, got %v", builder.schemas["time_Month"])
	}

	badBuild := "//go:build (linux\n\n" + text
	cases := []struct {
		name    string
		files   map[string]string
		prepare func(dir string)
		reason  string
	}{
		{"parse", map[string]string{"month.go": month, "text.go": text, "bad.go": "package time\n\nfunc (\n"}, nil, "cannot parse "},
		{"build line", map[string]string{"month.go": month, "text.go": badBuild}, nil, "cannot evaluate the build constraints of "},
		{"unreadable", map[string]string{"month.go": month, "text.go": text}, func(dir string) {
			err := os.Chmod(filepath.Join(dir, "text.go"), 0)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				os.Chmod(filepath.Join(dir, "text.go"), 0o600)
			})
		}, "cannot evaluate the build constraints of "},
	}
	for _, c := range cases {
		for _, name := range []string{"Field", "Embedded"} {
			var dir string
			builder := build(t, c.files, name, func(d string) {
				dir = d
				if c.prepare != nil {
					c.prepare(d)
				}
			})
			if c.name == "unreadable" {
				_, err := os.ReadFile(filepath.Join(dir, "text.go"))
				if err == nil {
					t.Log("text.go stays readable to this user; skipping the unreadable case")
					continue
				}
			}
			err := builder.unresolvedError()
			file := "text.go"
			if c.name == "parse" {
				file = "bad.go"
			}
			if err == nil || !strings.Contains(err.Error(), c.reason+filepath.Join(dir, file)+":") || strings.Contains(err.Error(), "An internal error has occurred") {
				t.Errorf("%s, %s: expected %q naming %s, got %v (schemas %v)", c.name, name, c.reason, file, err, builder.schemas)
			}
		}
	}

	builder = build(t, map[string]string{"month.go": month, "text.go": text, "other.go": other}, "Other", nil)
	err = builder.unresolvedError()
	if err == nil || !strings.Contains(err.Error(), "time.Plan9Only: not declared in GOROOT's source at ") || !strings.Contains(err.Error(), "whose files the generator selects by their build constraints for ") || !strings.Contains(err.Error(), ", the platform it runs on") {
		t.Errorf("expected the platform named, got %v", err)
	}
}

type K4Level uint8

type K4Byte = byte

type K4Levels []K4Level

type K4Text uint8

func (K4Text) MarshalText() ([]byte, error) { return []byte("t"), nil }

type K4PointerText uint8

func (*K4PointerText) MarshalText() ([]byte, error) { return []byte("p"), nil }

type K4JSON uint8

func (K4JSON) MarshalJSON() ([]byte, error) { return []byte("7"), nil }

type K4PointerJSON uint8

func (*K4PointerJSON) MarshalJSON() ([]byte, error) { return []byte("8"), nil }

type K4FromText K4Text

type k4Slices struct {
	A []K4Level            `json:"a"`
	B []K4Byte             `json:"b"`
	C K4Levels             `json:"c"`
	D *[]K4Level           `json:"d"`
	E map[string][]K4Level `json:"e"`
	F []tls.AlertError     `json:"f"`
	G []elf.Class          `json:"g"`
	H []K4Text             `json:"h"`
	I []K4PointerText      `json:"i"`
	J []K4JSON             `json:"j"`
	K []K4PointerJSON      `json:"k"`
	L []K4FromText         `json:"l"`
	M [2]K4Level           `json:"m"`
	N []*K4Level           `json:"n"`
	O [][]K4Level          `json:"o"`
}

// encoding/json writes a slice whose element is of kind uint8 as one base64 string, whatever the
// element's name, alias or package, unless a pointer to the element has MarshalJSON or
// MarshalText, a pointer receiver included, when it is an array of what the method writes. A type
// declared from such a type does not get its methods and is base64 again. Arrays and slices of
// pointers stay arrays.
func TestNamedByteSlicesAreBase64(t *testing.T) {
	builder := buildThings(t, `package things

import (
	"crypto/tls"
	"debug/elf"
)

type Level uint8

type Byte = byte

type Levels []Level

type Text uint8

func (Text) MarshalText() ([]byte, error) { return []byte("t"), nil }

type PointerText uint8

func (*PointerText) MarshalText() ([]byte, error) { return []byte("p"), nil }

type JSON uint8

func (JSON) MarshalJSON() ([]byte, error) { return []byte("7"), nil }

type PointerJSON uint8

func (*PointerJSON) MarshalJSON() ([]byte, error) { return []byte("8"), nil }

type FromText Text

type Slices struct {
	A []Level            `+"`json:\"a\"`"+`
	B []Byte             `+"`json:\"b\"`"+`
	C Levels             `+"`json:\"c\"`"+`
	D *[]Level           `+"`json:\"d\"`"+`
	E map[string][]Level `+"`json:\"e\"`"+`
	F []tls.AlertError   `+"`json:\"f\"`"+`
	G []elf.Class        `+"`json:\"g\"`"+`
	H []Text             `+"`json:\"h\"`"+`
	I []PointerText      `+"`json:\"i\"`"+`
	J []JSON             `+"`json:\"j\"`"+`
	K []PointerJSON      `+"`json:\"k\"`"+`
	L []FromText         `+"`json:\"l\"`"+`
	M [2]Level           `+"`json:\"m\"`"+`
	N []*Level           `+"`json:\"n\"`"+`
	O [][]Level          `+"`json:\"o\"`"+`
}
`, "Slices")
	err := builder.unresolvedError()
	if err != nil {
		t.Fatal(err)
	}

	for _, value := range []any{[]K4PointerText{1}, []K4PointerJSON{1}} {
		elem := reflect.TypeOf(value).Elem()
		if elem.Kind() != reflect.Uint8 || (!reflect.PointerTo(elem).Implements(textMarshalerType) && !reflect.PointerTo(elem).Implements(jsonMarshalerType)) {
			t.Fatalf("expected %v's pointer to marshal", elem)
		}
	}
	level := K4Level(1)
	full := marshalled(t, &k4Slices{
		A: []K4Level{1, 2}, B: []K4Byte{1}, C: K4Levels{3}, D: &[]K4Level{4}, E: map[string][]K4Level{"k": {5}},
		F: []tls.AlertError{1}, G: []elf.Class{1}, H: []K4Text{1}, I: []K4PointerText{1}, J: []K4JSON{1},
		K: []K4PointerJSON{1}, L: []K4FromText{1}, M: [2]K4Level{1, 2}, N: []*K4Level{&level}, O: [][]K4Level{{1}},
	})
	want := map[string]any{
		"a": "AQI=", "b": "AQ==", "c": "Aw==", "d": "BA==", "e": map[string]any{"k": "BQ=="}, "f": "AQ==", "g": "AQ==",
		"h": []any{"t"}, "i": []any{"p"}, "j": []any{float64(7)}, "k": []any{float64(8)}, "l": "AQ==",
		"m": []any{float64(1), float64(2)}, "n": []any{float64(1)}, "o": []any{"AQ=="},
	}
	if !reflect.DeepEqual(full, want) {
		t.Fatalf("expected encoding/json to write %v, got %v", want, full)
	}
	schema := builder.schemas["things_Slices"]
	assertConforms(t, builder, schema, full)
	assertConforms(t, builder, schema, marshalled(t, &k4Slices{}))

	properties, _ := schema["properties"].(map[string]any)
	for name, value := range want {
		_, isString := value.(string)
		if !isString {
			continue
		}
		property, _ := properties[name].(map[string]any)
		if len(nonConformities(builder.schemas, property, []any{float64(1)}, "$."+name)) == 0 {
			t.Errorf("%s: expected an array of numbers refused, got %v", name, property)
		}
	}
}
