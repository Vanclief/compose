package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/rs/zerolog/log"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/vanclief/compose/interfaces/databases/relational"
	"github.com/vanclief/ez"
)

// ConnectToDatabase - Creates a new connection to a PSQL database with
// the given configuration.
func ConnectToDatabase(cfg *ConnectionConfig) (*relational.DB, error) {
	const op = "postgres.ConnectToDatabase"

	sslmode := "disable"
	if cfg.SSL {
		sslmode = "require"
	}

	log.Info().
		Str("Host", cfg.Host).
		Str("Username", cfg.Username).
		Str("Database", cfg.Database).
		Bool("SSL", cfg.SSL).
		Bool("Verbose", cfg.Verbose).
		Msg("Connecting to Postgres Database")

	dsn := fmt.Sprintf(
		"postgres://%s:%s@%s/%s?sslmode=%s&timeout=30s",
		cfg.Username,
		cfg.Password,
		cfg.Host,
		cfg.Database,
		sslmode,
	)

	sqldb := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	db := bun.NewDB(sqldb, pgdialect.New())

	ctx := context.Background()

	_, err := db.ExecContext(ctx, "SELECT 1")
	if err != nil {
		return nil, ez.Wrap(op, err)
	}

	return &relational.DB{DB: db}, nil
}
