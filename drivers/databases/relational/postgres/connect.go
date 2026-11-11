package postgres

import (
	"context"
	"crypto/x509"
	"database/sql"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/vanclief/compose/drivers/databases/relational"
	"github.com/vanclief/ez"
)

// ConnectToDatabase - Creates a new connection to a PSQL database with
// the given configuration.
func ConnectToDatabase(cfg *ConnectionConfig) (*relational.DB, error) {
	if cfg.MaxOpenConns < 0 {
		return nil, ez.New(ez.EINVALID, "Max open connections cannot be negative", nil)
	}
	if cfg.MaxIdleConns < 0 {
		return nil, ez.New(ez.EINVALID, "Max idle connections cannot be negative", nil)
	}
	if cfg.MigrationLockTimeout < 0 {
		return nil, ez.New(ez.EINVALID, "Migration lock timeout cannot be negative", nil)
	}

	sslmode, err := resolveSSLMode(cfg)
	if err != nil {
		return nil, ez.Wrap(err)
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

	lockTimeout := DEFAULT_MIGRATION_LOCK_TIMEOUT
	if cfg.MigrationLockTimeout != 0 {
		lockTimeout = cfg.MigrationLockTimeout
	}

	log.Info().
		Str("Host", cfg.Host).
		Str("Username", cfg.Username).
		Str("Database", cfg.Database).
		Bool("SSL", cfg.SSL).
		Str("SSL Mode", sslmode).
		Bool("Verbose", cfg.Verbose).
		Int("Statement Timeout", statementTimeout).
		Int("Migration Lock Timeout", lockTimeout).
		Int("Max Open Conns", cfg.MaxOpenConns).
		Int("Max Idle Conns", cfg.MaxIdleConns).
		Msg("Connecting to Postgres Database")

	dsn := buildDSN(cfg, sslmode)

	sqldb := sql.OpenDB(newConnector(dsn, dialTimeout, readTimeout, writeTimeout, map[string]interface{}{
		"statement_timeout": strconv.Itoa(statementTimeout),
	}))

	if cfg.MaxOpenConns > 0 {
		sqldb.SetMaxOpenConns(cfg.MaxOpenConns)
	}

	if cfg.MaxIdleConns > 0 {
		sqldb.SetMaxIdleConns(cfg.MaxIdleConns)
	}

	db := bun.NewDB(sqldb, NewDialect())

	// Migrations get their own pool: a read timeout of 0 sets no socket
	// deadline and statement_timeout 0 lets a long CREATE INDEX finish. The
	// connection count is not capped, so a migration that holds a transaction
	// can still query migrator.DB(). Idle connections are kept so a per-row
	// backfill reuses one instead of dialing for every statement, and closed
	// after 30 seconds so the pool holds nothing once migrations are done.
	// client_connection_check_interval (PostgreSQL 14+) makes the server abort
	// the statement of a client that died mid-migration within about 5 seconds,
	// instead of running it and holding its locks until it finishes.
	// The connection holding the migration advisory lock runs no statement, so
	// that check never sees its client go; TCP keepalives every 10 seconds after
	// 30 idle, 3 unanswered, make the server drop the session of a vanished
	// host connected directly, and the lock with it, in about a minute instead
	// of the 2 hours of Linux's default. The server ignores them on a Unix
	// socket and only logs a setting the platform cannot apply.
	// idle_session_timeout (PostgreSQL 14+) is turned off because a database
	// or role default would end that idle session, and release the lock, in
	// the middle of a migration.
	migrationSQLDB := sql.OpenDB(newConnector(dsn, dialTimeout, 0, writeTimeout, map[string]interface{}{
		"statement_timeout":                "0",
		"lock_timeout":                     strconv.Itoa(lockTimeout),
		"client_connection_check_interval": "5000",
		"tcp_keepalives_idle":              "30",
		"tcp_keepalives_interval":          "10",
		"tcp_keepalives_count":             "3",
		"idle_session_timeout":             "0",
	}))
	migrationSQLDB.SetConnMaxIdleTime(30 * time.Second)

	// The dialect is shared so both pools use one model registry, and models
	// registered on the main pool resolve in migrations too
	migrationDB := bun.NewDB(migrationSQLDB, db.Dialect())

	ctx := context.Background()

	_, err = db.ExecContext(ctx, "SELECT 1")
	if err != nil {
		migrationDB.Close() // nolint:errcheck // The connection error is the one that matters
		db.Close()          // nolint:errcheck // The connection error is the one that matters
		return nil, ez.Wrap(err)
	}

	return &relational.DB{DB: db, MigrationDB: migrationDB}, nil
}

// newConnector - Builds a pgdriver connector. Timeouts are in milliseconds and
// a timeout of 0 sets no deadline.
func newConnector(dsn string, dialTimeout, readTimeout, writeTimeout int, params map[string]interface{}) *pgdriver.Connector {
	return pgdriver.NewConnector(
		pgdriver.WithDSN(dsn),
		pgdriver.WithDialTimeout(time.Duration(dialTimeout)*time.Millisecond),
		pgdriver.WithReadTimeout(time.Duration(readTimeout)*time.Millisecond),
		pgdriver.WithWriteTimeout(time.Duration(writeTimeout)*time.Millisecond),
		pgdriver.WithConnParams(params),
	)
}

// resolveSSLMode returns the sslmode for cfg. SSLMode wins when set; otherwise
// SSL=true maps to "require" and SSL=false to "disable". The verify modes use
// the CA PEM in SSLRootCert when set, and the system CA pool otherwise.
func resolveSSLMode(cfg *ConnectionConfig) (string, error) {
	if cfg.SSLMode == "" {
		if cfg.SSL {
			return "require", nil
		}

		return "disable", nil
	}

	switch cfg.SSLMode {
	case "disable", "require":
		return cfg.SSLMode, nil
	case "verify-ca", "verify-full":
	default:
		return "", ez.New(ez.EINVALID, "sslMode must be one of disable, require, verify-ca or verify-full", nil)
	}

	// Without a CA file pgdriver verifies against the system pool
	if cfg.SSLRootCert == "" {
		return cfg.SSLMode, nil
	}

	// pgdriver reads the CA itself but panics when it cannot, so check it here
	// and fail with an error instead.
	rawCA, err := os.ReadFile(cfg.SSLRootCert)
	if err != nil {
		return "", ez.New(ez.EINVALID, "Could not read sslRootCert", err)
	}

	if !x509.NewCertPool().AppendCertsFromPEM(rawCA) {
		return "", ez.New(ez.EINVALID, "sslRootCert contains no valid PEM certificates", nil)
	}

	return cfg.SSLMode, nil
}

// buildDSN builds the pgdriver DSN with every component escaped, so
// credentials may contain characters such as #, /, ? or %.
func buildDSN(cfg *ConnectionConfig, sslmode string) string {
	query := url.Values{"sslmode": {sslmode}}
	if cfg.SSLRootCert != "" && (sslmode == "verify-ca" || sslmode == "verify-full") {
		query.Set("sslrootcert", cfg.SSLRootCert)
	}

	dsn := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(cfg.Username, cfg.Password),
		Host:     cfg.Host,
		Path:     "/" + cfg.Database,
		RawQuery: query.Encode(),
	}

	return dsn.String()
}
