package query

import (
	"time"

	"github.com/vanclief/ez"
)

// Filter represents a query filter
type Filter struct {
	Field      string
	Value      interface{}
	Comparison string
}

// TimeRangeFilters returns filters that bound column between from and to,
// either of which may be empty. Both are RFC 3339 timestamps, and a date
// alone is refused. Each bound is the instant as given and is itself in the
// range: nothing is added to to, so a caller that wants a whole day passes
// the last second of that day. The filter values are Unix seconds, so a
// fraction of a second in a bound is dropped.
func TimeRangeFilters(column, from, to string) ([]Filter, error) {
	var filters []Filter

	if from != "" {
		fromTimestamp, err := time.Parse(time.RFC3339, from)
		if err != nil {
			return nil, ez.New(ez.EINVALID, "invalid from_date format", err)
		}
		filters = append(filters, Filter{
			Field:      column,
			Value:      fromTimestamp.Unix(),
			Comparison: ">=",
		})
	}

	if to != "" {
		toTimestamp, err := time.Parse(time.RFC3339, to)
		if err != nil {
			return nil, ez.New(ez.EINVALID, "invalid to_date format", err)
		}
		filters = append(filters, Filter{
			Field:      column,
			Value:      toTimestamp.Unix(),
			Comparison: "<=",
		})
	}

	return filters, nil
}
