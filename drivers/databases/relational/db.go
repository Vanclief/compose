package relational

import (
	"context"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect"
	"github.com/vanclief/ez"
)

type DB struct {
	*bun.DB
	// MigrationDB is a second pool for migrations and the schema baseline load,
	// with no statement or read timeout and its own lock timeout. It is nil
	// when the driver does not provide one, then migrations run on DB.
	MigrationDB *bun.DB
}

// migrationDB - The pool migrations run on
func (db *DB) migrationDB() *bun.DB {
	if db.MigrationDB != nil {
		return db.MigrationDB
	}

	return db.DB
}

// Close - Closes the migration pool, when there is one, and then the main pool
func (db *DB) Close() error {
	if db.MigrationDB != nil {
		err := db.MigrationDB.Close()
		if err != nil {
			db.DB.Close() // nolint:errcheck // The first error is the one returned
			return ez.Wrap(err)
		}
	}

	err := db.DB.Close()
	if err != nil {
		return ez.Wrap(err)
	}

	return nil
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

// CreateExtensions - Creates a database extension if it doesn't already
// exist. Extensions are a Postgres concept, so requesting any on another
// dialect is an error rather than a silent no-op.
func (db *DB) CreateExtensions(extensions []string) error {
	ctx := context.Background()

	if len(extensions) == 0 {
		return nil
	}

	if db.Dialect().Name() != dialect.PG {
		return ez.New(ez.ENOTIMPLEMENTED, "Database extensions are only supported by PostgreSQL", nil)
	}

	for _, extension := range extensions {
		_, err := db.NewRaw("CREATE EXTENSION IF NOT EXISTS ?", bun.Ident(extension)).Exec(ctx)
		if err != nil {
			return ez.Wrap(err)
		}
	}

	return nil
}
