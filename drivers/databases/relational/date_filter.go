package relational

import (
	"fmt"
	"slices"
	"time"

	"github.com/uptrace/bun"
	"github.com/vanclief/ez"
)

type DateFilter struct {
	DateColumn   string `json:"date_column"`
	FromDate     string `json:"from_date"`
	ToDate       string `json:"to_date"`
	FromDateUnix int64  `json:"-"`
	ToDateUnix   int64  `json:"-"`
}

func (db *DB) AddDateFilters(query *bun.SelectQuery, filters []DateFilter) *bun.SelectQuery {
	for _, filter := range filters {
		if filter.DateColumn != "" {
			if filter.FromDateUnix != 0 {
				query = query.
					Where(fmt.Sprintf("%s >= ?", filter.DateColumn), filter.FromDateUnix)
			}

			if filter.ToDateUnix != 0 {
				query = query.
					Where(fmt.Sprintf("%s <= ?", filter.DateColumn), filter.ToDateUnix)
			}
		}
	}

	return query
}

func (df *DateFilter) ParseToUnix(validDBColumns []string) error {
	const op = "DateFilter.ParseToUnix"

	if df.DateColumn == "" {
		return nil
	} else if !slices.Contains(validDBColumns, df.DateColumn) {
		msg := fmt.Sprintf("%s is not a valid date filter", df.DateColumn)
		return ez.New(op, ez.EINVALID, msg, nil)
	}

	if df.FromDate != "" {
		fromDate, err := time.Parse(time.RFC3339, df.FromDate)
		if err != nil {
			return ez.Wrap(op, err)
		}

		df.FromDateUnix = fromDate.Unix()
	}

	if df.ToDate != "" {
		toDate, err := time.Parse(time.RFC3339, df.ToDate)
		if err != nil {
			return ez.Wrap(op, err)
		}

		df.ToDateUnix = toDate.Unix()
	}

	return nil
}
