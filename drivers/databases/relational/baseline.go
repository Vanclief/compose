package relational

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/rs/zerolog/log"
	"github.com/uptrace/bun/dialect"
	"github.com/uptrace/bun/migrate"
	"github.com/vanclief/ez"
)

// Baseline - A schema snapshot that stands in for every migration up to its floor.
type Baseline struct {
	// SQL is the output of pg_dump --schema-only of a database that has run
	// every migration up to and including Floor. Empty when the app has no baseline yet.
	SQL string
	// Floor is the highest name among the deleted migration files, as bun stores
	// it in the name column (the digits before the first underscore, not the
	// file name); SQL holds it and everything below it. Every migration named
	// at or below it must have its file deleted.
	Floor string
}

// RunMigrationsFromBaseline - Loads the baseline into a fresh database, or
// checks an existing one is not behind it, then runs the pending migrations.
// It holds the migration lock from the check to the last migration, like
// RunMigrations.
func (db *DB) RunMigrationsFromBaseline(baseline Baseline, migrations *migrate.Migrations) error {
	ctx := context.Background()

	if db.Dialect().Name() != dialect.PG {
		return ez.New(ez.ENOTIMPLEMENTED, "Schema baselines are only supported by PostgreSQL", nil)
	}

	if baseline.SQL == "" && baseline.Floor != "" {
		return ez.New(ez.EINVALID, "Baseline has a floor but no SQL", nil)
	}

	if baseline.SQL != "" && baseline.Floor == "" {
		return ez.New(ez.EINVALID, "Baseline has SQL but no floor", nil)
	}

	// A migration at or below the floor must have no file, so a rollback can
	// never reach into the baseline. Plain string comparison is right because
	// bun itself orders migrations with strings.Compare on their names.
	if baseline.Floor != "" {
		for _, migration := range migrations.Sorted() {
			if migration.Name <= baseline.Floor {
				msg := fmt.Sprintf("Migration %s is at or below the baseline floor %s: delete its file if the baseline already holds it, or rename it above the floor if it was merged after the checkpoint", migration.String(), baseline.Floor)
				return ez.New(ez.ECONFLICT, msg, nil)
			}
		}
	}

	// One lock covers the fresh check, the load and the migrations, so a second
	// instance reads the ledger only after the first has loaded and migrated
	unlock, err := db.lockMigrations(ctx)
	if err != nil {
		return ez.Wrap(err)
	}
	defer unlock()

	// The ledger is read before migrate.Migrator.Init runs, because Init would
	// create the bun_migrations table the baseline creates itself. Only a
	// missing table counts as fresh: the dump creates the bun tables without
	// IF NOT EXISTS, so it cannot load over an existing empty ledger.
	var fresh bool
	err = db.NewRaw("SELECT to_regclass('bun_migrations') IS NULL").Scan(ctx, &fresh)
	if err != nil {
		return ez.Wrap(err)
	}

	if fresh && baseline.SQL != "" {
		err = db.loadBaseline(ctx, baseline)
		if err != nil {
			return ez.Wrap(err)
		}

		log.Info().Str("Floor", baseline.Floor).Msg("Loaded schema baseline")
	}

	if !fresh {
		err = db.checkBaseline(ctx, baseline)
		if err != nil {
			return ez.Wrap(err)
		}
	}

	err = db.runMigrations(ctx, migrations)
	if err != nil {
		return ez.Wrap(err)
	}

	return nil
}

// loadBaseline - Runs the baseline SQL and marks its floor migration as
// applied, in one transaction on a connection that is discarded afterwards.
func (db *DB) loadBaseline(ctx context.Context, baseline Baseline) error {
	// The raw *sql.DB keeps bun's query formatter away from the dump, and the
	// migration pool keeps the main pool's socket read deadline away from a
	// large baseline; the dump turns statement_timeout off itself.
	conn, err := db.migrationDB().DB.Conn(ctx)
	if err != nil {
		return ez.Wrap(err)
	}

	// The dump empties search_path and sets lock_timeout and statement_timeout
	// to 0 for its session. Discarding the connection instead of returning it
	// to the pool stops the Migrate that follows from reusing that session.
	defer discardConn(conn)

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return ez.Wrap(err)
	}
	defer tx.Rollback() // nolint:errcheck // No-op after Commit

	_, err = tx.ExecContext(ctx, stripRestrictFraming(baseline.SQL))
	if err != nil {
		return loadError(err)
	}

	// Look the ledger up the way bun will, not through the dump's empty search_path
	_, err = tx.ExecContext(ctx, "RESET search_path")
	if err != nil {
		return ez.Wrap(err)
	}

	var missing bool
	err = tx.QueryRowContext(ctx, "SELECT to_regclass('bun_migrations') IS NULL").Scan(&missing)
	if err != nil {
		return ez.Wrap(err)
	}

	if missing {
		return ez.New(ez.ECONFLICT, "Baseline SQL does not create the bun_migrations table, dump it without excluding bun tables", nil)
	}

	_, err = tx.ExecContext(ctx, "INSERT INTO bun_migrations (name, group_id, migrated_at) VALUES ($1, 1, now())", baseline.Floor)
	if err != nil {
		return ez.Wrap(err)
	}

	err = tx.Commit()
	if err != nil {
		return ez.Wrap(err)
	}

	return nil
}

