package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"

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

	statementTimeout := DEFAULT_STATEMENT_TIMEOUT
	if cfg.StatementTimeout != 0 {
		statementTimeout = cfg.StatementTimeout
	}

	dialTimeout := DEFAULT_DIAL_TIMEOUT
	if cfg.DialTimeout != 0 {
		dialTimeout = cfg.DialTimeout
	}

	readTimeout := DEFAULT_READ_TIMEOUT
	if cfg.ReadTimeout != 0 {
		readTimeout = cfg.ReadTimeout
	}

	writeTimeout := DEFAULT_WRITE_TIMEOUT
	if cfg.WriteTimeout != 0 {
		writeTimeout = cfg.WriteTimeout
	}

	log.Info().
		Str("Host", cfg.Host).
		Str("Username", cfg.Username).
		Str("Database", cfg.Database).
		Bool("SSL", cfg.SSL).
		Bool("Verbose", cfg.Verbose).
		Int("Statement Timeout", statementTimeout).
		Msg("Connecting to Postgres Database")

	dsn := fmt.Sprintf(
		"postgres://%s:%s@%s/%s?sslmode=%s",
		cfg.Username,
		cfg.Password,
		cfg.Host,
		cfg.Database,
		sslmode,
	)

	sqldb := sql.OpenDB(pgdriver.NewConnector(
		pgdriver.WithDSN(dsn),
		pgdriver.WithDialTimeout(time.Duration(dialTimeout)*time.Millisecond),
		pgdriver.WithReadTimeout(time.Duration(readTimeout)*time.Millisecond),
		pgdriver.WithWriteTimeout(time.Duration(writeTimeout)*time.Millisecond),
		pgdriver.WithConnParams(map[string]interface{}{
			"statement_timeout": strconv.Itoa(statementTimeout),
		}),
	))
	db := bun.NewDB(sqldb, pgdialect.New())

	ctx := context.Background()

	_, err := db.ExecContext(ctx, "SELECT 1")
	if err != nil {
		return nil, ez.Wrap(op, err)
	}

	return &relational.DB{DB: db}, nil
}
