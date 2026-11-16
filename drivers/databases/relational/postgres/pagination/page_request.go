package pagination

import (
	"github.com/vanclief/compose/drivers/databases/relational/postgres/query"
	"github.com/vanclief/ez"
)

// PageRequest is the request of a list the client can neither sort nor count:
// it has only Limit and Cursor, so the OpenAPI registry documents only limit
// and cursor. Embed it in the request type of a list that passes no
// Options.SortFields and no Options.AllowCount to Paginate, and pass
// &req.PageRequest to Paginate. A list the client can sort or count embeds
// CursorRequest instead.
//
// Paginate reads a PageRequest as a CursorRequest with no Sort and no
// IncludeCount: the page is in the model's GetSortField order in
// Options.Direction, and the response has no total count. Options.SortFields
// and Options.AllowCount have nothing to act on, so Paginate does not use
// them. A cursor issued for a PageRequest is the one a CursorRequest without
// Sort issues, so a list can move from one type to the other without breaking
// the cursors its clients hold.
//
// Limit and Cursor come from the client as query parameters, never body
// fields, so they carry json:"-" and xml:"-". Filter is server-side only and
// never read from client input. All three are hidden from the JSON and XML
// decoders echo binds bodies with.
type PageRequest struct {
	Limit  int    `json:"-" xml:"-" query:"limit" minimum:"0" maximum:"1000" doc:"Page size, defaults to 50"`
	Cursor string `json:"-" xml:"-" query:"cursor" doc:"Opaque cursor from the previous page"`

	// Filter is server-side only: Field and Comparison are interpolated into SQL and must never come from client input.
	Filter []query.Filter `json:"-" xml:"-"`
}

// Validate applies the default limit and clamps the limit to MAX_PAGE_SIZE. A
// negative limit is ez.EINVALID.
func (r *PageRequest) Validate() error {
	limit, err := validLimit(r.Limit)
	if err != nil {
		return ez.Wrap(err)
	}

	r.Limit = limit

	return nil
}

// cursorRequest returns the CursorRequest Paginate reads for r: its Limit,
// Cursor and Filter, with no Sort and no IncludeCount.
func (r *PageRequest) cursorRequest() *CursorRequest {
	return &CursorRequest{
		Limit:  r.Limit,
		Cursor: r.Cursor,
		Filter: r.Filter,
	}
}
