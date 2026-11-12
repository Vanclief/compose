// Package pgtest is test support for apps that test against a real
// PostgreSQL: it creates a uniquely named database per test run, empties it
// between tests, drops it at the end, and sweeps the databases left behind by
// runs that died.
//
// It drops databases on whatever server the config points at, so it only
// drops a database whose name has the stamp Create appends: Sweep only drops
// names that match the exact pattern Create uses for the base name, skips
// databases with an open session, and never forces the drop. Drop is the only
// forced drop, it drops the one database it is given, and it refuses a name
// without the stamp, such as the base name itself.
//
// It needs PostgreSQL 13 or later for DROP DATABASE ... WITH (FORCE).
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/uptrace/bun"
	"github.com/vanclief/compose/drivers/databases/relational"
	"github.com/vanclief/compose/drivers/databases/relational/postgres"
	"github.com/vanclief/ez"
)

// maxBaseNameLength is the longest base name Create and Sweep accept.
// PostgreSQL cuts a database name at 63 bytes without an error, and the stamp
// Create appends is 28 bytes: two underscores, 10 digits of unix seconds and
// 16 hex chars. A longer base would lose part of the stamp, so Sweep could no
// longer recognise the database.
const maxBaseNameLength = 35

// stampPattern matches the end of a name Create generated.
var stampPattern = regexp.MustCompile(`_[0-9]+_[0-9a-f]{16}$`)

func checkBaseName(name string) error {
	if name == "" {
		return ez.New(ez.EINVALID, "The base name of a test database cannot be empty", nil)
	}

	// Measured as Create names it: lowercasing can make a name longer
	if len(strings.ToLower(name)) > maxBaseNameLength {
		return ez.New(ez.EINVALID, fmt.Sprintf("The base name of a test database cannot be longer than %d bytes", maxBaseNameLength), nil)
	}

	return nil
}

// Create makes a new database for one test run and returns the config that
// points at it. cfg.Database is the base name, at most 35 bytes; the database
// is named <base>_<unix seconds>_<16 hex chars>, lowercased, so Sweep can
// recognise it and tell its age.
//
// synchronous_commit is turned off for it, so no session on it waits for its
// WAL flush on commit. The database is thrown away after the run, so losing
// the last commits on a crash costs nothing, while the per-test fixtures and
// truncates stop paying an fsync each. When that fails the database is dropped
// before the error is returned.
//
// Create, Drop and Sweep run their own statements with a statement timeout of
// at least the default of the postgres package, and a read timeout longer
// than that statement timeout: the timeouts of cfg are for the queries of the
// tests, and CREATE and DROP DATABASE can outlast short ones on a busy server.
// A statement or read timeout of cfg that is longer is kept, the dial and
// write timeouts are used as given, and the config Create returns carries the
// timeouts of cfg unchanged.
//
// The read timeout also bounds the wait for the server's first answer, so a
// server that accepts the connection and then says nothing is waited on for
// that read timeout, 35 seconds with the defaults, and not for the read
// timeout of cfg. One that does not accept the connection is given up on
// after the dial timeout of cfg.
func Create(cfg postgres.ConnectionConfig) (postgres.ConnectionConfig, error) {
	err := checkBaseName(cfg.Database)
	if err != nil {
		return postgres.ConnectionConfig{}, ez.Wrap(err)
	}

	randomBytes := make([]byte, 8)
	_, err = rand.Read(randomBytes)
	if err != nil {
		return postgres.ConnectionConfig{}, ez.New(ez.EINTERNAL, "Generate test database name", err)
	}

	cfg.Database = strings.ToLower(fmt.Sprintf("%s_%d_%s", cfg.Database, time.Now().Unix(), hex.EncodeToString(randomBytes)))

	err = postgres.CreateDatabase(withAdminTimeouts(cfg))
	if err != nil {
		return postgres.ConnectionConfig{}, ez.Wrap(err)
	}

	err = disableSynchronousCommit(cfg)
	if err != nil {
		Drop(cfg) // nolint:errcheck // The synchronous_commit error is the one that matters
		return postgres.ConnectionConfig{}, ez.Wrap(err)
	}

	return cfg, nil
}

