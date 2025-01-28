package postgres

import (
	"context"
	"fmt"

	"github.com/rs/zerolog/log"
	"github.com/vanclief/ez"
)

// CreateDatabase - Creates a new database with the given configuration
func CreateDatabase(cfg ConnectionConfig) error {
	const op = "postgres.CreateDatabase"

	databaseName := cfg.Database

	cfg.Database = "postgres" // We need to connect to the default database

	db, err := ConnectToDatabase(&cfg)
	if err != nil {
		return ez.Wrap(op, err)
	}

	// Create a new database
	ctx := context.Background()
	cfg.Database = databaseName

	query := fmt.Sprintf("CREATE DATABASE %s", cfg.Database)
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
		Msg("Created Postgres Database")

	err = db.Close()
	if err != nil {
		return ez.Wrap(op, err)
	}

	return nil
}
