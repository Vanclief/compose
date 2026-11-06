package pagination

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/schema"
	"github.com/vanclief/compose/drivers/databases/relational/postgres/query"
	"github.com/vanclief/ez"
)

// CursorRequest represents a cursor paginated request.
//
// Limit, Cursor, Sort and IncludeCount come from the client. Filter is
// server-side only and never read from client input: it is hidden from the
// JSON and XML decoders echo binds bodies with.
//
// Sort is one of the Options.SortFields keys, prefixed with "-" for descending
// ("-name"); see Options.SortFields. An empty Sort orders by the model's
// GetSortField in Options.Direction.
//
// IncludeCount asks for a count, which Paginate runs only when the endpoint
// sets Options.AllowCount; see Options.AllowCount. The endpoint passes its
// Options to Paginate.
type CursorRequest struct {
	Limit        int    `json:"limit"`
	Cursor       string `json:"cursor"`
	Sort         string `json:"sort,omitempty"`
	IncludeCount bool   `json:"include_count,omitempty"`

	// Filter is server-side only: Field and Comparison are interpolated into SQL and must never come from client input.
	Filter []query.Filter `json:"-" xml:"-"`
}

// Validate applies the default limit and clamps the limit to MAX_PAGE_SIZE. A
// negative limit is ez.EINVALID. It does not check Sort: the sort allowlist is
// an argument of Paginate, so Paginate, not Validate, resolves Sort.
func (r *CursorRequest) Validate() error {
	if r.Limit < 0 {
		return ez.New(ez.EINVALID, "Page size cannot be negative", nil)
	}

	if r.Limit == 0 {
		r.Limit = DEFAULT_LIMIT
	}

	if r.Limit > MAX_PAGE_SIZE {
		r.Limit = MAX_PAGE_SIZE
	}

	return nil
}

// sortOrder is the effective order of a page.
type sortOrder struct {
	expr        string        // SQL expression ordered by, as pinned in the cursor
	schemaField *schema.Field // bun field of a client sort, nil for the model's default sort
	direction   OrderDirection
}

// order returns the effective order: the client sort when Sort is set,
// otherwise the model's default sort field in opts.Direction. A direction
// other than ASC or DESC is ez.EINTERNAL, even under a client sort. Sort
// missing from opts.SortFields, or no SortFields, is ez.EINVALID; a SortFields
// value modelField rejects (not a column, scanonly, or a type a cursor cannot
// hold) is ez.EINTERNAL.
func (r *CursorRequest) order(db *bun.DB, model Paginatable, opts Options) (sortOrder, error) {
	err := checkDirection(opts.Direction)
	if err != nil {
		return sortOrder{}, ez.Wrap(err)
	}

	if r.Sort == "" {
		order := sortOrder{
			expr:      model.GetSortField(),
			direction: opts.Direction,
		}
		return order, nil
	}

	if len(opts.SortFields) == 0 {
		return sortOrder{}, ez.New(ez.EINVALID, "This list cannot be sorted", nil)
	}

	name := strings.TrimPrefix(r.Sort, "-")
	clientDirection := ASC
	if name != r.Sort {
		clientDirection = DESC
	}

	column, ok := opts.SortFields[name]
	if !ok {
		names := slices.Sorted(maps.Keys(opts.SortFields))
		msg := fmt.Sprintf("Sort must be one of %s, prefixed with - for descending", strings.Join(names, ", "))
		return sortOrder{}, ez.New(ez.EINVALID, msg, nil)
	}

	field, err := modelField(db, model, column)
	if err != nil {
		return sortOrder{}, ez.Wrap(err)
	}

	order := sortOrder{
		expr:        "?TableAlias." + string(field.SQLName),
		schemaField: field,
		direction:   clientDirection,
	}

	return order, nil
}

// applyFilters adds the server-side filters to q.
func applyFilters(q *bun.SelectQuery, filters []query.Filter) (*bun.SelectQuery, error) {
	for _, filter := range filters {
		switch filter.Comparison {
		case ">=", "<=":
		default:
			return nil, ez.New(ez.EINTERNAL, "Filter comparison must be >= or <=", nil)
		}
		q = q.Where("? "+filter.Comparison+" ?", bun.Ident(filter.Field), filter.Value)
	}

	return q, nil
}

// applyCursor adds the keyset condition of cursor, when there is one, the
// ORDER BY and the limit plus one row that tells whether a next page exists.
// The caller has already checked that cursor was issued for order.
func applyCursor(q *bun.SelectQuery, cursor *Cursor, order sortOrder, uniqueField string, limit int) *bun.SelectQuery {
	comparator, keyword := orderValues(order.direction)

	if cursor != nil {
		if order.expr == uniqueField {
			q = q.Where(fmt.Sprintf("%s %s ?", order.expr, comparator), cursor.SortValue)
		} else {
			condition := fmt.Sprintf("(%s, %s) %s (?, ?)", order.expr, uniqueField, comparator)
			q = q.Where(condition, cursor.SortValue, cursor.UniqueValue)
		}
	}

	orderBy := order.expr + " " + keyword
	if order.expr != uniqueField {
		orderBy += ", " + uniqueField + " " + keyword
	}

	return q.OrderExpr(orderBy).Limit(limit + 1)
}
