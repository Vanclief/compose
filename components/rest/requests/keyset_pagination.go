package requests

import (
	"github.com/vanclief/compose/drivers/databases/relational"
	"github.com/vanclief/ez"
)

type KeysetBasedList struct {
	Limit       int                     `json:"limit"`
	Cursor      string                  `json:"cursor"`
	DateFilters []relational.DateFilter `json:"date_filters"`
}

func (r *KeysetBasedList) Validate() error {
	if r.Limit > MAX_PAGE_SIZE {
		return ez.New(ez.EINVALID, "Page size must be less than 1000", nil)
	} else if r.Limit == 0 {
		r.Limit = DEFAULT_LIMIT
	}

	return nil
}

func (r *KeysetBasedList) ParseDatesToUnix(validDBColumns []string) error {
	for i := range r.DateFilters {
		err := r.DateFilters[i].ParseToUnix(validDBColumns)
		if err != nil {
			return ez.Wrap(err)
		}
	}

	return nil
}
