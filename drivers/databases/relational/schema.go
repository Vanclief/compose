package relational

import (
	"context"
	"fmt"

	"github.com/rs/zerolog/log"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/migrate"
	"github.com/vanclief/ez"
)

// bunMigrationsTable and bunMigrationLocksTable must match bun/migrate's
// default table names. They cover the two operations bun's API cannot
// express: marking migrations applied inside a transaction (the bootstrap)
// and reading lock state without acquiring the lock (acquiring it to probe
// would abort a concurrently starting migration). Both are guarded by
// tests that fail immediately if a bun upgrade renames the defaults.
const (
	bunMigrationsTable     = "bun_migrations"
	bunMigrationLocksTable = "bun_migration_locks"
)

// SchemaStatus - The state of the database schema relative to a migration
// registry
type SchemaStatus struct {
	// Fresh is true when no migrations have ever been recorded
	Fresh bool
	// Pending counts registered migrations with no applied record
	Pending int
	// Missing counts applied records that are no longer in the registry
	Missing int
	// Locked is true when the migration lock is held: a migration is
	// running, or a previous one crashed before unlocking — in which case
	// the recorded history cannot be trusted (bun records a migration
	// before executing it)
	Locked bool
}

// Current - Reports whether the schema is up to date with the registry
func (s SchemaStatus) Current() bool {
	return !s.Fresh && s.Pending == 0 && s.Missing == 0 && !s.Locked
}

// VerifySchema - Reports the schema's state relative to the registry
// without changing it (beyond ensuring bun's bookkeeping tables exist).
// Callers decide what to do with the answer: refuse to boot, instruct the
// operator to migrate, or call InitSchema.
func (db *DB) VerifySchema(migrations *migrate.Migrations) (SchemaStatus, error) {
	ctx := context.Background()

	status := SchemaStatus{}

	// An empty registry still gets verified against the recorded history:
	// any recorded migration is then Missing, never silently current
	if migrations == nil {
		migrations = migrate.NewMigrations()
	}

	migrator := migrate.NewMigrator(db.DB, migrations, migrate.WithUpsert(true))
	err := migrator.Init(ctx)
	if err != nil {
		return status, ez.Wrap(err)
	}

	applied, err := migrator.AppliedMigrations(ctx)
	if err != nil {
		return status, ez.Wrap(err)
	}

	missing, err := migrator.MissingMigrations(ctx)
	if err != nil {
		return status, ez.Wrap(err)
	}

	status.Fresh = len(applied) == 0
	status.Pending = countPendingMigrations(applied, migrations)
	status.Missing = len(missing)

	locked, err := db.migrationLockHeld(ctx)
	if err != nil {
		return status, ez.Wrap(err)
	}

	status.Locked = locked

	return status, nil
}

// migrationLockHeld - Reports whether bun's migration lock is held, by
// reading the locks table passively: acquiring the lock to find out would
// abort a concurrently starting migration, and would misread database
// errors as "locked"
func (db *DB) migrationLockHeld(ctx context.Context) (bool, error) {
	var count int

	// The locks table holds one row per migrations table, so only our own
	// registry's lock counts — a custom migrator with its own table must
	// not make this schema appear locked
	err := db.NewRaw(
		"SELECT count(*) FROM ? WHERE table_name = ?",
		bun.Ident(bunMigrationLocksTable),
		bunMigrationsTable,
	).Scan(ctx, &count)
	if err != nil {
		return false, ez.Wrap(err)
	}

	return count > 0, nil
}

