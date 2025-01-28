package requests

import (
	"github.com/vanclief/compose/interfaces/databases/relational"
	"github.com/vanclief/ez"
)

const (
	MAX_PAGE_SIZE = 1000
	DEFAULT_LIMIT = 50
)

type OffsetBasedList struct {
	Limit       int                     `json:"limit"`
	Offset      int                     `json:"offset"`
	DateFilters []relational.DateFilter `json:"date_filters"`
}

func (r *OffsetBasedList) Validate() error {
	if r.Limit-r.Offset > MAX_PAGE_SIZE {
		return ez.New("OffsetBasedList.Validate", ez.EINVALID, "Page size must be less than 1000", nil)
	} else if r.Limit == 0 {
		r.Limit = DEFAULT_LIMIT
	}

	return nil
}

func (r *OffsetBasedList) ParseDatesToUnix(validDBColumns []string) error {
	const op = "OffsetBasedList.ParseDateToUnix"

	for i := range r.DateFilters {
		err := r.DateFilters[i].ParseToUnix(validDBColumns)
		if err != nil {
			return ez.Wrap(op, err)
		}
	}

	return nil
}
