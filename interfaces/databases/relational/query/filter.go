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

// Handler helper to convert date strings to filters
func ParseDateToFilter(column, fromDate, toDate string) ([]Filter, error) {
	const op = "query.ParseDateToFilter"
	var filters []Filter

	if fromDate != "" {
		fromTimestamp, err := time.Parse(time.RFC3339, fromDate)
		if err != nil {
			return nil, ez.New(op, ez.EINVALID, "invalid from_date format", err)
		}
		filters = append(filters, Filter{
			Field:      column,
			Value:      fromTimestamp.Unix(),
			Comparison: ">=",
		})
	}

	if toDate != "" {
		toTimestamp, err := time.Parse(time.RFC3339, toDate)
		if err != nil {
			return nil, ez.New(op, ez.EINVALID, "invalid to_date format", err)
		}
		// Set to end of day
		toTimestamp = toTimestamp.Add(24*time.Hour - time.Second)
		filters = append(filters, Filter{
			Field:      column,
			Value:      toTimestamp.Unix(),
			Comparison: "<=",
		})
	}

	return filters, nil
}
