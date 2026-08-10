package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"github.com/rs/zerolog/log"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/sqlitedialect"
	"github.com/vanclief/compose/drivers/databases/relational"
	"github.com/vanclief/ez"

	// Registers the cgo-free "sqlite" database/sql driver
	_ "modernc.org/sqlite"
)

// ConnectToDatabase - Opens a SQLite database file with the given
// configuration, creating the file and its parent directories if they don't
// exist. Connections use WAL journaling with a busy timeout so a writer and
// concurrent readers can share the file.
func ConnectToDatabase(cfg *ConnectionConfig) (*relational.DB, error) {
	if cfg == nil || cfg.Path == "" {
		return nil, ez.New(ez.EINVALID, "A database file path is required", nil)
	}

	busyTimeout := DEFAULT_BUSY_TIMEOUT
	if cfg.BusyTimeout != 0 {
		busyTimeout = cfg.BusyTimeout
	}

	absPath, err := filepath.Abs(cfg.Path)
	if err != nil {
		return nil, ez.Wrap(err)
	}

	err = os.MkdirAll(filepath.Dir(absPath), 0o755)
	if err != nil {
		return nil, ez.Wrap(err)
	}

	log.Info().
		Str("Path", absPath).
		Bool("Verbose", cfg.Verbose).
		Int("Busy Timeout", busyTimeout).
		Msg("Connecting to SQLite Database")

	// The path goes through a URL so characters like '?', '#' or '%' are
	// escaped instead of being misread as DSN parameters. Pragmas are applied
	// to every pooled connection; journal_mode is persistent in the file, the
	// rest are per-connection settings.
	query := url.Values{}
	query.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeout))
	query.Add("_pragma", "journal_mode(WAL)")
	query.Add("_pragma", "foreign_keys(1)")
	query.Add("_pragma", "synchronous(NORMAL)")

	dsn := (&url.URL{Scheme: "file", Path: absPath, RawQuery: query.Encode()}).String()

	sqldb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, ez.Wrap(err)
	}

	db := bun.NewDB(sqldb, sqlitedialect.New())

	ctx := context.Background()

	_, err = db.ExecContext(ctx, "SELECT 1")
	if err != nil {
		db.Close() // nolint:errcheck // The connection error is the one that matters
		return nil, ez.Wrap(err)
	}

	return &relational.DB{DB: db}, nil
}

// ConnectToMemoryDatabase - Opens an in-memory SQLite database, useful for
// tests. The pool is pinned to a single connection because each new
// connection to an in-memory database would otherwise see its own empty
// database.
func ConnectToMemoryDatabase() (*relational.DB, error) {
	sqldb, err := sql.Open("sqlite", "file::memory:?_pragma=foreign_keys(1)")
	if err != nil {
		return nil, ez.Wrap(err)
	}

	sqldb.SetMaxOpenConns(1)
	sqldb.SetMaxIdleConns(1)
	sqldb.SetConnMaxLifetime(0)
	sqldb.SetConnMaxIdleTime(0)

	db := bun.NewDB(sqldb, sqlitedialect.New())

	ctx := context.Background()

	_, err = db.ExecContext(ctx, "SELECT 1")
	if err != nil {
		db.Close() // nolint:errcheck // The connection error is the one that matters
		return nil, ez.Wrap(err)
	}

	return &relational.DB{DB: db}, nil
}
