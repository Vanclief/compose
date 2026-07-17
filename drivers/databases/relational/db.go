package relational

import (
	"context"

	"github.com/uptrace/bun"
	"github.com/vanclief/ez"
)

type DB struct {
	*bun.DB
}

// CreateTables - Creates the database schema if it doesn't already exist
func (db *DB) CreateTables(models []interface{}) error {
	ctx := context.Background()

	for _, model := range models {
		_, err := db.NewCreateTable().
			Model(model).
			IfNotExists().
			Exec(ctx)
		if err != nil {
			return ez.Wrap(err)
		}
	}

	return nil
}

// RegisterModels - Registers many-to-many relationship
func (db *DB) RegisterModels(models []interface{}) error {
	for _, model := range models {
		db.RegisterModel(model)
	}

	return nil
}

// ResetTables - Drops and recreates the database schema
func (db *DB) ResetTables(models []interface{}) error {
	ctx := context.Background()

	for _, model := range models {
		err := db.ResetModel(ctx, model)
		if err != nil {
			return ez.Wrap(err)
		}
	}

	return nil
}

// CreateExtensions - Creates a database extension if it doesn't already exist
func (db *DB) CreateExtensions(extensions []string) error {
	ctx := context.Background()

	// TODO: Only works with PSQL
	for _, extension := range extensions {
		_, err := db.NewRaw("CREATE EXTENSION IF NOT EXISTS ?", bun.Ident(extension)).Exec(ctx)
		if err != nil {
			return ez.Wrap(err)
		}
	}

	return nil
}
