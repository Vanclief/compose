package generator

import (
	"reflect"
	"testing"
)

type ZeroNever []int

func (ZeroNever) IsZero() bool { return false }

type ZeroNeverPointer []int

func (*ZeroNeverPointer) IsZero() bool { return false }

type ZeroNeverMap map[string]int

func (ZeroNeverMap) IsZero() bool { return false }

type ZeroEmpty []int

func (z ZeroEmpty) IsZero() bool { return len(z) == 0 }

type ZeroDeclared ZeroNever

type ZeroAlias = ZeroNever

type zeroThing struct {
	A ZeroNever        `json:"a,omitzero"`
	B ZeroNeverPointer `json:"b,omitzero"`
	C ZeroNeverMap     `json:"c,omitzero"`
	D ZeroNever        `json:"d,omitempty,omitzero"`
	E ZeroEmpty        `json:"e,omitzero"`
	F ZeroDeclared     `json:"f,omitzero"`
	G ZeroAlias        `json:"g,omitzero"`
	H []int            `json:"h,omitzero"`
	I ZeroNeverMap     `json:"i,omitempty,omitzero"`
}

// With omitzero alone, encoding/json leaves a field out when its type's IsZero, with a value or a
// pointer receiver, reports true, and compares with the zero value only for a type without one.
// A nil slice or map whose IsZero reports false is written as null, so its field stays nullable;
// omitempty leaves a nil one out whatever IsZero says, and a type declared from one with IsZero
// does not get the method.
func TestOmitZeroWithIsZeroIsNullable(t *testing.T) {
	builder := buildThings(t, `package things

type Never []int

func (Never) IsZero() bool { return false }

type NeverPointer []int

func (*NeverPointer) IsZero() bool { return false }

type NeverMap map[string]int

func (NeverMap) IsZero() bool { return false }

type Empty []int

func (z Empty) IsZero() bool { return len(z) == 0 }

type Declared Never

type Alias = Never

type Thing struct {
	A Never        `+"`json:\"a,omitzero\"`"+`
	B NeverPointer `+"`json:\"b,omitzero\"`"+`
	C NeverMap     `+"`json:\"c,omitzero\"`"+`
	D Never        `+"`json:\"d,omitempty,omitzero\"`"+`
	E Empty        `+"`json:\"e,omitzero\"`"+`
	F Declared     `+"`json:\"f,omitzero\"`"+`
	G Alias        `+"`json:\"g,omitzero\"`"+`
	H []int        `+"`json:\"h,omitzero\"`"+`
	I NeverMap     `+"`json:\"i,omitempty,omitzero\"`"+`
}
`, "Thing")
	err := builder.unresolvedError()
	if err != nil {
		t.Fatal(err)
	}

	empty := marshalled(t, &zeroThing{})
	wantEmpty := map[string]any{"a": nil, "b": nil, "c": nil, "g": nil}
	if !reflect.DeepEqual(empty, wantEmpty) {
		t.Fatalf("expected encoding/json to write %v, got %v", wantEmpty, empty)
	}
	assertConforms(t, builder, builder.schemas["things_Thing"], empty)
	assertConforms(t, builder, builder.schemas["things_Thing"], marshalled(t, &zeroThing{
		A: ZeroNever{1}, B: ZeroNeverPointer{2}, C: ZeroNeverMap{"c": 3}, D: ZeroNever{4}, E: ZeroEmpty{5},
		F: ZeroDeclared{6}, G: ZeroAlias{7}, H: []int{8}, I: ZeroNeverMap{"i": 9},
	}))

	nullable := map[string]bool{"a": true, "b": true, "c": true, "e": true, "g": true}
	for name, property := range componentProperties(t, builder, "things_Thing") {
		got := property.(map[string]any)["nullable"] == true
		if got != nullable[name] {
			t.Errorf("%s: expected nullable %v, got %v", name, nullable[name], property)
		}
	}
	if builder.schemas["things_Thing"]["required"] != nil {
		t.Errorf("expected no field required, got %v", builder.schemas["things_Thing"]["required"])
	}
}
