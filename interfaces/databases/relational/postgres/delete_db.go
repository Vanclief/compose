package postgres

import (
	"context"
	"fmt"

	"github.com/vanclief/ez"
)

// DeleteDatabase - Deletes an existing database with the given configuration
func DeleteDatabase(cfg ConnectionConfig) error {
	const op = "database.DeleteDatabase"

	cfg.Database = "postgres" // We need to connect to the default database

	db, err := ConnectToDatabase(&cfg)
	if err != nil {
		return ez.Wrap(op, err)
	}

	// Create a new database
	ctx := context.Background()

	// Delete the database
	query := fmt.Sprintf("DROP DATABASE %s", cfg.Database)
	_, err = db.ExecContext(ctx, query)
	if err != nil {
		return ez.Wrap(op, err)
	}

	err = db.Close()
	if err != nil {
		return ez.Wrap(op, err)
	}

	return nil
}
