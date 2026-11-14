//go:build goexperiment.jsonv2 && go1.27

package generator

import (
	"encoding/json/jsontext"
	"io"
	"reflect"
	"strings"
	"testing"
)

type jsonToRaw struct {
	A int
}

func (jsonToRaw) MarshalJSONTo(enc *jsontext.Encoder) error {
	return enc.WriteToken(jsontext.String("raw"))
}

type jsonToHolder struct {
	jsonToRaw
	N int `json:"n"`
}

// encoding/json/v2 writes a value with MarshalJSONTo, as TestMarshalJSONToFollowsTheToolchain
// has the generator expect, also a struct that gets the method.
func TestMarshalJSONToIsWritten(t *testing.T) {
	type thing struct {
		Raw jsonToRaw `json:"raw"`
	}
	got := []any{marshalled(t, &thing{}), marshalled(t, &jsonToHolder{N: 2})}
	if got[0].(map[string]any)["raw"] != "raw" || got[1] != "raw" {
		t.Fatalf("expected encoding/json to write MarshalJSONTo's output, got %v", got)
	}
}

// The Go types below mirror the fixture's, to show what encoding/json writes for each signature.

type ownEncoder struct{}

type (
	encoderAlias        = jsontext.Encoder
	encoderPtr          = *jsontext.Encoder
	byteAlias           = []byte
	elemByte            = byte
	errorAlias          = error
	genericBytes[T any] = []byte
)

type coderStandard struct{ A int }

func (coderStandard) MarshalJSONTo(enc *jsontext.Encoder) error {
	return enc.WriteToken(jsontext.String("x"))
}

type coderAlias struct{ A int }

func (coderAlias) MarshalJSONTo(enc *encoderAlias) error {
	return enc.WriteToken(jsontext.String("x"))
}

type coderPointerAlias struct{ A int }

func (coderPointerAlias) MarshalJSONTo(enc encoderPtr) error {
	return enc.WriteToken(jsontext.String("x"))
}

type coderErrorAlias struct{ A int }

func (coderErrorAlias) MarshalJSONTo(enc *jsontext.Encoder) errorAlias {
	return enc.WriteToken(jsontext.String("x"))
}

type coderNamed struct{ A int }

func (coderNamed) MarshalJSONTo(enc *jsontext.Encoder) (err error) {
	return enc.WriteToken(jsontext.String("x"))
}

type coderPointerReceiver struct{ A int }

func (*coderPointerReceiver) MarshalJSONTo(enc *jsontext.Encoder) error {
	return enc.WriteToken(jsontext.String("x"))
}

type coderValue struct{ A int }

func (coderValue) MarshalJSONTo(jsontext.Encoder) error { return nil }

type coderWriter struct{ A int }

func (coderWriter) MarshalJSONTo(io.Writer) error { return nil }

type coderDefined struct{ A int }

func (coderDefined) MarshalJSONTo(*ownEncoder) error { return nil }

type gradeDefined int

func (gradeDefined) MarshalText() ([]byte, error) { return []byte("gold"), nil }

func (gradeDefined) MarshalJSONTo(*ownEncoder) error { return nil }

type appendOK int

func (appendOK) AppendText(b []byte) ([]byte, error) { return append(b, "x"...), nil }

type appendBytesAlias int

func (appendBytesAlias) AppendText(b byteAlias) (byteAlias, error) { return append(b, "x"...), nil }

type appendElemAlias int

func (appendElemAlias) AppendText(b []elemByte) ([]elemByte, error) { return append(b, "x"...), nil }

type appendErrorAlias int

func (appendErrorAlias) AppendText(b []byte) ([]byte, errorAlias) { return append(b, "x"...), nil }

type appendGenericReceiver[T any] int

func (appendGenericReceiver[T]) AppendText(b []byte) ([]byte, error) { return append(b, "x"...), nil }

type appendGenericAlias int

func (appendGenericAlias) AppendText(b genericBytes[int]) (genericBytes[int], error) {
	return append(b, "x"...), nil
}

