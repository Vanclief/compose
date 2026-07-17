package postgres

import (
	"context"

	"github.com/rs/zerolog/log"
	"github.com/uptrace/bun"
	"github.com/vanclief/ez"
)

// DeleteDatabase - Deletes an existing database with the given configuration
func DeleteDatabase(cfg ConnectionConfig) error {
	databaseName := cfg.Database

	cfg.Database = "postgres" // We need to connect to the default database

	db, err := ConnectToDatabase(&cfg)
	if err != nil {
		return ez.Wrap(err)
	}

	// Create a new database
	ctx := context.Background()
	cfg.Database = databaseName

	// Delete the database
	_, err = db.ExecContext(ctx, "DROP DATABASE ?", bun.Ident(cfg.Database))
	if err != nil {
		return ez.Wrap(err)
	}

	log.Info().
		Str("Host", cfg.Host).
		Str("Username", cfg.Username).
		Str("Database", cfg.Database).
		Bool("SSL", cfg.SSL).
		Bool("Verbose", cfg.Verbose).
		Msg("Deleted Postgres Database")

	err = db.Close()
	if err != nil {
		return ez.Wrap(err)
	}

	return nil
}
