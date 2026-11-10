package fixture

import "github.com/vanclief/compose/types"

// OptionalRequest is a PATCH body of types.Optional fields.
type OptionalRequest struct {
	Name  types.Optional[string]   `json:"name" minLength:"1" doc:"Display name"`
	Mode  types.Optional[Mode]     `json:"mode" enum:"a"`
	Meta  types.Optional[Meta]     `json:"meta"`
	Tags  types.Optional[[]string] `json:"tags"`
	Small types.Optional[int32]    `json:"small"`
	Must  types.Optional[string]   `json:"must" required:"true"`
}

// OptionalResponse uses types.Optional in a response.
type OptionalResponse struct {
	Name types.Optional[string] `json:"name"`
	Mode types.Optional[Mode]   `json:"mode,omitzero"`
	Meta types.Optional[Meta]   `json:"meta,omitempty"`
}

// OptionalParam puts types.Optional on a query parameter.
type OptionalParam struct {
	Q types.Optional[string] `json:"-" query:"q"`
}

// OptionalPointer takes types.Optional by pointer, which the lint rejects.
type OptionalPointer struct {
	Name *types.Optional[string] `json:"name"`
}

// OptionalBadKeys reaches UnexportedEmbed through types.Optional.
type OptionalBadKeys struct {
	Item types.Optional[UnexportedEmbed] `json:"item"`
}

// OptionalWideBound sets a maximum the Optional's int8 cannot hold.
type OptionalWideBound struct {
	N types.Optional[int8] `json:"n" maximum:"1000"`
}

// Collections pairs each map field with a slice field under the same
// nullability rule (plain, omitempty, omitzero, pointer, pointer with
// omitempty, nullable:"false", required pointer, types.Optional): the map
// must be as nullable as the slice.
type Collections struct {
	M   map[string]int                 `json:"m"`
	S   []int                          `json:"s"`
	OM  map[string]int                 `json:"om,omitempty"`
	OS  []int                          `json:"os,omitempty"`
	ZM  map[string]int                 `json:"zm,omitzero"`
	ZS  []int                          `json:"zs,omitzero"`
	PM  *map[string]int                `json:"pm"`
	PS  *[]int                         `json:"ps"`
	POM *map[string]int                `json:"pom,omitempty"`
	POS *[]int                         `json:"pos,omitempty"`
	NM  map[string]int                 `json:"nm" nullable:"false"`
	NS  []int                          `json:"ns" nullable:"false"`
	RM  *map[string]int                `json:"rm" required:"true"`
	RS  *[]int                         `json:"rs" required:"true"`
	XM  types.Optional[map[string]int] `json:"xm"`
	XS  types.Optional[[]int]          `json:"xs"`
}