type appendString int

func (appendString) AppendText(b string) ([]byte, error) { return []byte(b), nil }

type appendNoArg int

func (appendNoArg) AppendText() ([]byte, error) { return []byte("x"), nil }

type appendUint8 int

func (appendUint8) AppendText(b []uint8) ([]uint8, error) { return append(b, "x"...), nil }

type appendTwo int

func (appendTwo) AppendText(a, b []byte) ([]byte, error) { return a, nil }

type appendArray int

func (appendArray) AppendText(b [4]byte) ([4]byte, error) { return b, nil }

// ownEncoder names a type parameter as it names a type of the package.
type boxParam[ownEncoder any] struct{ V ownEncoder }

func (boxParam[ownEncoder]) AppendText(b []ownEncoder) ([]ownEncoder, error) { return b, nil }

type boxOuter struct {
	boxParam[byte]
	N int
}

type textInner struct{ A int }

func (textInner) AppendText(b []byte) ([]byte, error) { return append(b, "x"...), nil }

type textMid struct {
	textInner
	B int
}

func (textMid) AppendText(s string) string { return s }

type TextShadow interface{ AppendText(s string) string }

type textDeep struct{ textInner }

type textShadowed struct {
	TextShadow
	textDeep
}

type textWrong struct{ B int }

func (textWrong) AppendText(s string) string { return s }

type textTie struct {
	textInner
	textWrong
}

type textOuterMid struct {
	textMid
	C int
}

type TextCode int

func (TextCode) AppendText(b []byte) ([]byte, error) { return append(b, "x"...), nil }

type textMidCode struct {
	TextCode
	B int
}

func (textMidCode) AppendText(s string) string { return s }

type textOuterCode struct {
	textMidCode
	C int
}

type pointedText struct{ X int }

type pointedAlias = *pointedText

func (pointedAlias) AppendText(b []byte) ([]byte, error) { return append(b, "x"...), nil }