// Drop drops the database cfg.Database, ending any session still connected to
// it. Dropping a database that does not exist is not an error. It is the only
// forced drop in the package, so it takes the config Create returned and
// refuses a name that does not end in Create's stamp: passing the base config
// by mistake must not drop the database of that name. See Create for the
// timeouts the drop runs with.
func Drop(cfg postgres.ConnectionConfig) error {
	name := cfg.Database

	if !stampPattern.MatchString(name) {
		return ez.New(ez.EINVALID, "Drop only drops a database Create named, pass the config Create returned", nil)
	}

	return onAdminDatabase(cfg, func(db *relational.DB) error {
		_, err := db.ExecContext(context.Background(), "DROP DATABASE IF EXISTS ? WITH (FORCE)", bun.Ident(name))
		if err != nil {
			return ez.New(ez.EINTERNAL, "Drop test database", err)
		}

		return nil
	})
}

// Sweep drops the databases Create left behind under the base name
// cfg.Database that are older than maxAge and have no open session. Such a
// database was left by a run that died before dropping it: interrupted,
// killed by -timeout, or failed to drop.
//
// Only names that match the exact pattern Create generates for the base name
// are considered. A database with an open session still belongs to a live
// run, such as a test paused in a debugger, so it is skipped, and the drop is
// not forced: if a session connects after the listing, PostgreSQL refuses the
// drop instead of terminating it. A failed drop does not stop the others, but
// the sweep stops once ctx is done and leaves the rest for a later run. The
// error returned holds what failed, joined with errors.Join.
//
// See Create for the timeouts the sweep runs with. Connecting to the server
// is not bound by ctx, only by those timeouts. A ctx deadline earlier than the
// statement timeout cuts the wait for a drop short, and the server may still
// finish that drop after Sweep has reported it as failed.
func Sweep(ctx context.Context, cfg postgres.ConnectionConfig, maxAge time.Duration) error {
	err := checkBaseName(cfg.Database)
	if err != nil {
		return ez.Wrap(err)
	}

	baseName := strings.ToLower(cfg.Database)
	pattern := regexp.MustCompile(`^` + regexp.QuoteMeta(baseName) + `_([0-9]+)_[0-9a-f]{16}$`)

	return onAdminDatabase(cfg, func(db *relational.DB) error {
		var names []string

		query := `SELECT d.datname FROM pg_database d
			WHERE d.datname ~ ?
			AND NOT EXISTS (SELECT 1 FROM pg_stat_activity a WHERE a.datid = d.oid)`

		err := db.NewRaw(query, pattern.String()).Scan(ctx, &names)
		if err != nil {
			return ez.New(ez.EINTERNAL, "List test databases", err)
		}

		return dropStale(ctx, db, pattern, names, time.Now().Add(-maxAge).Unix())
	})
}

// dropStale drops the databases in names that match pattern and were created
// at or before cutoff, in unix seconds. It stops once ctx is done.
func dropStale(ctx context.Context, db *relational.DB, pattern *regexp.Regexp, names []string, cutoff int64) error {
	var failed []error

	for _, name := range names {
		if ctx.Err() != nil {
			failed = append(failed, ez.New(ez.EINTERNAL, "Stale test database sweep ran out of time", ctx.Err()))
			break
		}

		// Checked again in Go so a difference between the PostgreSQL and Go
		// regexp dialects can never let another name through
		match := pattern.FindStringSubmatch(name)
		if match == nil {
			continue
		}

		createdAt, err := strconv.ParseInt(match[1], 10, 64)
		if err != nil {
			failed = append(failed, ez.New(ez.EINTERNAL, fmt.Sprintf("Parse creation time of test database %s", name), err))
			continue
		}

		if createdAt > cutoff {
			continue
		}

		_, err = db.ExecContext(ctx, "DROP DATABASE IF EXISTS ?", bun.Ident(name))
		if err != nil {
			failed = append(failed, ez.New(ez.EINTERNAL, fmt.Sprintf("Drop stale test database %s", name), err))
		}
	}

	return errors.Join(failed...)
}

