package requests

import (
	"github.com/vanclief/compose/interfaces/databases/relational"
	"github.com/vanclief/ez"
)

const MAX_PAGE_SIZE = 1000

type OffsetBasedList struct {
	Limit       int                     `json:"limit"`
	Offset      int                     `json:"offset"`
	DateFilters []relational.DateFilter `json:"date_filters"`
}

func (r *OffsetBasedList) Validate() error {
	if r.Limit-r.Offset > MAX_PAGE_SIZE {
		return ez.New("OffsetBasedList.Validate", ez.EINVALID, "Page size must be less than 1000", nil)
	}

	return nil
}

func (r *OffsetBasedList) ParseDatesToUnix(validDBColumns []string) error {
	const op = "OffsetBasedList.ParseDateToUnix"

	for _, dateFilter := range r.DateFilters {
		err := dateFilter.ParseToUnix(validDBColumns)
		if err != nil {
			return ez.Wrap(op, err)
		}
	}

	return nil
}

type KeysetBasedList struct {
	Limit       int                     `json:"limit"`
	LastValue   interface{}             `json:"last_value"`
	DateFilters []relational.DateFilter `json:"date_filters"`
}

func (r *KeysetBasedList) Validate() error {
	return nil
}

func (r *KeysetBasedList) ParseDatesToUnix(validDBColumns []string) error {
	const op = "KeysetBasedList.ParseDateToUnix"

	for _, dateFilter := range r.DateFilters {
		err := dateFilter.ParseToUnix(validDBColumns)
		if err != nil {
			return ez.Wrap(op, err)
		}
	}

	return nil
}
