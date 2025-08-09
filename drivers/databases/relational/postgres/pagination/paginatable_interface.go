package pagination

// Paginatable interface that models must implement to use pagination
type Paginatable interface {
	GetSortField() string        // Returns sort field name without table prefix
	GetSortValue() interface{}   // Returns the value for the sort field
	GetUniqueField() string      // Returns unique field name without table prefix
	GetUniqueValue() interface{} // Returns the value for the unique field
}