// Truncate empties every table in the public schema and restarts their
// sequences. bun's migration ledger (bun_migrations, bun_migration_locks) is
// kept: an empty bun_migrations lacks the baseline floor row, and the next
// boot on this database would refuse it. A table that belongs to an extension
// is kept too, since its rows are the extension's and not test data. The
// truncate cascades, so a table outside that set with a foreign key to one of
// the emptied tables is emptied with them.
func Truncate(ctx context.Context, db *relational.DB) error {
	if db == nil {
		return ez.New(ez.EINVALID, "Database is nil", nil)
	}

	var tables []string

	// format's %I quotes each name in PostgreSQL itself, so any table name is
	// safe to put in the statement below
	err := db.NewRaw(`
		SELECT format('%I.%I', n.nspname, c.relname)
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public'
		AND c.relkind IN ('r', 'p')
		AND c.relname NOT IN ('bun_migrations', 'bun_migration_locks')
		AND NOT EXISTS (
			SELECT 1 FROM pg_depend d
			WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype = 'e'
		)
	`).Scan(ctx, &tables)
	if err != nil {
		return ez.New(ez.EINTERNAL, "List tables to truncate", err)
	}

	if len(tables) == 0 {
		return nil
	}

	_, err = db.ExecContext(ctx, "TRUNCATE TABLE ? RESTART IDENTITY CASCADE", bun.Safe(strings.Join(tables, ", ")))
	if err != nil {
		return ez.New(ez.EINTERNAL, "Truncate tables", err)
	}

	return nil
}

// disableSynchronousCommit turns synchronous_commit off for every new session
// on the database cfg.Database. See Create for why that is safe.
func disableSynchronousCommit(cfg postgres.ConnectionConfig) error {
	name := cfg.Database

	return onAdminDatabase(cfg, func(db *relational.DB) error {
		_, err := db.ExecContext(context.Background(), "ALTER DATABASE ? SET synchronous_commit = off", bun.Ident(name))
		if err != nil {
			return ez.New(ez.EINTERNAL, "Disable synchronous commit on test database", err)
		}

		return nil
	})
}

// adminReadMargin is how much longer than the statement timeout, in
// milliseconds, an admin connection waits for an answer.
const adminReadMargin = 5 * 1000

// withAdminTimeouts returns cfg with the statement timeout raised to the
// default of the postgres package when it is shorter, and the read timeout
// raised to adminReadMargin past that statement timeout when it is shorter.
// See Create for why. The read timeout is the longer one so the server ends a
// statement that runs out of time and says so; were the connection to give up
// first, the statement would go on without it, and a database could be
// created or dropped after the call reported a failure.
func withAdminTimeouts(cfg postgres.ConnectionConfig) postgres.ConnectionConfig {
	cfg.StatementTimeout = max(cfg.StatementTimeout, postgres.DEFAULT_STATEMENT_TIMEOUT)
	cfg.ReadTimeout = max(cfg.ReadTimeout, cfg.StatementTimeout+adminReadMargin)

	return cfg
}

// onAdminDatabase runs fn on a connection to the postgres database of the
// server cfg points at, opened with the timeouts of withAdminTimeouts, then
// closes it. A close error is returned only when fn succeeded.
func onAdminDatabase(cfg postgres.ConnectionConfig, fn func(db *relational.DB) error) error {
	cfg = withAdminTimeouts(cfg)
	cfg.Database = "postgres" // We need to connect to the default database

	db, err := postgres.ConnectToDatabase(&cfg)
	if err != nil {
		return ez.Wrap(err)
	}

	err = fn(db)
	if err != nil {
		db.Close() // nolint:errcheck // The operation error is the one that matters
		return ez.Wrap(err)
	}

	err = db.Close()
	if err != nil {
		return ez.New(ez.EINTERNAL, "Close admin database connection", err)
	}

	return nil
}
