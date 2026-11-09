package fixture

import "github.com/google/uuid"

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
