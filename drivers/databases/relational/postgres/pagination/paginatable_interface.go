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
// The default sort column must be NOT NULL and selected by a query sorted by
// it. When the last row of a page that has a next page returns from
// GetSortValue nil, a nil pointer, a driver.Valuer whose Value is nil, a zero
// bun.NullTime, or a zero time.Time returned directly while the column
// GetSortField names is a field of the model tagged nullzero, Paginate fails
// with ez.EINTERNAL instead of issuing a cursor that ends or repeats the list.
// That column is the text after GetSortField's last ".", without surrounding
// double quotes. Any other zero time.Time, behind a non-nil pointer, as a
// Valuer's Value or with a column not tagged nullzero, is a stored 0001-01-01
// and pages as one, so a NULL or an unselected column read into it is not
// caught, nor is a NULL GetSortValue returns as 0 or "".
//
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
