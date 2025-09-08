package relational

import (
	"context"

	"github.com/uptrace/bun"
)

// PaginableModel defines the minimal interface for cursor-based pagination.
type PaginableModel interface {
	// GetCursor returns a stable, opaque value representing the record’s
	// position in the sorted result set.
	GetCursor() string

	// GetSortField returns the DB column used for ordering in pagination queries.
	GetSortField() string

	// GetSortValue returns the value of the sort field for this record.
	GetSortValue() interface{}

	// GetUniqueField returns the DB column that uniquely identifies this record.
	GetUniqueField() string

	// GetUniqueValue returns the value of the unique field for this record.
	GetUniqueValue() interface{}
}

// DBModel defines the minimal interface for a model that can be persisted.
type DBModel interface {
	// Validate checks the model for logical and business-rule correctness.
	Validate() error

	// Insert validates and inserts the model into the DB.
	Insert(ctx context.Context, db bun.IDB) error

	// Update validates and updates the model in the DB.
	Update(ctx context.Context, db bun.IDB) error

	// Delete removes the model from the DB.
	Delete(ctx context.Context, db bun.IDB) error
}
