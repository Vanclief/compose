package requests

import (
	"fmt"
	"slices"
	"time"

	"github.com/vanclief/compose/interfaces/databases/relational"
	"github.com/vanclief/ez"
)

const MAX_PAGE_SIZE = 1000

type StandardList struct {
	Limit       int                     `json:"limit"`
	Offset      int                     `json:"offset"`
	DateFilters []relational.DateFilter `json:"date_filters"` // TODO: Make this a DateFilter interface
}

func (r *StandardList) Validate() error {
	if r.Limit-r.Offset > MAX_PAGE_SIZE {
		return ez.New("StandardList.Validate", ez.EINVALID, "Page size must be less than 1000", nil)
	}

	return nil
}

func (r *StandardList) ParseDatesToUnix(validDBColumns []string) error {
	const op = "StandardList.ParseDateToUnix"

	for i, dateFilter := range r.DateFilters {

		if dateFilter.DateColumn == "" {
			continue
		} else if !slices.Contains(validDBColumns, r.DateFilters[i].DateColumn) {
			msg := fmt.Sprintf("%s is not a valid date filter", r.DateFilters[i].DateColumn)
			return ez.New(op, ez.EINVALID, msg, nil)
		}

		if r.DateFilters[i].FromDate != "" {
			fromDate, err := time.Parse(time.RFC3339, r.DateFilters[i].FromDate)
			if err != nil {
				return ez.Wrap(op, err)
			}

			r.DateFilters[i].FromDateUnix = fromDate.Unix()
		}

		if r.DateFilters[i].ToDate != "" {
			toDate, err := time.Parse(time.RFC3339, r.DateFilters[i].ToDate)
			if err != nil {
				return ez.Wrap(op, err)
			}

			r.DateFilters[i].ToDateUnix = toDate.Unix()
		}
	}

	return nil
}
