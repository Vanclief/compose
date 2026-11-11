package relational

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/uptrace/bun/dialect"
	"github.com/uptrace/bun/migrate"
	"github.com/vanclief/ez"
)

// migrationLockKey is the key of the PostgreSQL advisory lock one instance
// holds while it runs migrations ("compose" in ASCII). Advisory locks are per
// database, so one fixed key serializes every instance of an app.
const migrationLockKey int64 = 0x636f6d706f7365

// migrationLockPoll is how long an instance waits between two tries of the
// migration lock.
const migrationLockPoll = 500 * time.Millisecond

// RunMigrations - Executes all pending migrations. Each one is marked as
// applied only after its Up succeeds, so a failed migration stays pending and
// the next start retries it: every migration must be safe to run twice.
// Rolling back is an explicit call to RollbackLastMigration. On PostgreSQL it
// first waits for any other instance running migrations on the same database
// to finish.
func (db *DB) RunMigrations(migrations *migrate.Migrations) error {
	ctx := context.Background()

	if len(migrations.Sorted()) == 0 {
		log.Info().Msg("No pending migrations to run")
		return nil
	}

	unlock, err := db.lockMigrations(ctx)
	if err != nil {
		return ez.Wrap(err)
	}
	defer unlock()

	err = db.runMigrations(ctx, migrations)
	if err != nil {
		return ez.Wrap(err)
	}

	return nil
}

// runMigrations - RunMigrations for a caller that already holds the migration lock
func (db *DB) runMigrations(ctx context.Context, migrations *migrate.Migrations) error {
	if len(migrations.Sorted()) == 0 {
		log.Info().Msg("No pending migrations to run")
		return nil
	}

	migrator := migrate.NewMigrator(db.migrationDB(), migrations, migrate.WithMarkAppliedOnSuccess(true))
	err := migrator.Init(ctx)
	if err != nil {
		return ez.Wrap(err)
	}

	group, err := migrator.Migrate(ctx)
	if err != nil {
		// When a migration fails bun returns the group up to and including it,
		// whether its Up or the write of its ledger row failed; either way it
		// is not marked and runs again on the next start. The ones before it
		// are applied and stay that way.
		failed := log.Error().Err(err)
		applied := group.Migrations
		if len(applied) > 0 {
			last := applied[len(applied)-1]
			failed = failed.Str("ID", last.Name).Str("Name", last.Comment)
			applied = applied[:len(applied)-1]
		}
		logExecuted(applied)

		failed.Msg("Failed to apply migration")
		return ez.Wrap(err)
	}

	if group.IsZero() {
		log.Info().Msg("No pending migrations to run")
		return nil
	}

	logExecuted(group.Migrations)

	return nil
}

// logExecuted - Logs each migration whose Up ran
func logExecuted(migrations migrate.MigrationSlice) {
	for _, migration := range migrations {
		log.Warn().
			Str("ID", migration.Name).
			Str("Name", migration.Comment).
			Msg("Executed migration")
	}
}

// RollbackLastMigration - Rolls back the last applied group of migrations.
// Each migration is unmarked only after its Down succeeds, so a failed Down
// stays applied. It returns nil when the set is empty or no registered
// migration is applied. On PostgreSQL it fails with ECONFLICT while another
// instance runs or rolls back migrations on the same database: after a wait
// the last group would be a different one than the caller meant to undo.
func (db *DB) RollbackLastMigration(migrations *migrate.Migrations) error {
	ctx := context.Background()

	if len(migrations.Sorted()) == 0 {
		log.Info().Msg("No migrations to roll back")
		return nil
	}

	unlock, err := db.tryLockMigrations(ctx)
	if err != nil {
		return ez.Wrap(err)
	}
	defer unlock()

	migrator := migrate.NewMigrator(db.migrationDB(), migrations, migrate.WithMarkAppliedOnSuccess(true))
	err = migrator.Init(ctx)
	if err != nil {
		return ez.Wrap(err)
	}

	group, err := migrator.Rollback(ctx)
	if err != nil {
		return ez.Wrap(err)
	}

	if group.IsZero() {
		log.Info().Msg("No migrations to roll back")
		return nil
	}

	for _, migration := range group.Migrations {
		log.Warn().
			Str("ID", migration.Name).
			Str("Name", migration.Comment).
			Msg("Reverted migration")
	}

	return nil
}