var coderFiles = map[string]string{
	"oldexp/encode.go": "package oldexp\n\ntype Encoder struct{}\n",
	"newexp/alias.go":  "package newexp\n\nimport \"encoding/json/jsontext\"\n\ntype Encoder = jsontext.Encoder\n",
	"other/other.go":   "package other\n\nimport \"encoding/json/jsontext\"\n\ntype EncPtr = *jsontext.Encoder\n",
	"things/dot.go": `package things

import . "encoding/json/jsontext"

type DotAlias = Encoder

type Dot struct{ A int }

func (Dot) MarshalJSONTo(e *Encoder) error { return nil }

type Target struct{ A int }

type AliasReceiver = Target

func (AliasReceiver) MarshalJSONTo(e *Encoder) error { return nil }
`,
	"shadow/shadow_windows.go": "package shadow\n\ntype byte = int\n",
	"shadow/level.go":          "package shadow\n\ntype Level int\n\nfunc (Level) AppendText(b []byte) ([]byte, error) { return b, nil }\n",
	"things/bytes_darwin.go":   "package things\n\ntype PlatformBytes = []byte\n",
	"things/bytes_other.go":    "//go:build !darwin\n\npackage things\n\ntype PlatformBytes = string\n",
	"things/things.go": `package things

import (
	"encoding/json/jsontext"
	jt "encoding/json/jsontext"
	"io"

	"example.com/app/newexp"
	"example.com/app/oldexp"
	"example.com/app/other"
	"example.com/dep/missing"
)

type (
	Enc             = jsontext.Encoder
	EncPtr          = *jsontext.Encoder
	Bytes           = []byte
	Byte            = byte
	Err             = error
	GBytes[T any]   = []byte
	OwnEncoder      struct{}
)

type Standard struct{ A int }

func (Standard) MarshalJSONTo(*jsontext.Encoder) error { return nil }

type Renamed struct{ A int }

func (Renamed) MarshalJSONTo(*jt.Encoder) error { return nil }

type Local struct{ A int }

func (Local) MarshalJSONTo(*Enc) error { return nil }

type LocalPointer struct{ A int }

func (LocalPointer) MarshalJSONTo(EncPtr) error { return nil }

type ErrorJSON struct{ A int }

func (ErrorJSON) MarshalJSONTo(*jsontext.Encoder) Err { return nil }

type Named struct{ A int }

func (Named) MarshalJSONTo(enc *jsontext.Encoder) (err error) { return nil }

type PointerReceiver struct{ A int }

func (*PointerReceiver) MarshalJSONTo(*jsontext.Encoder) error { return nil }

type Value struct{ A int }

func (Value) MarshalJSONTo(jsontext.Encoder) error { return nil }

type Writer struct{ A int }

func (Writer) MarshalJSONTo(io.Writer) error { return nil }

type Defined struct{ A int }

func (Defined) MarshalJSONTo(*OwnEncoder) error { return nil }

type Grade int

func (Grade) MarshalText() ([]byte, error) { return []byte("gold"), nil }

func (Grade) MarshalJSONTo(*OwnEncoder) error { return nil }

type Own struct{ A int }

func (Own) MarshalJSONTo(*oldexp.Encoder) error { return nil }

type Experiment struct{ A int }

func (Experiment) MarshalJSONTo(*newexp.Encoder) error { return nil }

type PointerAlias struct{ A int }

func (PointerAlias) MarshalJSONTo(other.EncPtr) error { return nil }

type Missing struct{ A int }

func (Missing) MarshalJSONTo(*missing.Encoder) error { return nil }

type DotElsewhere struct{ A int }

func (DotElsewhere) MarshalJSONTo(*DotAlias) error { return nil }

type Appends int

func (Appends) AppendText(b []byte) ([]byte, error) { return b, nil }

type BytesAlias int

func (BytesAlias) AppendText(b Bytes) (Bytes, error) { return b, nil }

type ElemAlias int

func (ElemAlias) AppendText(b []Byte) ([]Byte, error) { return b, nil }

type ErrorAlias int

func (ErrorAlias) AppendText(b []byte) ([]byte, Err) { return b, nil }

type NamedAppend int

func (NamedAppend) AppendText(b []byte) (out []byte, err error) { return b, nil }

type GenericReceiver[T any] int

func (GenericReceiver[T]) AppendText(b []byte) ([]byte, error) { return b, nil }

type GenericAlias int

func (GenericAlias) AppendText(b GBytes[int]) (GBytes[int], error) { return b, nil }

type String int

func (String) AppendText(b string) ([]byte, error) { return nil, nil }

type NoArg int

func (NoArg) AppendText() ([]byte, error) { return nil, nil }

type Uint8 int

func (Uint8) AppendText(b []uint8) ([]uint8, error) { return b, nil }

type Two int

func (Two) AppendText(a, b []byte) ([]byte, error) { return a, nil }

type Array int

func (Array) AppendText(b [4]byte) ([4]byte, error) { return b, nil }

type PlatformLevel int

func (PlatformLevel) AppendText(b PlatformBytes) (PlatformBytes, error) { return b, nil }

type E struct{}

type Box[E any] struct{ V E }

func (Box[E]) AppendText(b []E) ([]E, error) { return b, nil }

type BoxOuter struct {
	Box[byte]
	N int
}

type Appender[E any] interface{ AppendText(b []E) ([]E, error) }

type AppenderHolder struct {
	Appender[byte]
	N int
}

type Priced struct {
	PointerAlias
	Note string
}

type Inner struct{ A int }

func (Inner) AppendText(b []byte) ([]byte, error) { return b, nil }

type Mid struct {
	Inner
	B int
}

func (Mid) AppendText(s string) string { return s }

type Shadow interface{ AppendText(s string) string }

type Deep struct{ Inner }

type Shadowed struct {
	Shadow
	Deep
}

type Pointed struct{ X int }

type PointedAlias = *Pointed

func (PointedAlias) AppendText(b []byte) ([]byte, error) { return b, nil }

type Paren int

func (Paren) AppendText(b ([]byte)) ([]byte, error) { return b, nil }

type ParenEncoder struct{ A int }

func (ParenEncoder) MarshalJSONTo(e (*jt.Encoder)) error { return nil }

type WrongSig struct{ B int }

func (WrongSig) AppendText(s string) string { return s }

type Tie struct {
	Inner
	WrongSig
}

type OuterMid struct {
	Mid
	C int
}

type Code int

func (Code) AppendText(b []byte) ([]byte, error) { return b, nil }

type MidCode struct {
	Code
	B int
}

func (MidCode) AppendText(s string) string { return s }

type OuterCode struct {
	MidCode
	C int
}

type Holder struct {
	T Target ` + "`json:\"t\"`" + `
}
`,
}

