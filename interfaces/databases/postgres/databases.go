package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/vanclief/ez"
)

// CreateDatabase - Creates a new database with the given configuration
func CreateDatabase(cfg ConnectionConfig) error {
	const op = "database.CreateDatabase"

	sslmode := "disable"
	if cfg.SSL {
		sslmode = "require"
	}

	dsn := fmt.Sprintf(
		"postgres://%s:%s@%s/%s?sslmode=%s",
		cfg.Username,
		cfg.Password,
		cfg.Host,
		"postgres", // We need to connect to the default database
		sslmode,
	)

	sqldb := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	db := bun.NewDB(sqldb, pgdialect.New())

	ctx := context.Background()

	// Create a new database
	query := fmt.Sprintf("CREATE DATABASE %s", cfg.Database)
	_, err := db.ExecContext(ctx, query)
	if err != nil {
		return ez.Wrap(op, err)
	}

	err = db.Close()
	if err != nil {
		return ez.Wrap(op, err)
	}

	return nil
}