// lockMigrations - Waits until this instance holds the migration lock and
// returns the function that releases it. The lock is a session-level advisory
// lock on a connection of its own out of the migration pool, which does
// nothing but hold it. It is tried again every migrationLockPoll instead of
// waited on with pg_advisory_lock, which the pool's lock_timeout would cut.
// On any dialect but PostgreSQL there is no lock and the release does nothing.
func (db *DB) lockMigrations(ctx context.Context) (func(), error) {
	if db.Dialect().Name() != dialect.PG {
		return func() {}, nil
	}

	conn, err := db.migrationDB().DB.Conn(ctx)
	if err != nil {
		return nil, ez.Wrap(err)
	}

	waiting := false
	for {
		locked, err := tryMigrationLock(ctx, conn)
		if err != nil {
			return nil, ez.Wrap(err)
		}

		if locked {
			break
		}

		if !waiting {
			log.Info().Msg("Waiting for another instance to finish its migrations")
			waiting = true
		}

		select {
		case <-ctx.Done():
			discardConn(conn)
			return nil, ez.Wrap(ctx.Err())
		case <-time.After(migrationLockPoll):
		}
	}

	return func() {
		unlockMigrations(conn)
	}, nil
}

// tryLockMigrations - Like lockMigrations, but fails with ECONFLICT when
// another instance holds the migration lock instead of waiting for it
func (db *DB) tryLockMigrations(ctx context.Context) (func(), error) {
	if db.Dialect().Name() != dialect.PG {
		return func() {}, nil
	}

	conn, err := db.migrationDB().DB.Conn(ctx)
	if err != nil {
		return nil, ez.Wrap(err)
	}

	locked, err := tryMigrationLock(ctx, conn)
	if err != nil {
		return nil, ez.Wrap(err)
	}

	if !locked {
		conn.Close() // nolint:errcheck // Returns it to the pool, fails only when already closed
		return nil, ez.New(ez.ECONFLICT, "Another instance is running migrations, try again when it has finished", nil)
	}

	return func() {
		unlockMigrations(conn)
	}, nil
}

// tryMigrationLock - Tries once to take the migration lock on conn's session.
// After an error the session may or may not hold the lock, so conn is closed.
func tryMigrationLock(ctx context.Context, conn *sql.Conn) (bool, error) {
	var locked bool
	err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", migrationLockKey).Scan(&locked)
	if err != nil {
		discardConn(conn)
		return false, ez.Wrap(err)
	}

	return locked, nil
}

// unlockMigrations - Releases the migration lock held by conn's session and
// returns conn to the pool. When the unlock fails the session is closed
// instead, and the server drops the lock with it.
func unlockMigrations(conn *sql.Conn) {
	// A fresh context, so the lock is released even when the run's is done
	var unlocked bool
	err := conn.QueryRowContext(context.Background(), "SELECT pg_advisory_unlock($1)", migrationLockKey).Scan(&unlocked)
	if err != nil || !unlocked {
		log.Warn().Err(err).Bool("Unlocked", unlocked).Msg("Could not release the migration lock, closing its connection")
		discardConn(conn)
		return
	}

	conn.Close() // nolint:errcheck // Returns it to the pool, fails only when already closed
}

// discardConn - Closes conn's session instead of returning it to the pool, so
// nothing set on that session reaches the pool's next user
func discardConn(conn *sql.Conn) {
	conn.Raw(func(any) error { // nolint:errcheck // Returns ErrBadConn on purpose
		return driver.ErrBadConn
	})
	conn.Close() // nolint:errcheck // Raw already closed it
}
