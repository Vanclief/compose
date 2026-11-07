package relational

import (
	"context"

	"github.com/rs/zerolog/log"
	"github.com/uptrace/bun/migrate"
	"github.com/vanclief/ez"
)

// RunMigrations - Executes all pending migrations. Each one is marked as
// applied only after its Up succeeds, so a failed migration stays pending and
// the next start retries it: every migration must be safe to run twice.
// Rolling back is an explicit call to RollbackLastMigration.
func (db *DB) RunMigrations(migrations *migrate.Migrations) error {
	ctx := context.Background()

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
// migration is applied.
func (db *DB) RollbackLastMigration(migrations *migrate.Migrations) error {
	ctx := context.Background()

	if len(migrations.Sorted()) == 0 {
		log.Info().Msg("No migrations to roll back")
		return nil
	}

	migrator := migrate.NewMigrator(db.migrationDB(), migrations, migrate.WithMarkAppliedOnSuccess(true))
	err := migrator.Init(ctx)
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
