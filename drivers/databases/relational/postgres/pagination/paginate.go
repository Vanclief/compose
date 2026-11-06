package pagination

import (
	"context"
	"database/sql/driver"
	"fmt"
	"reflect"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/schema"
	"github.com/vanclief/ez"
)

// Options is the endpoint's server-side list configuration, passed to
// Paginate. None of it comes from the client.
type Options struct {
	// Direction is the default order, used when the request has no Sort. A Direction other than ASC or DESC,
	// the zero value included, is ez.EINTERNAL, even under a client sort.
	Direction OrderDirection
	// SortFields is the allowlist of client sorts: sort name to a column of the model's table,
	// e.g. {"name": "name"}, rendered as ?TableAlias."name" from the model's bun schema. Empty means the
	// client cannot choose the sort. A client-sortable column must be NOT NULL and selected by the query: the
	// keyset comparison skips rows whose sort value is NULL, and a column the query does not select pages from
	// a zero value. Give each sortable column an index on (column, unique column) for large tables. A Go float
	// field sorts correctly only on a column no wider than its type, holding finite values: a float32 only on
	// real, a float64 on real or double precision. A wider column, double precision under a float32 or numeric
	// with more digits than the field holds, is rounded in the cursor, so pages repeat or skip rows and may
	// never end, and NaN or ±Infinity cannot be stored in a cursor, so the page fails with ez.EINTERNAL. Map a
	// numeric column to a string or decimal type implementing driver.Valuer instead.
	SortFields map[string]string
	// AllowCount lets the client ask for a count with IncludeCount. False means the client cannot ask for a
	// count. The count is a count(*) over the whole filtered query on every request that asks for it, so
	// enable it only on lists whose filtered set is bounded. The count does not run the model's BeforeSelect
	// hook, so rows that hook filters out of the page are still counted: filter on the query passed to
	// Paginate instead. A count renders the query twice, and bun runs the apply function of a has-one or
	// belongs-to relation on every render. What an apply function adds to the query it is given (conditions,
	// joins, columns) is not repeated in the page query. Two things can still change the page query when a
	// count is asked for, so the page can fail or disagree with the total: an apply function that calls
	// Relation with another apply function, or JoinOn on a join the query already had, and a relation with
	// an apply function inside a With query or a subquery. Do not enable the count on such a list.
	AllowCount bool
}

// Paginate runs one page of q, a select the caller has already scoped and
// filtered, and returns its items and the response to embed next to them.
//
// It validates r, resolves the order, decodes the cursor and checks it was
// issued for that order, applies r.Filter, counts the filtered rows when
// r.IncludeCount is set (before the cursor, so the count covers the whole
// filtered set), applies the cursor, the order and the limit, scans and issues
// the next cursor. The order is r.Sort when set, resolved against
// opts.SortFields and rendered from the model's bun schema, otherwise
// model.GetSortField() in opts.Direction, with model.GetUniqueField() as the
// tie-breaker. A direction other than ASC or DESC is ez.EINTERNAL. A cursor
// issued for another sort field or direction is ez.EINVALID; a cursor without
// a direction predates Direction and was issued under the endpoint's default
// order, so it is held to opts.Direction.
//
// The count is closed by default: r.IncludeCount without opts.AllowCount is
// ez.EINVALID, before any query.
//
// A query error keeps its own code: an ez error keeps its code,
// context.Canceled is ez.ECANCELED, context.DeadlineExceeded or a network
// timeout is ez.ETIMEOUT, and any other error, a Postgres statement_timeout
// included, is ez.EINTERNAL. Whether and when a done context ends a query,
// and with which error, is up to database/sql and the driver.
//
// q's top-level conditions must be joined by AND: put OR conditions inside
// WhereGroup(" AND ", ...). After a top-level OR, r.Filter, the cursor and
// bun's soft-delete scope bind only to the last OR branch, so the cursor does
// not hold back rows matching the other branches: pages repeat them, deleted
// ones included, and once they fill a page paging never ends.
//
// A query with Union is not supported: the cursor, the order and the limit
// cover only its first member, and a count fails or is wrong.
//
// The count does not run the model's BeforeSelect hook, as bun's Count does
// not, so rows that hook filters out of the page are still counted. Such
// filtering belongs on q.
//
// A client sort is rendered on ?TableAlias, so it needs q to have a Model; a
// client sort on a query without one is ez.EINTERNAL.
//
// When q has Model(&items) with items a []T, T being the model argument's
// type, the rows are scanned into it, which keeps has-many relations working,
// and items is left holding the returned page. A query model that is a slice
// of another type, such as []*T, is ez.EINTERNAL. Without a slice model the
// rows are scanned into a new slice. q is modified in place, as bun queries
// are.
func Paginate[T Paginatable](ctx context.Context, q *bun.SelectQuery, r *CursorRequest, model T, opts Options) ([]T, CursorResponse, error) {
	err := r.Validate()
	if err != nil {
		return nil, CursorResponse{}, ez.Wrap(err)
	}

	if r.IncludeCount && !opts.AllowCount {
		return nil, CursorResponse{}, ez.New(ez.EINVALID, "This list cannot be counted", nil)
	}

	order, err := r.order(q.DB(), model, opts)
	if err != nil {
		return nil, CursorResponse{}, ez.Wrap(err)
	}

	if order.schemaField != nil && q.GetModel() == nil {
		return nil, CursorResponse{}, ez.New(ez.EINTERNAL, "A client sort needs a query with Model", nil)
	}

	dest, err := queryModel[T](q)
	if err != nil {
		return nil, CursorResponse{}, ez.Wrap(err)
	}

	cursor, err := DecodeCursor(r.Cursor)
	if err != nil {
		return nil, CursorResponse{}, ez.Wrap(err)
	}

	if cursor != nil {
		cursorDirection := cursor.Direction
		if cursorDirection == "" {
			cursorDirection = string(opts.Direction)
		}

		if cursor.SortField != order.expr || cursorDirection != string(order.direction) {
			return nil, CursorResponse{}, ez.New(ez.EINVALID, "Cursor was issued for a different order", nil)
		}
	}

	q, err = applyFilters(q, r.Filter)
	if err != nil {
		return nil, CursorResponse{}, ez.Wrap(err)
	}

	var totalCount *int
	if r.IncludeCount {
		// Rendering a query runs the apply functions of its has-one and
		// belongs-to relations on it, so counting on q would leave what they
		// add twice in the page query. A copy of the query value takes what
		// they append instead and, unlike bun's Clone, keeps q's connection,
		// soft-delete flags, WherePK and stored error. The copy shares q's
		// table model, the elements of its slices and the queries it embeds
		// (With, union, subquery arguments), so an apply function's Relation,
		// or JoinOn on a join q already had, and the relations of an embedded
		// query, still reach the page query, as they would counting on q.
		// Appends on the copy go past the end of q's slices or to a new array,
		// so q never sees them.
		countQuery := *q
		count, err := countQuery.Count(ctx)
		if err != nil {
			return nil, CursorResponse{}, ez.Wrap(err)
		}
		totalCount = &count
	}

	q = applyCursor(q, cursor, order, model.GetUniqueField(), r.Limit)

	dest, err = scanPage(ctx, q, dest)
	if err != nil {
		return nil, CursorResponse{}, ez.Wrap(err)
	}

	items, resp, err := buildPage(*dest, r.Limit, order)
	if err != nil {
		return nil, CursorResponse{}, ez.Wrap(err)
	}

	// Trim the caller's slice too, so it does not keep the row past the page.
	*dest = items
	resp.TotalCount = totalCount

	return items, resp, nil
}

