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
	"github.com/uptrace/bun/dialect/pgdialect"
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

	log.Info().
		Str("Host", cfg.Host).
		Str("Username", cfg.Username).
		Str("Database", cfg.Database).
		Bool("SSL", cfg.SSL).
		Str("SSL Mode", sslmode).
		Bool("Verbose", cfg.Verbose).
		Int("Statement Timeout", statementTimeout).
		Int("Max Open Conns", cfg.MaxOpenConns).
		Int("Max Idle Conns", cfg.MaxIdleConns).
		Msg("Connecting to Postgres Database")

	dsn := buildDSN(cfg, sslmode)

	sqldb := sql.OpenDB(pgdriver.NewConnector(
		pgdriver.WithDSN(dsn),
		pgdriver.WithDialTimeout(time.Duration(dialTimeout)*time.Millisecond),
		pgdriver.WithReadTimeout(time.Duration(readTimeout)*time.Millisecond),
		pgdriver.WithWriteTimeout(time.Duration(writeTimeout)*time.Millisecond),
		pgdriver.WithConnParams(map[string]interface{}{
			"statement_timeout": strconv.Itoa(statementTimeout),
		}),
	))

	if cfg.MaxOpenConns > 0 {
		sqldb.SetMaxOpenConns(cfg.MaxOpenConns)
	}

	if cfg.MaxIdleConns > 0 {
		sqldb.SetMaxIdleConns(cfg.MaxIdleConns)
	}

	db := bun.NewDB(sqldb, pgdialect.New())

	ctx := context.Background()

	_, err = db.ExecContext(ctx, "SELECT 1")
	if err != nil {
		db.Close() // nolint:errcheck // The connection error is the one that matters
		return nil, ez.Wrap(err)
	}

	return &relational.DB{DB: db}, nil
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
