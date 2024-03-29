package postgres

import (
	"context"

	"github.com/vanclief/ez"
)

// CreateTables - Creates the database schema if it doesn't already exist
func (db *DB) CreateTables(models []interface{}) error {
	const op = "database.CreateTables"

	ctx := context.Background()

	for _, model := range models {
		_, err := db.NewCreateTable().
			Model(model).
			IfNotExists().
			Exec(ctx)
		if err != nil {
			return ez.Wrap(op, err)
		}
	}

	return nil
}

// RegisterModels - Registers many-to-many relationship
func (db *DB) RegisterModels(models []interface{}) error {
	const op = "database.RegisterModels"

	for _, model := range models {
		db.RegisterModel(model)
	}

	return nil
}

// ResetTables - Drops and recreates the database schema
func (db *DB) ResetTables(models []interface{}) error {
	const op = "database.ResetTables"

	ctx := context.Background()

	for _, model := range models {
		err := db.ResetModel(ctx, model)
		if err != nil {
			return ez.Wrap(op, err)
		}
	}

	return nil
}
