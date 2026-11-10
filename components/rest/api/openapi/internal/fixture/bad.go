package fixture

import (
	"mime/multipart"
	"time"

	"github.com/google/uuid"

	"github.com/vanclief/compose/types"
)

// BadTag has a typo in a constraint tag.
type BadTag struct {
	Count int `json:"count" minimun:"0"`
}

// EchoParam uses Echo's param tag instead of path.
type EchoParam struct {
	ID uuid.UUID `json:"-" param:"id"`
}

// MissingDash has a query param that would also be a body property.
type MissingDash struct {
	Status RunStatus `query:"status"`
}

// ContentEncodingProp has a property name huma's Downgrade renames.
type ContentEncodingProp struct {
	Encoding string `json:"contentEncoding"`
}

// StructParam has a query param of struct type.
type StructParam struct {
	Filter Meta `json:"-" query:"filter"`
}

// DuplicateParam declares the same header twice.
type DuplicateParam struct {
	A string `json:"-" header:"X-Trace"`
	B string `json:"-" header:"x-trace"`
}

// NestedQuery holds a query parameter: read only when a request type embeds
// it.
type NestedQuery struct {
	Q string `json:"-" query:"q"`
	B int    `json:"b"`
}

// NestedParam reaches NestedQuery through a named field.
type NestedParam struct {
	N NestedQuery `json:"n"`
}

// NestedParamSlice reaches NestedQuery through a slice element.
type NestedParamSlice struct {
	Items []NestedQuery `json:"items"`
}

// NestedParamOptional reaches NestedQuery through an Optional's element.
type NestedParamOptional struct {
	N types.Optional[NestedQuery] `json:"n"`
}

// EmbeddedAndNested embeds NestedQuery, so its q is a parameter, and also
// reaches it through a named field, where q is ignored.
type EmbeddedAndNested struct {
	NestedQuery
	Again NestedQuery `json:"again"`
}

// NestedFileHolder holds a file field: read only when a request type
// embeds it.
type NestedFileHolder struct {
	File *multipart.FileHeader `json:"-" form:"file"`
}

// NestedFile reaches NestedFileHolder through a named field.
type NestedFile struct {
	Upload NestedFileHolder `json:"upload"`
}

// BadValue has a constraint huma cannot parse (huma panics, we return an error).
type BadValue struct {
	N int `json:"n" minimum:"abc"`
}

// FormatOnTime sets format date on a time.Time, which encoding/json reads
// only as RFC 3339.
type FormatOnTime struct {
	Day time.Time `json:"day" format:"date"`
}

// FormatOnInt sets a format on an integer, whose format the type sets.
type FormatOnInt struct {
	N int `json:"n" format:"int32"`
}

// FormatOnUUID sets a format on a uuid.UUID query parameter, which
// UnmarshalText reads.
type FormatOnUUID struct {
	ID uuid.UUID `json:"-" query:"id" format:"hostname"`
}

// FormatOnEmail sets a format on types.Email, a string with a format and a
// decoder of its own.
type FormatOnEmail struct {
	Email *types.Email `json:"email" format:"idn-email"`
}

// Slug is a named string with no format or decoder of its own.
type Slug string

// FormatOnString sets formats on strings, which are read as they are.
type FormatOnString struct {
	Day   string                 `json:"day" format:"date"`
	Email *string                `json:"email" format:"email"`
	Slug  Slug                   `json:"slug" format:"hostname"`
	Hosts []string               `json:"hosts" format:"hostname"`
	Since types.Optional[string] `json:"since" format:"date"`
	On    string                 `json:"-" query:"on" format:"date"`
}

// EnumKey keys a map by an enum type, whose keys nothing validates.
type EnumKey struct {
	Counts map[RunStatus]int `json:"counts"`
}

// NestedEnumKey reaches a map keyed by an enum through an Optional of a
// slice, a map value and a nested struct.
type NestedEnumKey struct {
	Counts types.Optional[[]map[RunStatus]int] `json:"counts"`
	ByName map[string]map[Mode]int             `json:"by_name"`
	Inner  *EnumKey                            `json:"inner"`
}

// EnumValue uses an enum type as map values, which the schema validates.
type EnumValue struct {
	Statuses map[string]RunStatus `json:"statuses"`
}

// Pair is used in both directions, so its request variant is PairInput.
type Pair struct {
	V *int `json:"v,omitempty"`
}

// PairInput collides with Pair's request variant name.
type PairInput struct {
	W int `json:"w"`
}