// encoding/json calls MarshalJSONTo only as (*jsontext.Encoder) error and AppendText only as
// ([]byte) ([]byte, error). The generator reads the signature within the type's own package,
// following its aliases: it counts the method when every type matches, records it as not called
// when one is another type, where it still hides a method of its name promoted from deeper, and
// fails generation for the type when it cannot read one: a type of another package, which may be
// an alias of jsontext's Encoder, a generic alias, a type a dot import may declare, a type
// parameter or a type declared per platform, through which Go 1.27.2 may call the method too. A
// description of such a type is not one of a struct that embeds it.
func TestCodersNeedTheirSignature(t *testing.T) {
	written := map[string]any{
		"standard":        marshalled(t, coderStandard{A: 1}),
		"alias":           marshalled(t, coderAlias{A: 1}),
		"pointerAlias":    marshalled(t, coderPointerAlias{A: 1}),
		"errorAlias":      marshalled(t, coderErrorAlias{A: 1}),
		"named":           marshalled(t, coderNamed{A: 1}),
		"pointerReceiver": marshalled(t, &coderPointerReceiver{A: 1}),
		"value":           marshalled(t, coderValue{A: 1}),
		"writer":          marshalled(t, coderWriter{A: 1}),
		"defined":         marshalled(t, coderDefined{A: 1}),
		"grade":           marshalled(t, gradeDefined(1)),
		"append":          marshalled(t, appendOK(1)),
		"bytesAlias":      marshalled(t, appendBytesAlias(1)),
		"elemAlias":       marshalled(t, appendElemAlias(1)),
		"errorAppend":     marshalled(t, appendErrorAlias(1)),
		"genericReceiver": marshalled(t, appendGenericReceiver[int](1)),
		"genericAlias":    marshalled(t, appendGenericAlias(1)),
		"string":          marshalled(t, appendString(1)),
		"noArg":           marshalled(t, appendNoArg(1)),
		"uint8":           marshalled(t, appendUint8(1)),
		"two":             marshalled(t, appendTwo(1)),
		"array":           marshalled(t, appendArray(1)),
		"boxOuter":        marshalled(t, boxOuter{}),
		"mid":             marshalled(t, textMid{}),
		"shadowed":        marshalled(t, textShadowed{}),
		"tie":             marshalled(t, textTie{}),
		"outerMid":        marshalled(t, textOuterMid{}),
		"pointed":         marshalled(t, &pointedText{}),
		"outerCode":       marshalled(t, textOuterCode{}),
	}
	fields := map[string]any{"A": float64(1)}
	expected := map[string]any{
		"standard": "x", "alias": "x", "pointerAlias": "x", "errorAlias": "x", "named": "x", "pointerReceiver": "x",
		"value": fields, "writer": fields, "defined": fields, "grade": "gold",
		"append": "x", "bytesAlias": "x", "elemAlias": "x", "errorAppend": "x", "genericReceiver": "x", "genericAlias": "x",
		"string": float64(1), "noArg": float64(1), "uint8": "x", "two": float64(1), "array": float64(1),
		"boxOuter": "", "mid": map[string]any{"A": float64(0), "B": float64(0)}, "shadowed": map[string]any{"TextShadow": nil, "A": float64(0)},
		"tie": map[string]any{"A": float64(0), "B": float64(0)}, "outerMid": map[string]any{"A": float64(0), "B": float64(0), "C": float64(0)}, "pointed": "x",
		"outerCode": map[string]any{"TextCode": "x", "B": float64(0), "C": float64(0)},
	}
	if !reflect.DeepEqual(written, expected) {
		t.Fatalf("expected encoding/json to write %v, got %v", expected, written)
	}

	builder := newTestSchemaBuilder(t, writeFixture(t, coderFiles))
	pkg, err := builder.loadPackage("example.com/app/things")
	if err != nil {
		t.Fatal(err)
	}
	// counted is the method's entry in Methods: true for one called and one the generator cannot
	// read, false for one not called.
	cases := []struct {
		names      []string
		method     string
		counted    bool
		unreadable string
	}{
		{[]string{"Standard", "Renamed", "Local", "LocalPointer", "ErrorJSON", "Named", "PointerReceiver", "ParenEncoder"}, "MarshalJSONTo", true, ""},
		{[]string{"Appends", "BytesAlias", "ElemAlias", "ErrorAlias", "NamedAppend", "GenericReceiver", "Uint8", "Pointed", "Paren"}, "AppendText", true, ""},
		{[]string{"Value", "Writer", "Defined", "Grade"}, "MarshalJSONTo", false, ""},
		{[]string{"String", "NoArg", "Two", "Array", "Mid", "WrongSig"}, "AppendText", false, ""},
		{[]string{"Own"}, "MarshalJSONTo", true, "names oldexp.Encoder, a type of another package"},
		{[]string{"Experiment"}, "MarshalJSONTo", true, "names newexp.Encoder, a type of another package"},
		{[]string{"PointerAlias"}, "MarshalJSONTo", true, "names other.EncPtr, a type of another package"},
		{[]string{"Missing"}, "MarshalJSONTo", true, "names missing.Encoder, a type of another package"},
		{[]string{"Dot", "DotElsewhere", "AliasReceiver", "Target"}, "MarshalJSONTo", true, "names Encoder, which a dot import may declare"},
		{[]string{"GenericAlias"}, "AppendText", true, "names GBytes[int], a generic type"},
		{[]string{"PlatformLevel"}, "AppendText", true, "names PlatformBytes, which is declared in several files, so its meaning depends on the platform"},
		{[]string{"Box"}, "AppendText", true, "names E, a type parameter"},
	}
	for _, c := range cases {
		for _, name := range c.names {
			counted, declared := pkg.Methods[name][c.method]
			if !declared || counted != c.counted {
				t.Errorf("%s: expected %s declared and counted %v, got %v, %v", name, c.method, c.counted, declared, counted)
			}
			reason := pkg.RefusedMethods[name][c.method]
			if (c.unreadable == "") != (reason == "") || !strings.Contains(reason, c.unreadable) {
				t.Errorf("%s: expected the refusal %q, got %q", name, c.unreadable, reason)
			}
		}
	}

	// The package declares a byte of its own on windows, so on darwin Go calls Level's AppendText.
	shadow, err := builder.loadPackage("example.com/app/shadow")
	if err != nil {
		t.Fatal(err)
	}
	reason := shadow.RefusedMethods["Level"]["AppendText"]
	if !shadow.Methods["Level"]["AppendText"] || !strings.Contains(reason, "names byte, which the package declares as its own type") {
		t.Errorf("expected Level's AppendText refused, got %v, %q", shadow.Methods["Level"], reason)
	}

	for _, name := range []string{"Standard", "Grade", "NoArg", "Appends", "Holder", "PointerAlias", "Uint8", "Two", "Array", "Mid", "Shadowed", "AppenderHolder", "Tie", "OuterMid", "OuterCode", "Pointed", "Paren"} {
		builder.schemaRef(typeRef{ImportPath: "example.com/app/things", Name: name})
	}
	err = builder.unresolvedError()
	refusals := []string{
		"things.Standard: type Standard defines MarshalJSONTo",
		`things.Target: the signature of method MarshalJSONTo of AliasReceiver in dot.go names Encoder, which a dot import may declare, so the generator cannot tell whether encoding/json calls it; describe Target with Config.TypeSchemas["example.com/app/things.Target"]`,
		`things.PointerAlias: the signature of method MarshalJSONTo of PointerAlias in things.go names other.EncPtr, a type of another package, so the generator cannot tell whether encoding/json calls it`,
		"things.AppenderHolder: type AppenderHolder embeds Appender[byte], whose source the generator cannot read (the signature of its method AppendText names E, a type parameter)",
	}
	for _, refusal := range refusals {
		if err == nil || !strings.Contains(err.Error(), refusal) {
			t.Errorf("expected %q, got %v", refusal, err)
		}
	}
	kinds := map[string]string{"Grade": "string", "NoArg": "integer", "Appends": "string", "Uint8": "string", "Two": "integer", "Array": "integer", "Mid": "object", "Pointed": "string", "Paren": "string"}
	for name, kind := range kinds {
		if builder.schemas["things_"+name]["type"] != kind {
			t.Errorf("expected %s to be of type %s, got %v", name, kind, builder.schemas["things_"+name])
		}
	}
	// A method of another signature hides Inner's below it, and ties with it at its depth.
	for _, name := range []string{"Mid", "Shadowed", "Tie", "OuterMid", "OuterCode"} {
		if err != nil && strings.Contains(err.Error(), "things."+name+":") {
			t.Errorf("expected %s's schema, got %v", name, err)
		}
	}
	assertConforms(t, builder, builder.schemas["things_OuterMid"], written["outerMid"])
	// MidCode does not marshal itself, so it is an allOf member as any embedded struct.
	if builder.schemas["things_OuterCode"]["allOf"] == nil {
		t.Errorf("expected OuterCode to be allOf MidCode and its fields, got %v", builder.schemas["things_OuterCode"])
	}
	outerCode := written["outerCode"].(map[string]any)
	outerCode["Code"] = outerCode["TextCode"]
	delete(outerCode, "TextCode")
	assertConforms(t, builder, builder.schemas["things_OuterCode"], outerCode)
	for name, property := range map[string]string{"Mid": "B", "Shadowed": "Shadow", "Tie": "B"} {
		properties, _ := builder.schemas["things_"+name]["properties"].(map[string]any)
		if properties["A"] == nil || properties[property] == nil {
			t.Errorf("expected %s to have the properties A and %s, got %v", name, property, builder.schemas["things_"+name])
		}
	}

	described := map[string]map[string]any{
		"example.com/app/things.Target":       {"type": "string"},
		"example.com/app/things.PointerAlias": {"type": "string"},
		"example.com/app/things.Box":          {"type": "object"},
	}
	builder = newTestSchemaBuilder(t, writeFixture(t, coderFiles))
	builder.typeSchemas = described
	for _, name := range []string{"Holder", "PointerAlias", "Priced", "BoxOuter"} {
		builder.schemaRef(typeRef{ImportPath: "example.com/app/things", Name: name})
	}
	err = builder.unresolvedError()
	refusals = []string{
		`things.BoxOuter: type BoxOuter embeds Box, which defines AppendText, which encoding/json may call, so it may marshal as Box's text alone; Config.TypeSchemas describes Box, not the types that embed it; describe it with Config.TypeSchemas["example.com/app/things.BoxOuter"]`,
		`things.Priced: type Priced embeds PointerAlias, which defines MarshalJSONTo, which encoding/json may call, so it may marshal as PointerAlias alone; Config.TypeSchemas describes PointerAlias, not the types that embed it; describe it with Config.TypeSchemas["example.com/app/things.Priced"]`,
	}
	if err == nil || strings.Count(err.Error(), "\n") != len(refusals) {
		t.Errorf("expected the descriptions to settle all but the embedding structs, got %v", err)
	}
	for _, refusal := range refusals {
		if err == nil || !strings.Contains(err.Error(), refusal) {
			t.Errorf("expected %q, got %v", refusal, err)
		}
	}
}
