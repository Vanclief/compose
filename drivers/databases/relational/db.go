package relational

import (
	"context"
	"errors"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect"
	"github.com/vanclief/ez"
)

// createAttempts - How many times CreateTables and CreateExtensions run a
// statement while it fails because another instance created the same object
// at the same moment. Each lost race means another instance committed one
// object, so a call loses at most once per object its statement and the
// model's hooks create.
const createAttempts = 20

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

// CreateTables - Creates the database schema if it doesn't already exist.
// Every instance can call it at startup: on PostgreSQL concurrent CREATE TABLE
// IF NOT EXISTS statements, and the statements of a model's create-table
// hooks, can fail on the catalog when another instance creates the same
// object at the same moment, and the model's statement is then run again, up
// to createAttempts attempts in all. A model's create-table hooks run on every
// attempt, since bun runs them inside the statement's Exec, AfterCreateTable
// also when the table already exists, so they must tolerate being run again,
// as they already must on every boot. A hook statement that rewrites an
// existing catalog row, such as CREATE OR REPLACE FUNCTION or GRANT, can still
// fail when instances run it at the same moment, with an error that is not
// retried.
func (db *DB) CreateTables(models []interface{}) error {
	ctx := context.Background()

	for _, model := range models {
		err := retryCreate(func() error {
			_, err := db.NewCreateTable().
				Model(model).
				IfNotExists().
				Exec(ctx)
			return err
		})
		if err != nil {
			return ez.Wrap(err)
		}
	}

	return nil
}

// retryCreate - Runs create again while it fails with a catalog race, up to
// createAttempts times in all, and returns its last error. A lost race is not
// lost again on the same object, which exists by then, so an attempt that
// fails with the SQLSTATE, message and detail of the one before ends the
// retries: that error is not a race and running create again would not help.
func retryCreate(create func() error) error {
	var err error
	lost := ""
	for attempt := 0; attempt < createAttempts; attempt++ {
		err = create()
		if !isCreateRace(err) {
			return err
		}

		var pgErr interface{ Field(byte) string }
		if !errors.As(err, &pgErr) {
			return err
		}
		failure := pgErr.Field('C') + "\n" + pgErr.Field('M') + "\n" + pgErr.Field('D')
		if failure == lost {
			return err
		}
		lost = failure
	}

	return err
}

// isCreateRace - Whether err is the PostgreSQL error of a CREATE ... IF NOT
// EXISTS that lost to a concurrent creator: one that committed the same
// object between the statement's existence check and its catalog insert. A
// unique violation counts only on a table of the pg_catalog schema, so a
// violation on an app's own data is not taken for a race.
func isCreateRace(err error) bool {
	// pgdriver.Error is matched by its Field method, so SQLite-only builds do
	// not link pgdriver
	var pgErr interface{ Field(byte) string }
	if !errors.As(err, &pgErr) {
		return false
	}

	switch pgErr.Field('C') {
	case "23505": // unique violation, such as on pg_class_relname_nsp_index
		return pgErr.Field('s') == "pg_catalog"
	case "42P07", "42710": // duplicate table, duplicate object such as the row type
		return true
	}

	return false
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
// dialect is an error rather than a silent no-op. Every instance can call it
// at startup: a CREATE EXTENSION IF NOT EXISTS that fails on the catalog
// because another instance created the extension at the same moment is run
// again, up to createAttempts attempts in all.
func (db *DB) CreateExtensions(extensions []string) error {
	ctx := context.Background()

	if len(extensions) == 0 {
		return nil
	}

	if db.Dialect().Name() != dialect.PG {
		return ez.New(ez.ENOTIMPLEMENTED, "Database extensions are only supported by PostgreSQL", nil)
	}

	for _, extension := range extensions {
		err := retryCreate(func() error {
			_, err := db.NewRaw("CREATE EXTENSION IF NOT EXISTS ?", bun.Ident(extension)).Exec(ctx)
			return err
		})
		if err != nil {
			return ez.Wrap(err)
		}
	}

	return nil
}