// InitSchema - Brings the database schema up to date, on any dialect.
//
// The schema has two cooperating sources of truth (see MIGRATIONS.md): the
// model structs describe the complete current schema, while migrations
// describe deltas for databases created by older releases.
//
// A database with no recorded migrations is treated as matching the current
// structs: tables are created from them (existing tables are left untouched)
// and every registered migration is recorded as applied without running,
// atomically. If such a database actually holds an older schema, reconcile
// it manually before adopting InitSchema. A database with recorded
// migrations only runs the pending ones, holding bun's migration lock; a
// concurrent InitSchema fails fast instead of waiting. Callers are
// responsible for not running InitSchema concurrently with code that
// assumes a settled schema — pair it with VerifySchema at boot.
//
// Renaming an applied migration is refused: it looks missing under its old
// name and pending under its new one, and running it would replay its
// changes. After deliberately squashing the registry, remove the stale
// records with PruneMigrationRecords.
func (db *DB) InitSchema(models []interface{}, migrations *migrate.Migrations) error {
	ctx := context.Background()

	// An empty registry goes through the same path so an emptied-out squash
	// still surfaces its orphaned history instead of silently no-opping
	if migrations == nil {
		migrations = migrate.NewMigrations()
	}

	// WithUpsert makes Init add a unique index on the migration name, which
	// turns a concurrent bootstrap into a clean constraint failure for the
	// loser instead of duplicated migration records
	migrator := migrate.NewMigrator(db.DB, migrations, migrate.WithUpsert(true))
	err := migrator.Init(ctx)
	if err != nil {
		return ez.Wrap(err)
	}

	applied, err := migrator.AppliedMigrations(ctx)
	if err != nil {
		return ez.Wrap(err)
	}

	if len(applied) == 0 {
		err = db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			return createAndBaseline(ctx, tx, models, migrations)
		})
		if err != nil {
			return ez.Wrap(err)
		}

		// With an empty registry there is no way to tell a first boot from
		// a repeat one, so only a real baseline gets announced
		if len(migrations.Sorted()) > 0 {
			log.Info().
				Int("Migrations", len(migrations.Sorted())).
				Msg("Created fresh schema and baselined migrations")
		}

		return nil
	}

	var missing migrate.MigrationSlice
	missing, err = migrator.MissingMigrations(ctx)
	if err != nil {
		return ez.Wrap(err)
	}

	pending := countPendingMigrations(applied, migrations)

	if len(missing) > 0 {
		// Recorded-but-unregistered migrations alongside unknown pending
		// ones is the signature of a renamed migration, which would replay
		// its changes under the new name
		if pending > 0 {
			msg := fmt.Sprintf(
				"%d applied migrations are missing from the registry while %d unknown migrations are pending, which looks like renamed migrations that would replay. If the registry was deliberately squashed, run PruneMigrationRecords first",
				len(missing),
				pending,
			)
			return ez.New(ez.ECONFLICT, msg, nil)
		}

		log.Warn().
			Str("Migrations", missing.String()).
			Msg("Applied migrations are missing from the registry; run PruneMigrationRecords to clean up after a deliberate squash")
	}

	if pending == 0 {
		// Records alone cannot distinguish "current" from "a migration was
		// recorded but crashed mid-execution" — only the held lock can
		var locked bool
		locked, err = db.migrationLockHeld(ctx)
		if err != nil {
			return ez.Wrap(err)
		}

		if locked {
			msg := "The schema looks current but the migration lock is held: a migration is running, or a previous one crashed mid-flight and the schema may be incomplete. Inspect it before clearing the lock"
			return ez.New(ez.ECONFLICT, msg, nil)
		}

		return nil
	}

	// The lock prevents two concurrent runs from marking the same
	// migration and rolling back each other's groups; the loser fails fast
	err = migrator.Lock(ctx)
	if err != nil {
		msg := "Another migration is already running. If none is, a previous run crashed mid-migration: inspect the schema and delete the stale row from bun's migration locks table"
		return ez.New(ez.ECONFLICT, msg, err)
	}

	_, err = migrator.Migrate(ctx)
	if err != nil {
		log.Error().Err(err).Msg("Failed to apply migration")

		rollbackErr := db.RollbackLastMigration(migrations)
		if rollbackErr != nil {
			// The schema state is now unknown: the lock stays held as the
			// tombstone so later boots refuse instead of trusting records
			// the failed rollback may have left behind
			log.Error().
				Err(rollbackErr).
				Msg("Rollback failed, keeping the migration lock held for inspection")

			msg := "A migration failed and its rollback also failed, so the schema may be partially migrated. The migration lock was left held; inspect the schema before clearing it"
			return ez.New(ez.EINTERNAL, msg, err)
		}

		// Rolled back cleanly: the schema is back at its previous version,
		// so retrying is safe and needs no manual lock cleanup
		releaseMigrationLock(ctx, migrator)
		return ez.Wrap(err)
	}

	// A failed release would leave a stale lock that blocks later runs, so
	// it must surface
	err = migrator.Unlock(ctx)
	if err != nil {
		return ez.Wrap(err)
	}

	return nil
}

// PruneMigrationRecords - Deletes recorded migrations that are no longer in
// the registry. Run it once after deliberately squashing or removing old
// migrations, so migrations added later are not mistaken for renames.
//
// Only prune after every instance runs a release without the removed
// migrations: an older binary that still registers them would see the
// pruned records as pending and replay them.
func (db *DB) PruneMigrationRecords(migrations *migrate.Migrations) error {
	ctx := context.Background()

	// A nil registry would read as "every recorded migration is missing"
	// and delete the entire history, so it must be explicit
	if migrations == nil {
		return ez.New(ez.EINVALID, "A migration registry is required to prune against", nil)
	}

	migrator := migrate.NewMigrator(db.DB, migrations, migrate.WithUpsert(true))
	err := migrator.Init(ctx)
	if err != nil {
		return ez.Wrap(err)
	}

	var missing migrate.MigrationSlice
	missing, err = migrator.MissingMigrations(ctx)
	if err != nil {
		return ez.Wrap(err)
	}

	if len(missing) == 0 {
		return nil
	}

	for i := range missing {
		err = migrator.MarkUnapplied(ctx, &missing[i])
		if err != nil {
			return ez.Wrap(err)
		}
	}

	log.Info().
		Str("Migrations", missing.String()).
		Msg("Pruned migration records that are no longer in the registry")

	return nil
}

// releaseMigrationLock - Best-effort unlock for paths that already carry an
// error; a failure here only gets logged
func releaseMigrationLock(ctx context.Context, migrator *migrate.Migrator) {
	err := migrator.Unlock(ctx)
	if err != nil {
		log.Error().Err(err).Msg("Failed to release the migration lock")
	}
}

// countPendingMigrations - Counts registered migrations that have no applied
// record
func countPendingMigrations(applied migrate.MigrationSlice, migrations *migrate.Migrations) int {
	appliedNames := make(map[string]bool, len(applied))
	for i := range applied {
		appliedNames[applied[i].Name] = true
	}

	pending := 0
	sorted := migrations.Sorted()
	for i := range sorted {
		if !appliedNames[sorted[i].Name] {
			pending++
		}
	}

	return pending
}

// createAndBaseline - Creates the schema from the model structs and records
// every migration as applied, for use inside the bootstrap transaction
func createAndBaseline(ctx context.Context, tx bun.Tx, models []interface{}, migrations *migrate.Migrations) error {
	err := createTables(ctx, tx, models)
	if err != nil {
		return err
	}

	return markMigrationsApplied(ctx, tx, migrations)
}
