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

	migrator := migrate.NewMigrator(db.DB, migrations)
	err := migrator.Init(ctx)
	if err != nil {
		return ez.Wrap(op, err)
	}

	group, err := migrator.Migrate(ctx)
	if err != nil {
		return ez.Wrap(op, err)
	}

	if group.IsZero() {
		log.Info().Msg("No pending migrations to run")
	} else {
		for _, migration := range group.Migrations {
			log.Info().
				Str("ID", migration.Name).
				Str("Name", migration.Comment).
				Msg("Ran migration")
		}
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
	} else {
		for _, migration := range group.Migrations {
			log.Info().
				Str("ID", migration.Name).
				Str("Name", migration.Comment).
				Msg("Rolled back migration")
		}
	}

	return nil
}
