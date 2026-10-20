package fixture

import "github.com/vanclief/compose/primitives/enums"

// Body shapes whose encoding/json key set differs from huma's schema. Each
// must be rejected when reachable from a request root.

type base struct {
	Level int `json:"level" maximum:"5"`
}

// UnexportedEmbed embeds an unexported struct: huma skips it, encoding/json
// promotes Level.
type UnexportedEmbed struct {
	base
	Name string `json:"name"`
}

// HiddenField carries hidden:"true", which huma drops from the schema.
type HiddenField struct {
	Secret string `json:"secret" hidden:"true"`
}

// Tags is a non-struct type used embedded.
type Tags []string

// EmbeddedSlice embeds a non-struct type: encoding/json binds key "Tags".
type EmbeddedSlice struct {
	Tags
}

// Inner is embedded under ShadowedGoName.
type Inner struct {
	ID string `json:"a_id"`
}

// ShadowedGoName has a promoted field whose Go name is shadowed: huma drops
// a_id, encoding/json binds it.
type ShadowedGoName struct {
	ID string `json:"id"`
	Inner
}

// ConflictA and ConflictB declare the same JSON name under different Go names.
type ConflictA struct {
	X int `json:"x"`
}

// ConflictB declares "x" as Y. Embedding ConflictA and ConflictB side by side
// makes encoding/json drop "x" while huma publishes it; checks_test builds that
// struct with reflect.StructOf, since go vet rejects the repeated json tag in
// a declared type.
type ConflictB struct {
	Y int `json:"x" maximum:"5"`
}

// ShadowOuter: encoding/json binds Count, huma's schema describes ConflictB.Y.
type ShadowOuter struct {
	Count int `json:"x"`
	ConflictB
}

// NestedBad reaches UnexportedEmbed through a slice.
type NestedBad struct {
	Items []UnexportedEmbed `json:"items"`
}

// HiddenParam hides a query parameter.
type HiddenParam struct {
	Q string `json:"-" query:"q" hidden:"true"`
}

// NullableStruct puts nullable on a struct reference.
type NullableStruct struct {
	Meta Meta `json:"meta" nullable:"true"`
}

// WideBound sets a maximum an int8 cannot hold.
type WideBound struct {
	N int8 `json:"n" maximum:"1000"`
}

// HiddenResponse hides fields in a response, which stays allowed.
type HiddenResponse struct {
	Name   string `json:"name"`
	Secret *Mode  `json:"secret" hidden:"true"`
	Meta   *Meta  `json:"meta" hidden:"true"`
}

// Mode is a second enum used by the nullability fixtures.
type Mode string

var Modes = enums.Set([]Mode{"a", "b"})

// Flags is a response covering pointer enums, nullable enums and sized ints.
type Flags struct {
	Mode   Mode    `json:"mode"`
	PMode  *Mode   `json:"pmode"`
	OMode  *Mode   `json:"omode,omitempty"`
	NMode  Mode    `json:"nmode" nullable:"true"`
	PModes []*Mode `json:"pmodes"`
	Choice *string `json:"choice" enum:"x,y"`
	Small  int32   `json:"small"`
	Tiny   uint8   `json:"tiny" maximum:"100"`
	Big    uint32  `json:"big"`
	Count  uint    `json:"count"`
	Wide   int64   `json:"wide"`
}

// FlagsRequest covers the request direction of the same rules.
type FlagsRequest struct {
	Small    int32   `json:"-" query:"small"`
	Choice   *string `json:"choice" enum:"a,b"`
	Must     *int    `json:"must" required:"true"`
	MustMode *Mode   `json:"must_mode" required:"true"`
	PMode    *Mode   `json:"pmode,omitempty"`
	Levels   []int16 `json:"levels"`
}
