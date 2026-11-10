package fixture

import (
	"mime/multipart"

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

// Pair is used in both directions, so its request variant is PairInput.
type Pair struct {
	V *int `json:"v,omitempty"`
}

// PairInput collides with Pair's request variant name.
type PairInput struct {
	W int `json:"w"`
}