// loadError - Gives the two common load failures, an existing object and a
// missing setting or role, a hint and their own code, by SQLSTATE; any other
// error goes through ez.Wrap, which makes a Postgres error EINTERNAL with its
// text in Error().
func loadError(err error) error {
	// pgdriver.Error is matched by its Field method, so SQLite-only builds do
	// not link pgdriver
	var pgErr interface{ Field(byte) string }
	if !errors.As(err, &pgErr) {
		return ez.Wrap(err)
	}

	// The hint goes into the cause as well as the message: with a cause ez
	// prints only the cause, and a boot log prints Error()
	switch pgErr.Field('C') {
	case "42P07", "42710", "42P06", "42723": // duplicate table, object, schema or function
		hint := "Baseline SQL failed to load: a fresh database must have no tables or other objects of the dump before the load"
		return ez.New(ez.ECONFLICT, hint, fmt.Errorf("%s: %w", hint, err))
	case "42704": // undefined object: a SET only a newer server knows, a role, a collation
		hint := "Baseline SQL failed to load: the dump names a setting, role or other object this server does not have; for a setting, dump with the pg_dump of the dumped server's major and load it on that major or newer"
		return ez.New(ez.EINVALID, hint, fmt.Errorf("%s: %w", hint, err))
	}

	return ez.Wrap(err)
}

// checkBaseline - Fails when the floor migration has not run on the database,
// since its file is gone and nothing could run it anymore.
func (db *DB) checkBaseline(ctx context.Context, baseline Baseline) error {
	if baseline.Floor == "" {
		return nil
	}

	var applied bool
	err := db.NewRaw("SELECT EXISTS (SELECT 1 FROM bun_migrations WHERE name = ?)", baseline.Floor).Scan(ctx, &applied)
	if err != nil {
		return ez.Wrap(err)
	}

	if !applied {
		msg := fmt.Sprintf("Database is behind the baseline floor %s: that migration has not run and its file is gone", baseline.Floor)
		return ez.New(ez.ECONFLICT, msg, nil)
	}

	log.Info().Str("Floor", baseline.Floor).Msg("Database is not behind the schema baseline")

	return nil
}

// restrictRE matches the psql meta-command pg_dump 17.6, 16.10, 15.14, 14.19,
// 13.22 and later put before the first SQL statement, with its random key.
var restrictRE = regexp.MustCompile(`^\\restrict (\S+)$`)

// stripRestrictFraming - Removes the \restrict line pg_dump opens the dump
// with and the \unrestrict line with the same key that closes it, which are
// not SQL. The opening line counts only when nothing but blank lines and
// comments come before it, and the closing line only when nothing but blank
// lines and comments come after it, so a dump without the frame is returned
// as is even when a dollar-quoted function body holds a line of that shape.
// A line is matched with its trailing carriage return trimmed, so a CRLF
// checkout of the dump is unframed too; every other byte is kept.
func stripRestrictFraming(dump string) string {
	lines := strings.Split(dump, "\n")

	first := 0
	for first < len(lines) && isFrameNeighbour(lines[first]) {
		first++
	}

	if first == len(lines) {
		return dump
	}

	match := restrictRE.FindStringSubmatch(strings.TrimSuffix(lines[first], "\r"))
	if match == nil {
		return dump
	}

	last := len(lines) - 1
	for last > first && isFrameNeighbour(lines[last]) {
		last--
	}

	if strings.TrimSuffix(lines[last], "\r") != `\unrestrict `+match[1] {
		return dump
	}

	kept := make([]string, 0, len(lines))
	kept = append(kept, lines[:first]...)
	kept = append(kept, lines[first+1:last]...)
	kept = append(kept, lines[last+1:]...)

	return strings.Join(kept, "\n")
}

// isFrameNeighbour - True for the blank and comment lines pg_dump writes
// outside the frame, the only lines allowed there
func isFrameNeighbour(line string) bool {
	line = strings.TrimSuffix(line, "\r")

	return line == "" || strings.HasPrefix(line, "--")
}
