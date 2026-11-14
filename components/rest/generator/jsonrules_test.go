package generator

import (
	"reflect"
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
