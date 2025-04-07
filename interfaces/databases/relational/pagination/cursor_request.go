package pagination

import (
	"fmt"
	"strings"

	"github.com/uptrace/bun"
	"github.com/vanclief/compose/interfaces/databases/relational/query"
	"github.com/vanclief/ez"
)

// CursorRequest represents a cursor paginated request
type CursorRequest struct {
	Limit  int            `json:"limit"`
	Cursor string         `json:"cursor"`
	Filter []query.Filter `json:"filters,omitempty"`
}

func (r *CursorRequest) Validate() error {
	if r.Limit > MAX_PAGE_SIZE {
		return ez.New("CursorRequest.Validate", ez.EINVALID, "Page size must be less than 1000", nil)
	} else if r.Limit == 0 {
		r.Limit = DEFAULT_LIMIT
	}

	return nil
}

// ApplyCursorToQuery adds cursor-based pagination to a bun query based on the Paginatable model.
// The order parameter is of type OrderDirection, ensuring that only valid order directions are used.
func ApplyCursorToQuery[T Paginatable](query *bun.SelectQuery, r *CursorRequest, model T, order OrderDirection) (*bun.SelectQuery, error) {
	const op = "pagination.ApplyCursorToQueryModel"

	// Validate the request parameters (i
	err := r.Validate()
	if err != nil {
		return nil, ez.Wrap(op, err)
	}

	// Increment the limit by 1 to check for a next page
	query = query.Limit(r.Limit + 1)

	// Use the provided order argument (of type OrderDirection) to get the proper comparison operator and ORDER BY direction
	comparator, orderDirection := getOrderValues(order)

	// Decode the cursor from the request
	cursor, err := DecodeCursor(r.Cursor)
	if err != nil {
		return nil, ez.Wrap(op, err)
	}

	// Apply keyset pagination conditions based on the decoded cursor
	if cursor != nil {
		if strings.Contains(model.GetSortField(), "(") {
			// For complex sort fields (e.g., functions like LENGTH(unit.name))
			condition := fmt.Sprintf("(%s, %s) %s (?, ?)", model.GetSortField(), model.GetUniqueField(), comparator)
			query = query.Where(condition, cursor.SortValue, cursor.UniqueValue)
		} else if model.GetSortField() == model.GetUniqueField() {
			// When sort field and unique field are the same, a single condition is enough
			query = query.Where(fmt.Sprintf("%s %s ?", model.GetSortField(), comparator), cursor.SortValue)
		} else {
			// For non-unique sort fields, add a tie-breaker condition using the unique field
			condition := fmt.Sprintf("(%s %s ? OR (%s = ? AND %s %s ?))",
				model.GetSortField(), comparator,
				model.GetSortField(), model.GetUniqueField(), comparator)
			query = query.Where(condition, cursor.SortValue, cursor.SortValue, cursor.UniqueValue)
		}
	}

	// Apply any additional filter like date filters
	for _, filter := range r.Filter {
		query = query.Where(fmt.Sprintf("%s %s ?", filter.Field, filter.Comparison), filter.Value)
	}

	// Order primarily by the sort field and then, if necessary, by the unique field as a tie-breaker.
	orderByClause := fmt.Sprintf("%s %s", model.GetSortField(), orderDirection)
	if model.GetSortField() != model.GetUniqueField() {
		orderByClause += fmt.Sprintf(", %s %s", model.GetUniqueField(), orderDirection)
	}
	query = query.OrderExpr(orderByClause)

	return query, nil
}
