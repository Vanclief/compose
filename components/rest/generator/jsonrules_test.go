package generator

import (
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

type AppendLevel int

func (AppendLevel) AppendText(b []byte) ([]byte, error) { return append(b, "high"...), nil }

type appendPoint struct {
	X int
	Y int
}

func (p *appendPoint) AppendText(b []byte) ([]byte, error) {
	return append(b, strconv.Itoa(p.X)+","+strconv.Itoa(p.Y)...), nil
}

type appendThing struct {
	Level AppendLevel `json:"level"`
	Point appendPoint `json:"point"`
}

type appendStamped struct {
	AppendLevel
	Note string `json:"note"`
}

type AppendAddr struct {
	A int
}

func (AppendAddr) AppendText(b []byte) ([]byte, error) { return append(b, "addr"...), nil }

type appendOwn struct {
	AppendAddr
	Note string `json:"note"`
}

func (o appendOwn) MarshalText() ([]byte, error) { return []byte("own"), nil }

const appendSource = `package things

import "strconv"

type Level int

func (Level) AppendText(b []byte) ([]byte, error) { return append(b, "high"...), nil }

type Point struct {
	X int
	Y int
}

func (p *Point) AppendText(b []byte) ([]byte, error) {
	return append(b, strconv.Itoa(p.X)+","+strconv.Itoa(p.Y)...), nil
}

type Thing struct {
	Level Level ` + "`json:\"level\"`" + `
	Point Point ` + "`json:\"point\"`" + `
}

type Stamped struct {
	Level
	Note string ` + "`json:\"note\"`" + `
}

type Addr struct {
	A int
}

func (Addr) AppendText(b []byte) ([]byte, error) { return append(b, "addr"...), nil }

type Own struct {
	Addr
	Note string ` + "`json:\"note\"`" + `
}

func (o Own) MarshalText() ([]byte, error) { return []byte("own"), nil }
`

// A type with AppendText is written by its kind by encoding/json's own implementation, and as
// the string AppendText returns on encoding/json/v2, which prefers AppendText to MarshalText: a
// struct with its own MarshalText that gets an AppendText is written with the AppendText.
func TestAppendTextFollowsTheToolchain(t *testing.T) {
	thing := marshalled(t, &appendThing{Level: 2, Point: appendPoint{X: 1, Y: 2}})
	stamped := marshalled(t, &appendStamped{AppendLevel: 2, Note: "x"})
	own := marshalled(t, &appendOwn{Note: "x"})
	want := []any{
		map[string]any{"level": float64(2), "point": map[string]any{"X": float64(1), "Y": float64(2)}},
		map[string]any{"AppendLevel": float64(2), "note": "x"},
		"own",
	}
	if encodingJSONV2 {
		want = []any{map[string]any{"level": "high", "point": "1,2"}, "high", "addr"}
	}
	if !reflect.DeepEqual([]any{thing, stamped, own}, want) {
		t.Fatalf("expected encoding/json to write %v, got %v", want, []any{thing, stamped, own})
	}

	builder := buildThings(t, appendSource, "Thing")
	err := builder.unresolvedError()
	if err != nil {
		t.Fatal(err)
	}
	assertConforms(t, builder, builder.schemas["things_Thing"], thing)
	level := map[string]any{"type": "integer", "format": "int64"}
	if encodingJSONV2 {
		level = map[string]any{"type": "string"}
	}
	if !reflect.DeepEqual(builder.schemas["things_Level"], level) {
		t.Errorf("expected Level %v, got %v", level, builder.schemas["things_Level"])
	}

	for _, name := range []string{"Stamped", "Own"} {
		builder := buildThings(t, appendSource, name)
		err := builder.unresolvedError()
		if !encodingJSONV2 {
			if err != nil {
				t.Errorf("%s: expected its schema, got %v", name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), "defines AppendText, so it marshals as") {
			t.Errorf("%s: expected the promoted AppendText reported, got %v", name, err)
		}
	}
	stampedSchema := buildThings(t, appendSource, "Stamped")
	if !encodingJSONV2 {
		assertConforms(t, stampedSchema, stampedSchema.schemas["things_Stamped"], map[string]any{"Level": float64(2), "note": "x"})
	}
	ownSchema := buildThings(t, appendSource, "Own")
	if !encodingJSONV2 && !reflect.DeepEqual(ownSchema.schemas["things_Own"], map[string]any{"type": "string"}) {
		t.Errorf("expected Own a string, got %v", ownSchema.schemas["things_Own"])
	}
}

const marshalJSONToSource = `package things

import "encoding/json/jsontext"

type Raw struct {
	A int
}

func (Raw) MarshalJSONTo(enc *jsontext.Encoder) error {
	return enc.WriteToken(jsontext.String("raw"))
}

type Thing struct {
	Raw Raw ` + "`json:\"raw\"`" + `
}

type Holder struct {
	Raw
	N int ` + "`json:\"n\"`" + `
}
`

// A struct with MarshalJSONTo is written as its fields by encoding/json's own implementation,
// which does not know the method, and as what the method writes on encoding/json/v2, which the
// generator cannot read, so it refuses the struct and one that gets the method.
func TestMarshalJSONToFollowsTheToolchain(t *testing.T) {
	cases := []struct {
		name   string
		reason string
	}{
		{"Thing", "type Raw defines MarshalJSONTo; describe it with Config.TypeSchemas"},
		{"Holder", "type Holder embeds Raw, which defines MarshalJSONTo, so it marshals as Raw alone"},
	}
	for _, c := range cases {
		builder := buildThings(t, marshalJSONToSource, c.name)
		err := builder.unresolvedError()
		if !encodingJSONV2 {
			if err != nil {
				t.Errorf("%s: expected its fields, got %v", c.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.reason) {
			t.Errorf("%s: expected %q, got %v", c.name, c.reason, err)
		}
	}

	builder := buildThings(t, marshalJSONToSource, "Holder")
	if !encodingJSONV2 {
		assertConforms(t, builder, builder.schemas["things_Holder"], map[string]any{"A": float64(1), "n": float64(2)})
	}
}

// tagNames are json tag names, written after `json:` as Go quotes them, that encoding/json's own
// implementation and encoding/json/v2 read differently, or alike.
var tagNames = []string{
	`it's`, `a\b`, `ab\`, `\ab`, `b"c`, `d-e'x`, `9a'x`, `_x'y`, `'quoted'`, `f'g'`,
	`a b`, "\U0001F600", `é`, `日本`, "a\tb", `<>`, `a&b`, `h,i`, `a-b.c`, `x y z`, `~`, `-,`, `x²'y`,
}

// The key of a field whose json tag names it is the key encoding/json writes for that tag, on
// the toolchain the test runs on: a name encoding/json's own implementation does not take, such
// as it's, is replaced by the Go field name, and encoding/json/v2 takes it up to its quote, "it".
func TestJSONTagNamesFollowTheToolchain(t *testing.T) {
	source := "package things\n\ntype Thing struct {\n"
	for i, name := range tagNames {
		source += "\tF" + strconv.Itoa(i) + " int `json:" + strconv.Quote(name) + "`\n"
	}
	source += "}\n"
	builder := buildThings(t, source, "Thing")
	err := builder.unresolvedError()
	if err != nil {
		t.Fatal(err)
	}
	properties := componentProperties(t, builder, "things_Thing")

	fields := make([]reflect.StructField, 0, len(tagNames))
	for i, name := range tagNames {
		fields = append(fields, reflect.StructField{Name: "F" + strconv.Itoa(i), Type: reflect.TypeFor[int](), Tag: reflect.StructTag("json:" + strconv.Quote(name))})
	}
	written := marshalled(t, reflect.New(reflect.StructOf(fields)).Interface()).(map[string]any)
	keys := make([]string, 0, len(written))
	for key := range written {
		keys = append(keys, key)
	}
	got := make([]string, 0, len(properties))
	for key := range properties {
		got = append(got, key)
	}
	slices.Sort(keys)
	slices.Sort(got)
	if !slices.Equal(got, keys) {
		t.Fatalf("expected the keys encoding/json writes, %q, got %q", keys, got)
	}

	// The names the two implementations read apart, as each reads them.
	want := map[string]string{`it's`: "F0", `a\b`: "F1", `9a'x`: "F6", "\U0001F600": "F11", "a\tb": "F14", `x²'y`: "F22"}
	if encodingJSONV2 {
		// A name cut at its quote keeps its leading letters, numbers and underscores, ² among them,
		// and falls back to the Go name when it starts with a number.
		want = map[string]string{`it's`: "it", `a\b`: "a", `9a'x`: "F6", "\U0001F600": "\U0001F600", "a\tb": "a\tb", `x²'y`: "x²"}
	}
	for name, key := range want {
		if properties[key] == nil {
			t.Errorf("%q: expected the key %q, got %v", name, key, got)
		}
	}
}

type ownTextInner struct{}

func (ownTextInner) MarshalText() ([]byte, error) {
	return []byte("inner"), nil
}

// ownTextOuter declares AppendText and gets MarshalText from ownTextInner.
type ownTextOuter struct {
	ownTextInner
	A int
}

func (ownTextOuter) AppendText(b []byte) ([]byte, error) {
	return append(b, "own"...), nil
}

// encoding/json calls the first marshal method a struct has, so a promoted one ranking below a
// method the struct declares does not count: on encoding/json/v2 a struct with its own AppendText
// is written by it, whatever MarshalText it gets, and the generator describes it as a string; on
// encoding/json's own implementation, which does not call AppendText, it is written as the
// promoted MarshalText's text, and the generator refuses it.
func TestOwnAppendTextOutranksAPromotedMarshalText(t *testing.T) {
	source := `package things

type Inner struct{}

func (Inner) MarshalText() ([]byte, error) {
	return []byte("inner"), nil
}

type Outer struct {
	Inner
	A int
}

func (Outer) AppendText(b []byte) ([]byte, error) {
	return append(b, "own"...), nil
}
`
	written := marshalled(t, ownTextOuter{})
	builder := buildThings(t, source, "Outer")
	err := builder.unresolvedError()
	if written == "own" {
		if err != nil {
			t.Fatalf("expected Outer described, got %v", err)
		}
		if builder.schemas["things_Outer"]["type"] != "string" {
			t.Errorf("expected Outer to be a string, got %v", builder.schemas["things_Outer"])
		}
		return
	}
	if written != "inner" {
		t.Fatalf("expected encoding/json to write own or inner, got %v", written)
	}
	if err == nil || !strings.Contains(err.Error(), "embeds Inner, which defines MarshalText") {
		t.Errorf("expected Outer refused for the promoted MarshalText, got %v", err)
	}
}

// wrongAppender lists an AppendText encoding/json does not call.
type wrongAppender interface {
	AppendText() string
}

type rightAppender interface {
	AppendText(b []byte) ([]byte, error)
}

type appenderValue struct{}

func (appenderValue) AppendText(b []byte) ([]byte, error) {
	return append(b, "right"...), nil
}

type wrongAppenderHolder struct {
	wrongAppender `json:"-"`
	Note          string `json:"note"`
}

type rightAppenderHolder struct {
	rightAppender `json:"-"`
	Note          string `json:"note"`
}

// An embedded interface's MarshalJSONTo or AppendText counts only with the signature encoding/json
// calls: a struct embedding one with another signature is written as its fields.
func TestEmbeddedInterfaceCodersNeedTheirSignature(t *testing.T) {
	source := `package things

type WrongAppender interface {
	AppendText() string
}

type RightAppender interface {
	AppendText(b []byte) ([]byte, error)
}

type WrongHolder struct {
	WrongAppender ` + "`json:\"-\"`" + `
	Note          string ` + "`json:\"note\"`" + `
}

type RightHolder struct {
	RightAppender ` + "`json:\"-\"`" + `
	Note          string ` + "`json:\"note\"`" + `
}
`
	dotSource := `package things

import . "strings"

type DotAppender interface {
	AppendText(b Builder) ([]byte, error)
}

type DotHolder struct {
	DotAppender ` + "`json:\"-\"`" + `
	Note        string ` + "`json:\"note\"`" + `
}
`
	if !reflect.DeepEqual(marshalled(t, wrongAppenderHolder{}), map[string]any{"note": ""}) {
		t.Fatalf("expected encoding/json to write WrongHolder's fields, got %v", marshalled(t, wrongAppenderHolder{}))
	}
	builder := buildThings(t, source, "WrongHolder")
	err := builder.unresolvedError()
	if err != nil {
		t.Fatalf("expected WrongHolder described, got %v", err)
	}
	if componentProperties(t, builder, "things_WrongHolder")["note"] == nil {
		t.Errorf("expected WrongHolder's note, got %v", builder.schemas["things_WrongHolder"])
	}

	// encoding/json/v2 writes RightHolder with the AppendText it gets; the other implementation
	// writes its fields.
	written := marshalled(t, rightAppenderHolder{rightAppender: appenderValue{}})
	builder = buildThings(t, source, "RightHolder")
	err = builder.unresolvedError()
	// An interface whose AppendText the generator cannot read leaves the struct refused where
	// encoding/json may call the method, on encoding/json/v2.
	dot := buildThings(t, dotSource, "DotHolder").unresolvedError()
	if written == "right" {
		if err == nil || !strings.Contains(err.Error(), "embeds RightAppender, which defines AppendText") {
			t.Errorf("expected RightHolder refused, got %v", err)
		}
		if dot == nil || !strings.Contains(dot.Error(), "the signature of its method AppendText names Builder, which a dot import may declare") {
			t.Errorf("expected DotHolder refused, got %v", dot)
		}
		return
	}
	if err != nil || dot != nil {
		t.Errorf("expected RightHolder and DotHolder described, got %v and %v", err, dot)
	}
}

// Two packages whose AppendText signatures name each other's aliases: the generator reads a
// signature within its own package, so it loads neither from the other and does not recurse. A
// type of another package is one it cannot read, which fails generation where encoding/json may
// call AppendText, on encoding/json/v2, and does not matter elsewhere.
func TestCoderSignaturesDoNotLoadOtherPackages(t *testing.T) {
	files := map[string]string{
		"cyc/a/a_windows.go": "package a\n\nimport \"example.com/app/cyc/b\"\n\ntype T int\n\nfunc (T) AppendText(x b.B) ([]byte, error) { return x, nil }\n",
		"cyc/a/doc.go":       "package a\n\ntype A = []byte\n",
		"cyc/b/b_linux.go":   "package b\n\nimport \"example.com/app/cyc/a\"\n\ntype U int\n\nfunc (U) AppendText(x a.A) ([]byte, error) { return x, nil }\n",
		"cyc/b/doc.go":       "package b\n\ntype B = []byte\n",
	}
	builder := newTestSchemaBuilder(t, writeFixture(t, files))
	builder.schemaRef(typeRef{ImportPath: "example.com/app/cyc/a", Name: "T"})
	builder.schemaRef(typeRef{ImportPath: "example.com/app/cyc/b", Name: "U"})
	err := builder.unresolvedError()
	if !slices.Contains(marshalMethods(), "AppendText") {
		if err != nil {
			t.Fatalf("expected T and U described, got %v", err)
		}
		return
	}
	for _, refusal := range []string{"cyc/a.T: the signature of method AppendText of T in a_windows.go names b.B, a type of another package", "cyc/b.U: the signature of method AppendText of U in b_linux.go names a.A, a type of another package"} {
		if err == nil || !strings.Contains(err.Error(), refusal) {
			t.Errorf("expected %q, got %v", refusal, err)
		}
	}
}

type aliasReceived struct{ X int }

type aliasReceivedPointer = *aliasReceived

func (aliasReceivedPointer) MarshalText() ([]byte, error) { return []byte("x"), nil }

// A method declared on an alias of a pointer, `type PU = *U`, is (*U)'s, as one with a
// parenthesised receiver, `func (p (P))`, is P's: encoding/json writes U and P with MarshalText.
// gofmt drops the parentheses, so only the generator's fixture has them.
func TestMethodsOfPointerAliasAndParenthesisedReceivers(t *testing.T) {
	written := marshalled(t, &aliasReceived{X: 1})
	if written != "x" {
		t.Fatalf("expected encoding/json to write the method's text, got %v", written)
	}
	source := `package things

type U struct{ X int }

type PU = *U

func (PU) MarshalText() ([]byte, error) { return []byte("x"), nil }

type P struct{ X int }

func (p (P)) MarshalText() ([]byte, error) { return []byte("x"), nil }

type Holder struct {
	U *U ` + "`json:\"u\"`" + `
	P P  ` + "`json:\"p\"`" + `
}
`
	builder := buildThings(t, source, "Holder")
	err := builder.unresolvedError()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"U", "P"} {
		if builder.schemas["things_"+name]["type"] != "string" {
			t.Errorf("expected %s to be a string, got %v", name, builder.schemas["things_"+name])
		}
	}
}

type zeroTags []string

func (zeroTags) IsZero(strict bool) bool { return false }

type zeroGood []string

func (zeroGood) IsZero() bool { return false }

type zeroHolder struct {
	T zeroTags `json:"t,omitzero"`
	G zeroGood `json:"g,omitzero"`
}

// encoding/json calls IsZero for omitzero only as IsZero() bool: with another signature a nil
// slice is left out, so its field is not nullable, and a platform-limited one is not refused.
func TestIsZeroNeedsItsSignature(t *testing.T) {
	written := marshalled(t, zeroHolder{})
	if !reflect.DeepEqual(written, map[string]any{"g": nil}) {
		t.Fatalf("expected encoding/json to write only g, got %v", written)
	}
	files := map[string]string{
		"things.go":         "package things\n\ntype Tags []string\n\nfunc (Tags) IsZero(strict bool) bool { return false }\n\ntype Good []string\n\nfunc (Good) IsZero() bool { return false }\n\ntype Platform []string\n\ntype Holder struct {\n\tT Tags `json:\"t,omitzero\"`\n\tG Good `json:\"g,omitzero\"`\n\tP Platform `json:\"p,omitzero\"`\n}\n",
		"platform_linux.go": "package things\n\nfunc (Platform) IsZero(strict bool) bool { return false }\n",
	}
	builder := buildThingsFiles(t, files, nil, "Holder")
	err := builder.unresolvedError()
	if err != nil {
		t.Fatal(err)
	}
	properties := builder.schemas["things_Holder"]["properties"].(map[string]any)
	for name, nullable := range map[string]bool{"t": false, "g": true, "p": false} {
		if (properties[name].(map[string]any)["nullable"] == true) != nullable {
			t.Errorf("expected %s nullable %v, got %v", name, nullable, properties[name])
		}
	}
}
