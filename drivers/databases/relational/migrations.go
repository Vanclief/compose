package relational

import (
	"context"

	"github.com/rs/zerolog/log"
	"github.com/uptrace/bun/migrate"
	"github.com/vanclief/ez"
)

// RunMigrations - Executes all pending migrations
func (db *DB) RunMigrations(migrations *migrate.Migrations) error {
	const op = "database.RunMigrations"

	ctx := context.Background()

	if len(migrations.Sorted()) == 0 {
		log.Info().Msg("No pending migrations to run")
		return nil
	}

	migrator := migrate.NewMigrator(db.DB, migrations)
	err := migrator.Init(ctx)
	if err != nil {
		return ez.Wrap(op, err)
	}

	group, err := migrator.Migrate(ctx)
	if err != nil {
		log.Error().Err(err).Msg("Failed to apply migration")

		// If the migration was not successful, we rollback the migration
		rollbackErr := db.RollbackLastMigration(migrations)
		if rollbackErr != nil {
			log.Error().Err(rollbackErr).Msg("Failed to rollback migration")
		}
		return ez.Wrap(op, err)
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

// RollbackLastMigration - Rollbacks the last migration
func (db *DB) RollbackLastMigration(migrations *migrate.Migrations) error {
	const op = "database.RollbackLastMigration"

	ctx := context.Background()

	migrator := migrate.NewMigrator(db.DB, migrations)
	err := migrator.Init(ctx)
	if err != nil {
		return ez.Wrap(op, err)
	}

	group, err := migrator.Rollback(ctx)
	if err != nil {
		return ez.Wrap(op, err)
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
