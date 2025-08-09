package pagination

import "strings"

// OrderDirection represents the allowed direction for pagination.
type OrderDirection string

const (
	ASC  OrderDirection = "asc"
	DESC OrderDirection = "desc"
)

// getOrderValues returns the comparison operator for filtering and the order direction string for the ORDER BY clause
// based on the provided order, which is now of type OrderDirection.
func getOrderValues(order OrderDirection) (compOperator string, orderDirection string) {
	if strings.ToLower(string(order)) == "desc" {
		return "<", "DESC"
	}
	return ">", "ASC"
}
