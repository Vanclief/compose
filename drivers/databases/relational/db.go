package relational

import (
	"context"
	"errors"
	"time"

	"github.com/rs/zerolog/log"
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

// createLockKey - The key of the PostgreSQL advisory lock one instance holds
// while it runs CreateTables or CreateExtensions ("creates" in ASCII). It is
// not migrationLockKey, so a migration can call CreateTables and table
// creation does not wait for another instance's migrations. A migration that
// calls it inside a transaction holding a table lock can still stall the
// instance that holds this lock, see docs/migrations.md.
const createLockKey int64 = 0x63726561746573

// createLockWait - How long CreateTables and CreateExtensions wait for the
// create lock before they go on without it. It bounds a boot behind an
// instance that holds the lock and does not finish, and a call made from a
// model's hook while the outer call holds the lock. A variable so tests can
// shorten it.
var createLockWait = 30 * time.Second

// createLockPoll - How long an instance waits between two tries of the create
// lock. Creating what already exists takes milliseconds, so the instances
// queued behind one another try often rather than every migrationLockPoll.
const createLockPoll = 20 * time.Millisecond

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
// Every instance can call it at startup: on PostgreSQL it holds the create
// lock for the whole call (see lockCreate), so instances booting together
// create their tables one at a time, while the statements and the models'
// create-table hooks run on the main pool as without it. The lock takes one
// connection of its own for the call, out of the migration pool when there is
// one. Where an instance runs without the lock, concurrent CREATE TABLE IF
// NOT EXISTS statements, and the statements of a model's create-table hooks,
// can fail on the catalog when another instance creates the same object at
// the same moment, and the model's statement is then run again, up to
// createAttempts attempts in all. A model's create-table hooks run on every
// attempt, since bun runs them inside the statement's Exec, AfterCreateTable
// also when the table already exists, so they must tolerate being run again,
// as they already must on every boot. A hook statement that rewrites an
// existing catalog row, such as CREATE OR REPLACE FUNCTION or GRANT, can still
// fail with an error that is not retried when instances run it at the same
// moment without all holding the lock: an instance on a compose older than
// v1.39.0 during a rolling deploy, one whose wait for the lock ran out or
// that could not take it, or one that does not take it because the lock
// would come out of a main pool with a connection limit. A hook that calls
// CreateTables or CreateExtensions itself waits the whole createLockWait,
// since the outer call holds the lock on another session.
func (db *DB) CreateTables(models []interface{}) error {
	ctx := context.Background()

	if len(models) == 0 {
		return nil
	}

	unlock := db.lockCreate()
	defer unlock()

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

// lockCreate - Takes the create lock for CreateTables and CreateExtensions
// and returns the function that releases it: lockAdvisory on createLockKey,
// tried every createLockPoll. It waits for the lock up to createLockWait,
// then logs a warning and returns without it, as it does when taking the lock
// fails: the lock only spares the retries, so it is never a reason for the
// call to fail. It does not take it when the lock would come out of the main
// pool and that pool has a connection limit: the lock would hold one of the
// connections the statements then wait for, with no bound. On any dialect but
// PostgreSQL there is no lock.
// Without the lock the release does nothing.
func (db *DB) lockCreate() func() {
	if db.Dialect().Name() != dialect.PG {
		return func() {}
	}

	if db.migrationDB().DB == db.DB.DB && db.DB.Stats().MaxOpenConnections != 0 {
		return func() {}
	}

	deadline := time.Now().Add(createLockWait)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	unlock, err := db.lockAdvisory(ctx, createLockKey, createLockPoll, "Waiting for another instance to finish creating tables or extensions")
	// The driver puts the context's deadline on the connection, so a try cut
	// off by it can fail with an i/o timeout an instant before ctx.Err()
	// says so: the deadline having passed is what tells a wait that ran out
	// from a lock that could not be taken.
	if err != nil && (ctx.Err() != nil || !time.Now().Before(deadline)) {
		log.Warn().Dur("Waited", createLockWait).Msg("Did not get the create lock in time, creating tables or extensions without it")
		return func() {}
	}
	if err != nil {
		log.Warn().Err(err).Msg("Could not take the create lock, creating tables or extensions without it")
		return func() {}
	}

	return unlock
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
// at startup: it holds the create lock for the whole call, as CreateTables
// does, and a CREATE EXTENSION IF NOT EXISTS that fails on the catalog
// because another instance created the extension at the same moment, which
// takes an instance without the lock, is run again, up to createAttempts
// attempts in all.
func (db *DB) CreateExtensions(extensions []string) error {
	ctx := context.Background()

	if len(extensions) == 0 {
		return nil
	}

	if db.Dialect().Name() != dialect.PG {
		return ez.New(ez.ENOTIMPLEMENTED, "Database extensions are only supported by PostgreSQL", nil)
	}

	unlock := db.lockCreate()
	defer unlock()

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