// modelField returns the bun field of column on the model's table. A column
// the model does not have, a scanonly field, or a field whose type a cursor
// cannot hold is ez.EINTERNAL: Options.SortFields is server configuration. A
// cursor holds a basic kind (bool, ints, uints, floats, string), a time.Time
// or a driver.Valuer, with a value or pointer receiver.
func modelField(db *bun.DB, model any, column string) (*schema.Field, error) {
	typ := reflect.TypeOf(model)
	if typ != nil && typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}

	if typ == nil || typ.Kind() != reflect.Struct {
		msg := fmt.Sprintf("Sortable model must be a struct, got %v", typ)
		return nil, ez.New(ez.EINTERNAL, msg, nil)
	}

	field, ok := db.Table(typ).FieldMap[column]
	if !ok || field.Tag.HasOption("scanonly") {
		msg := fmt.Sprintf("Sort column %q is not a column of %s; SortFields values are column names such as \"name\"", column, typ)
		return nil, ez.New(ez.EINTERNAL, msg, nil)
	}

	if !cursorHoldsType(field.IndirectType) {
		msg := fmt.Sprintf("Sort column %q has type %s, which a cursor cannot hold", column, field.IndirectType)
		return nil, ez.New(ez.EINTERNAL, msg, nil)
	}

	return field, nil
}

// cursorHoldsType reports whether typ is a basic kind, a time.Time or a
// driver.Valuer through a value or pointer receiver.
func cursorHoldsType(typ reflect.Type) bool {
	switch typ.Kind() {
	case reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64,
		reflect.String:
		return true
	}

	if typ == reflect.TypeFor[time.Time]() {
		return true
	}

	return reflect.PointerTo(typ).Implements(reflect.TypeFor[driver.Valuer]())
}

// queryModel returns the query's own *[]T, or nil when q has no slice model.
// A slice model of another type is ez.EINTERNAL.
func queryModel[T any](q *bun.SelectQuery) (*[]T, error) {
	model := q.GetModel()
	if model == nil {
		return nil, nil
	}

	value := model.Value()
	dest, ok := value.(*[]T)
	if ok {
		return dest, nil
	}

	typ := reflect.TypeOf(value)
	if typ != nil && typ.Kind() == reflect.Pointer && typ.Elem().Kind() == reflect.Slice {
		msg := fmt.Sprintf("Query model is %T, Paginate needs *%T", value, []T(nil))
		return nil, ez.New(ez.EINTERNAL, msg, nil)
	}

	return nil, nil
}

// scanPage scans q into dest, the query's own slice, or into a new slice when
// dest is nil, and returns the slice it scanned into. A query error keeps its
// own code.
func scanPage[T any](ctx context.Context, q *bun.SelectQuery, dest *[]T) (*[]T, error) {
	if dest != nil {
		err := q.Scan(ctx)
		if err != nil {
			return nil, ez.Wrap(err)
		}
		return dest, nil
	}

	items := []T{}
	err := q.Scan(ctx, &items)
	if err != nil {
		return nil, ez.Wrap(err)
	}

	return &items, nil
}
