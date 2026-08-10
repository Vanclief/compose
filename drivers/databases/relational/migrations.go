package relational

import (
	"context"

	"github.com/rs/zerolog/log"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/migrate"
	"github.com/vanclief/ez"
)

// RunMigrations - Executes all pending migrations
func (db *DB) RunMigrations(migrations *migrate.Migrations) error {
	ctx := context.Background()

	if migrations == nil || len(migrations.Sorted()) == 0 {
		log.Info().Msg("No pending migrations to run")
		return nil
	}

	migrator := migrate.NewMigrator(db.DB, migrations)
	err := migrator.Init(ctx)
	if err != nil {
		return ez.Wrap(err)
	}

	group, err := migrator.Migrate(ctx)
	if err != nil {
		log.Error().Err(err).Msg("Failed to apply migration")

		// If the migration was not successful, we rollback the migration
		rollbackErr := db.RollbackLastMigration(migrations)
		if rollbackErr != nil {
			log.Error().Err(rollbackErr).Msg("Failed to rollback migration")
		}
		return ez.Wrap(err)
	}

	if group.IsZero() {
		log.Info().Msg("No pending migrations to run")
		return nil
	}

	for _, migration := range group.Migrations {
		log.Warn().
			Str("ID", migration.Name).
			Str("Name", migration.Comment).
			Msg("Executed migration")
	}

	return nil
}

// BaselineMigrations - Records every registered migration as applied without
// executing it, in a single transaction. Meant for freshly created schemas
// (e.g. from CreateTables) that already include the cumulative result of all
// migrations.
func (db *DB) BaselineMigrations(migrations *migrate.Migrations) error {
	ctx := context.Background()

	if migrations == nil || len(migrations.Sorted()) == 0 {
		return nil
	}

	migrator := migrate.NewMigrator(db.DB, migrations, migrate.WithUpsert(true))
	err := migrator.Init(ctx)
	if err != nil {
		return ez.Wrap(err)
	}

	err = db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		return markMigrationsApplied(ctx, tx, migrations)
	})
	if err != nil {
		return ez.Wrap(err)
	}

	sorted := migrations.Sorted()
	if len(sorted) > 0 {
		log.Info().
			Int("Count", len(sorted)).
			Msg("Marked migrations as applied without executing them")
	}

	return nil
}

// markMigrationsApplied - Inserts a record for every registered migration
// into bun's migrations table without executing them
func markMigrationsApplied(ctx context.Context, idb bun.IDB, migrations *migrate.Migrations) error {
	sorted := migrations.Sorted()
	for i := range sorted {
		migration := sorted[i]

		_, err := idb.NewInsert().
			Model(&migration).
			ModelTableExpr(bunMigrationsTable).
			Exec(ctx)
		if err != nil {
			return ez.Wrap(err)
		}
	}

	return nil
}

// RollbackLastMigration - Rollbacks the last migration
func (db *DB) RollbackLastMigration(migrations *migrate.Migrations) error {
	ctx := context.Background()

	migrator := migrate.NewMigrator(db.DB, migrations)
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
