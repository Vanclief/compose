package pagination

// Paginatable interface that models must implement to use pagination
//
// GetSortField and GetUniqueField are interpolated raw into SQL (so the
// default sort may be an expression like LENGTH(?TableAlias.name)), so they
// must return compile-time constants and never client input. GetSortField and
// GetSortValue serve the default sort, used when the request has no Sort; a
// client-chosen sort reads its value from the model's bun schema instead.
// Neither is called under a client sort, so a query sorted by the client need
// not select the default sort column.
// GetSortField's text is recorded in cursors and compared on replay, so
// changing its spelling rejects every cursor in flight. GetUniqueField's text is
// recorded in cursors too but not checked on replay, so changing it or its
// column's type leaves cursors in flight carrying values of the old one.
// When a request carries a cursor, Paginate calls GetUniqueValue, and
// GetSortValue under the default sort, on a zero model to learn the type of
// the values the cursor must hold, and a cursor value of another type is
// ez.EINVALID; see Paginate.
type Paginatable interface {
	GetSortField() string        // Returns the SQL expression of the default sort, e.g. "?TableAlias.created_at"
	GetSortValue() interface{}   // Returns the value of the default sort field
	GetUniqueField() string      // Returns the SQL expression of the unique tie-breaker, e.g. "?TableAlias.id"
	GetUniqueValue() interface{} // Returns the value for the unique field
}
