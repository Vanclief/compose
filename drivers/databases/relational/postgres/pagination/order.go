package pagination

import (
	"fmt"

	"github.com/vanclief/ez"
)

// OrderDirection represents the allowed direction for pagination.
type OrderDirection string

const (
	ASC  OrderDirection = "asc"
	DESC OrderDirection = "desc"
)

// checkDirection returns ez.EINTERNAL for any direction other than ASC or
// DESC: the direction is the endpoint's own Options.Direction.
func checkDirection(direction OrderDirection) error {
	if direction == ASC || direction == DESC {
		return nil
	}

	msg := fmt.Sprintf("Order direction must be %q or %q, got %q", ASC, DESC, direction)
	return ez.New(ez.EINTERNAL, msg, nil)
}

// orderValues returns the comparison operator of the keyset condition and the
// ORDER BY keyword for direction, which checkDirection has already accepted.
func orderValues(direction OrderDirection) (comparator, keyword string) {
	if direction == DESC {
		return "<", "DESC"
	}

	return ">", "ASC"
}
