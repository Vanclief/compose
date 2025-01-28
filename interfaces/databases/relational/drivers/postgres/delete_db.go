package postgres

import (
	"context"
	"fmt"

	"github.com/rs/zerolog/log"
	"github.com/vanclief/ez"
)

// DeleteDatabase - Deletes an existing database with the given configuration
func DeleteDatabase(cfg ConnectionConfig) error {
	const op = "database.DeleteDatabase"

	databaseName := cfg.Database

	cfg.Database = "postgres" // We need to connect to the default database

	db, err := ConnectToDatabase(&cfg)
	if err != nil {
		return ez.Wrap(op, err)
	}

	// Create a new database
	ctx := context.Background()
	cfg.Database = databaseName

	// Delete the database
	query := fmt.Sprintf("DROP DATABASE %s", cfg.Database)
	_, err = db.ExecContext(ctx, query)
	if err != nil {
		return ez.Wrap(op, err)
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
		return ez.Wrap(op, err)
	}

	return nil
}
