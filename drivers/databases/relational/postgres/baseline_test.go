package postgres

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun/migrate"
	"github.com/vanclief/compose/drivers/databases/relational"
	"github.com/vanclief/compose/drivers/databases/relational/sqlite"
	"github.com/vanclief/ez"
)

// baselineConfig - Points at a randomly named database so each test owns its state
func baselineConfig(t *testing.T) ConnectionConfig {
	suffix := make([]byte, 6)
	_, err := rand.Read(suffix)
	require.NoError(t, err)

	return ConnectionConfig{
		Username:     "postgres",
		Password:     "",
		Host:         "localhost:5432",
		Database:     "compose_baseline_" + hex.EncodeToString(suffix),
		MaxOpenConns: 2,
	}
}

// newBaselineDB - Creates and connects to a database that is dropped when the
// test ends. It skips like the suite when PostgreSQL is unreachable.
func newBaselineDB(t *testing.T) (*relational.DB, ConnectionConfig) {
	return connectTestDB(t, baselineConfig(t))
}

// connectTestDB - Like newBaselineDB, for a test that changes the config first
func connectTestDB(t *testing.T, cfg ConnectionConfig) (*relational.DB, ConnectionConfig) {
	err := CreateDatabase(cfg)
	if err != nil {
		if os.Getenv("COMPOSE_TEST_POSTGRES") != "" {
			t.Fatalf("PostgreSQL testing is enabled but the database is unavailable: %v", err)
		}

		t.Skipf("PostgreSQL is not available: %v", err)
	}

	db, err := ConnectToDatabase(&cfg)
	if err != nil {
		DeleteDatabase(cfg) // nolint:errcheck // The connection error is the one that matters
		t.Fatalf("Could not connect to %s: %v", cfg.Database, err)
	}

	t.Cleanup(func() {
		db.Close() // nolint:errcheck
		err := DeleteDatabase(cfg)
		assert.NoError(t, err)
	})

	return db, cfg
}

// dumpBaseline - Builds the baseline SQL from a real pg_dump of a database
// holding things and the bun tables, framed with the \restrict lines pg_dump
// emits since 17.6, 16.10, 15.14, 14.19 and 13.22. It
// skips without pg_dump unless PostgreSQL testing is enabled.
func dumpBaseline(t *testing.T) string {
	_, err := exec.LookPath("pg_dump")
	if err != nil {
		if os.Getenv("COMPOSE_TEST_POSTGRES") != "" {
			t.Fatalf("PostgreSQL testing is enabled but pg_dump is not on PATH: %v", err)
		}

		t.Skip("pg_dump is not on PATH")
	}

	ctx := context.Background()
	scratch, cfg := newBaselineDB(t)

	_, err = scratch.ExecContext(ctx, "CREATE TABLE things (id bigserial PRIMARY KEY, name text NOT NULL)")
	require.NoError(t, err)

	err = migrate.NewMigrator(scratch.DB, migrate.NewMigrations()).Init(ctx)
	require.NoError(t, err)

	var stderr bytes.Buffer
	cmd := exec.Command("pg_dump", "--schema-only", "--no-owner", "--no-privileges",
		"-h", "localhost", "-U", cfg.Username, "-d", cfg.Database)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	require.NoError(t, err, "pg_dump: %s", stderr.String())

	// A pg_dump from before the versions above has no frame and gets one here
	if strings.Contains(string(out), "\n\\restrict ") {
		return string(out)
	}

	return "\\restrict abc\n" + string(out) + "\\unrestrict abc\n"
}

// insideBaselineFrame - Puts sql before the dump's closing \unrestrict line,
// where pg_dump would have written it
func insideBaselineFrame(t *testing.T, dump string, sql string) string {
	at := strings.LastIndex(dump, "\\unrestrict ")
	require.NotEqual(t, -1, at)

	return dump[:at] + sql + dump[at:]
}

// colorMigrations - The one migration written after the baseline
func colorMigrations() *migrate.Migrations {
	migrations := migrate.NewMigrations()
	migrations.Add(migrate.Migration{
		Name: "20240102_two",
		Up: func(ctx context.Context, migrator *migrate.Migrator, _ *migrate.Migration) error {
			_, err := migrator.DB().ExecContext(ctx, "ALTER TABLE things ADD COLUMN IF NOT EXISTS color text")
			if err != nil {
				return ez.Wrap(err)
			}

			return nil
		},
		Down: func(ctx context.Context, migrator *migrate.Migrator, _ *migrate.Migration) error {
			_, err := migrator.DB().ExecContext(ctx, "ALTER TABLE things DROP COLUMN IF EXISTS color")
			if err != nil {
				return ez.Wrap(err)
			}

			return nil
		},
	})

	return migrations
}

// thing - The model of the dump's things table
type thing struct {
	ID   int64  `bun:",pk,autoincrement"`
	Name string `bun:",notnull"`
}

type ledgerRow struct {
	Name    string `bun:"name"`
	GroupID int64  `bun:"group_id"`
}

func readLedger(t *testing.T, db *relational.DB) []ledgerRow {
	var rows []ledgerRow
	err := db.NewRaw("SELECT name, group_id FROM bun_migrations ORDER BY name").Scan(context.Background(), &rows)
	require.NoError(t, err)

	return rows
}

func tableExists(t *testing.T, db *relational.DB, name string) bool {
	var exists bool
	err := db.NewRaw("SELECT to_regclass(?) IS NOT NULL", name).Scan(context.Background(), &exists)
	require.NoError(t, err)

	return exists
}

func columnExists(t *testing.T, db *relational.DB, table, column string) bool {
	var exists bool
	err := db.NewRaw(
		"SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = ? AND column_name = ?)",
		table, column,
	).Scan(context.Background(), &exists)
	require.NoError(t, err)

	return exists
}

func TestRunMigrationsFromBaselineLoadsFreshDatabase(t *testing.T) {
	sql := dumpBaseline(t)
	db, _ := newBaselineDB(t)
	baseline := relational.Baseline{SQL: sql, Floor: "20240101_one"}

	err := db.RunMigrationsFromBaseline(baseline, colorMigrations())
	require.NoError(t, err)

	assert.True(t, columnExists(t, db, "things", "color"))
	expected := []ledgerRow{{Name: "20240101_one", GroupID: 1}, {Name: "20240102_two", GroupID: 2}}
	assert.Equal(t, expected, readLedger(t, db))

	// A second run finds an existing database that is not behind
	err = db.RunMigrationsFromBaseline(baseline, colorMigrations())
	require.NoError(t, err)
	assert.Equal(t, expected, readLedger(t, db))
}

func TestRunMigrationsFromBaselineLeavesPoolSessionsAlone(t *testing.T) {
	// The dump turns statement_timeout off itself, so the only timeout that can
	// cancel the sleep at its end is the socket read deadline, which the dump
	// cannot change: the main pool's one second one would, the migration pool
	// has none
	sql := insideBaselineFrame(t, dumpBaseline(t), "SELECT pg_sleep(1.5);\n")
	cfg := baselineConfig(t)
	cfg.ReadTimeout = 1000
	db, _ := connectTestDB(t, cfg)
	baseline := relational.Baseline{SQL: sql, Floor: "20240101_one"}

	err := db.RunMigrationsFromBaseline(baseline, migrate.NewMigrations())
	require.NoError(t, err)

	// The load connection was discarded, and with no migrations nothing else opened one
	assert.Equal(t, 0, db.MigrationDB.Stats().OpenConnections)

	// The dump sets lock_timeout to 0 for its session and loadBaseline resets
	// only search_path, so a kept load session would answer 0 here
	var lockTimeout string
	err = db.MigrationDB.NewRaw("SHOW lock_timeout").Scan(context.Background(), &lockTimeout)
	require.NoError(t, err)
	assert.Equal(t, "10s", lockTimeout)

	var checkInterval string
	err = db.MigrationDB.NewRaw("SHOW client_connection_check_interval").Scan(context.Background(), &checkInterval)
	require.NoError(t, err)
	assert.Equal(t, "5s", checkInterval)
}

func TestRunMigrationsFromBaselineHintsWhenTablesExist(t *testing.T) {
	sql := dumpBaseline(t)
	db, _ := newBaselineDB(t)

	err := db.CreateTables([]interface{}{(*thing)(nil)})
	require.NoError(t, err)

	baseline := relational.Baseline{SQL: sql, Floor: "20240101_one"}
	err = db.RunMigrationsFromBaseline(baseline, colorMigrations())
	require.Error(t, err)
	assert.Equal(t, ez.ECONFLICT, ez.ErrorCode(err))
	// The hint is in the message and once in the text a boot log prints, and
	// the Postgres error stays in the chain
	assert.Contains(t, ez.ErrorMessage(err), "must have no tables")
	assert.Equal(t, 1, strings.Count(err.Error(), "must have no tables"))
	assert.Equal(t, "42P07", sqlState(t, err))
	assert.False(t, tableExists(t, db, "bun_migrations"))
}

func TestRunMigrationsFromBaselineHintsWhenFunctionExists(t *testing.T) {
	// pg_dump writes CREATE FUNCTION without OR REPLACE, so a function that is
	// already there fails like a table that is, with its own SQLSTATE; the
	// schema is spelled out because the dump has emptied search_path
	function := "CREATE FUNCTION public.probe_one() RETURNS int LANGUAGE sql AS 'SELECT 1';\n"
	sql := insideBaselineFrame(t, dumpBaseline(t), function+function)
	db, _ := newBaselineDB(t)

	baseline := relational.Baseline{SQL: sql, Floor: "20240101_one"}
	err := db.RunMigrationsFromBaseline(baseline, colorMigrations())
	require.Error(t, err)
	assert.Equal(t, ez.ECONFLICT, ez.ErrorCode(err))
	assert.Contains(t, ez.ErrorMessage(err), "must have no tables")
	assert.Equal(t, 1, strings.Count(err.Error(), "must have no tables"))
	assert.Equal(t, "42723", sqlState(t, err))
	assert.False(t, tableExists(t, db, "bun_migrations"))
}

func TestRunMigrationsFromBaselineHintsWhenServerRejectsSetting(t *testing.T) {
	// A made-up parameter stands in for the SET transaction_timeout a pg_dump 17
	// or newer writes: Postgres 16 rejects both with 42704, 17 accepts the real one
	sql := insideBaselineFrame(t, dumpBaseline(t), "SET transaction_timeout_of_a_newer_pg_dump = 0;\n")
	db, _ := newBaselineDB(t)

	baseline := relational.Baseline{SQL: sql, Floor: "20240101_one"}
	err := db.RunMigrationsFromBaseline(baseline, colorMigrations())
	require.Error(t, err)
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	assert.Contains(t, ez.ErrorMessage(err), "pg_dump of the dumped server's major")
	assert.Equal(t, 1, strings.Count(err.Error(), "pg_dump of the dumped server's major"))
	assert.Equal(t, "42704", sqlState(t, err))
	assert.False(t, tableExists(t, db, "bun_migrations"))
}

func TestRunMigrationsFromBaselineKeepsOtherLoadErrors(t *testing.T) {
	// A failure that is neither an existing object nor a missing setting gets
	// no hint: the generic internal code, with the Postgres error in the text.
	// An undefined table is a 42 class code next to the classified ones
	sql := insideBaselineFrame(t, dumpBaseline(t), "SELECT * FROM public.no_such_table;\n")
	db, _ := newBaselineDB(t)

	baseline := relational.Baseline{SQL: sql, Floor: "20240101_one"}
	err := db.RunMigrationsFromBaseline(baseline, colorMigrations())
	require.Error(t, err)
	assert.Equal(t, ez.EINTERNAL, ez.ErrorCode(err))
	assert.NotContains(t, ez.ErrorMessage(err), "Baseline SQL failed to load")
	assert.NotContains(t, err.Error(), "Baseline SQL failed to load")
	assert.Equal(t, "42P01", sqlState(t, err))
	assert.False(t, tableExists(t, db, "bun_migrations"))
}

func TestRunMigrationsFromBaselineWithoutBaseline(t *testing.T) {
	db, _ := newBaselineDB(t)

	migrations := migrate.NewMigrations()
	migrations.Add(migrate.Migration{
		Name: "20240101_one",
		Up: func(ctx context.Context, migrator *migrate.Migrator, _ *migrate.Migration) error {
			_, err := migrator.DB().ExecContext(ctx, "CREATE TABLE things (id bigserial PRIMARY KEY, name text NOT NULL)")
			if err != nil {
				return ez.Wrap(err)
			}

			return nil
		},
	})

	err := db.RunMigrationsFromBaseline(relational.Baseline{}, migrations)
	require.NoError(t, err)

	assert.True(t, tableExists(t, db, "things"))
	assert.Equal(t, []ledgerRow{{Name: "20240101_one", GroupID: 1}}, readLedger(t, db))
}

func TestRunMigrationsFromBaselineFloorWithoutSQL(t *testing.T) {
	db, _ := newBaselineDB(t)
	baseline := relational.Baseline{Floor: "20240101_one"}

	err := db.RunMigrationsFromBaseline(baseline, colorMigrations())
	require.Error(t, err)
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	assert.False(t, tableExists(t, db, "bun_migrations"))
}

func TestRunMigrationsFromBaselineSQLWithoutFloor(t *testing.T) {
	sql := dumpBaseline(t)
	db, _ := newBaselineDB(t)
	baseline := relational.Baseline{SQL: sql}

	err := db.RunMigrationsFromBaseline(baseline, colorMigrations())
	require.Error(t, err)
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	assert.False(t, tableExists(t, db, "bun_migrations"))
}

func TestRunMigrationsFromBaselineRejectsEmptyLedger(t *testing.T) {
	sql := dumpBaseline(t)
	db, _ := newBaselineDB(t)

	err := migrate.NewMigrator(db.DB, migrate.NewMigrations()).Init(context.Background())
	require.NoError(t, err)

	baseline := relational.Baseline{SQL: sql, Floor: "20240101_one"}
	err = db.RunMigrationsFromBaseline(baseline, colorMigrations())
	require.Error(t, err)
	assert.Equal(t, ez.ECONFLICT, ez.ErrorCode(err))
	assert.Contains(t, err.Error(), "20240101_one")

	assert.False(t, tableExists(t, db, "things"))
	assert.Empty(t, readLedger(t, db))
}

func TestRunMigrationsFromBaselineEmptyLedgerWithoutBaseline(t *testing.T) {
	db, _ := newBaselineDB(t)
	ctx := context.Background()

	err := migrate.NewMigrator(db.DB, migrate.NewMigrations()).Init(ctx)
	require.NoError(t, err)

	// colorMigrations alters things, which a baseline would have created
	_, err = db.ExecContext(ctx, "CREATE TABLE things (id bigserial PRIMARY KEY, name text NOT NULL)")
	require.NoError(t, err)

	err = db.RunMigrationsFromBaseline(relational.Baseline{}, colorMigrations())
	require.NoError(t, err)

	assert.True(t, columnExists(t, db, "things", "color"))
	assert.Equal(t, []ledgerRow{{Name: "20240102_two", GroupID: 1}}, readLedger(t, db))
}

func TestRunMigrationsFromBaselineKeepsBackslashLinesInsideSQL(t *testing.T) {
	// The function sits inside the dump's \restrict frame, where pg_dump puts
	// it: the frame is dropped, the lines of the same shape in the body are not
	sql := insideBaselineFrame(t, dumpBaseline(t), `
CREATE FUNCTION public.baseline_probe() RETURNS text LANGUAGE sql AS $$
SELECT '
\d+ is not a meta-command
\restrict othertoken
\restricted is not the framing either
\unrestrict othertoken'
$$;
`)
	db, _ := newBaselineDB(t)
	baseline := relational.Baseline{SQL: sql, Floor: "20240101_one"}

	err := db.RunMigrationsFromBaseline(baseline, colorMigrations())
	require.NoError(t, err)

	var probe string
	err = db.NewRaw("SELECT public.baseline_probe()").Scan(context.Background(), &probe)
	require.NoError(t, err)
	assert.Contains(t, probe, `\d+`)
	assert.Contains(t, probe, "\n\\restrict othertoken\n")
	assert.Contains(t, probe, "\n\\restricted is not the framing either\n")
	assert.Contains(t, probe, "\n\\unrestrict othertoken")
}

func TestRunMigrationsFromBaselineWithNoMigrationsAboveFloor(t *testing.T) {
	sql := dumpBaseline(t)
	db, _ := newBaselineDB(t)
	baseline := relational.Baseline{SQL: sql, Floor: "20240101_one"}

	err := db.RunMigrationsFromBaseline(baseline, migrate.NewMigrations())
	require.NoError(t, err)

	assert.True(t, tableExists(t, db, "things"))
	assert.Equal(t, []ledgerRow{{Name: "20240101_one", GroupID: 1}}, readLedger(t, db))
}

func TestRunMigrationsFromBaselineRejectsDatabaseBehind(t *testing.T) {
	sql := dumpBaseline(t)
	db, _ := newBaselineDB(t)

	zero := migrate.NewMigrations()
	zero.Add(migrate.Migration{Name: "20231201_zero"})
	err := db.RunMigrations(zero)
	require.NoError(t, err)

	baseline := relational.Baseline{SQL: sql, Floor: "20240101_one"}
	err = db.RunMigrationsFromBaseline(baseline, colorMigrations())
	require.Error(t, err)
	assert.Equal(t, ez.ECONFLICT, ez.ErrorCode(err))
	assert.Contains(t, err.Error(), "20240101_one")

	assert.False(t, tableExists(t, db, "things"))
	assert.Equal(t, []ledgerRow{{Name: "20231201_zero", GroupID: 1}}, readLedger(t, db))
}

func TestRunMigrationsFromBaselineRejectsLedgerWithoutFloor(t *testing.T) {
	// A ledger that holds a name above the floor but not the floor itself is
	// behind too: the check is for that one row, not for anything at or past it
	sql := dumpBaseline(t)
	db, _ := newBaselineDB(t)

	later := migrate.NewMigrations()
	later.Add(migrate.Migration{Name: "20240102_two"})
	err := db.RunMigrations(later)
	require.NoError(t, err)

	baseline := relational.Baseline{SQL: sql, Floor: "20240101_one"}
	err = db.RunMigrationsFromBaseline(baseline, migrate.NewMigrations())
	require.Error(t, err)
	assert.Equal(t, ez.ECONFLICT, ez.ErrorCode(err))
	assert.Contains(t, err.Error(), "20240101_one")

	assert.False(t, tableExists(t, db, "things"))
	assert.Equal(t, []ledgerRow{{Name: "20240102_two", GroupID: 1}}, readLedger(t, db))
}

// A file above the floor is accepted, see TestRunMigrationsFromBaselineLoadsFreshDatabase
func TestRunMigrationsFromBaselineRejectsFileAtOrBelowFloor(t *testing.T) {
	floors := map[string]string{
		"equal": "20240102_two",
		"below": "20240103_three",
	}

	for name, floor := range floors {
		t.Run(name, func(t *testing.T) {
			db, _ := newBaselineDB(t)
			baseline := relational.Baseline{SQL: "SELECT 1", Floor: floor}

			err := db.RunMigrationsFromBaseline(baseline, colorMigrations())
			require.Error(t, err)
			assert.Equal(t, ez.ECONFLICT, ez.ErrorCode(err))
			assert.Contains(t, err.Error(), "20240102_two")
			assert.False(t, tableExists(t, db, "bun_migrations"))
		})
	}
}

func TestRunMigrationsFromBaselineFloorWithBunShapedNames(t *testing.T) {
	sql := dumpBaseline(t)

	selectOne := func(name, comment string) migrate.Migration {
		return migrate.Migration{
			Name:    name,
			Comment: comment,
			Up: func(ctx context.Context, migrator *migrate.Migrator, _ *migrate.Migration) error {
				_, err := migrator.DB().ExecContext(ctx, "SELECT 1")
				if err != nil {
					return ez.Wrap(err)
				}

				return nil
			},
		}
	}

	tests := []struct {
		name       string
		registered []migrate.Migration
		accepted   bool
	}{
		{name: "below", registered: []migrate.Migration{selectOne("1", "add_colors")}},
		{name: "equal", registered: []migrate.Migration{selectOne("20240928", "add_colors")}},
		{name: "above", registered: []migrate.Migration{selectOne("20240928150000", "add_colors")}, accepted: true},
		{
			// Above in bun's string order, below as a number
			name:       "shorter",
			registered: []migrate.Migration{selectOne("9", "add_colors")},
			accepted:   true,
		},
		{
			// bun's Add takes two migrations with one name
			name:       "duplicate",
			registered: []migrate.Migration{selectOne("20240928", "add_colors"), selectOne("20240928", "add_shapes")},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db, _ := newBaselineDB(t)
			baseline := relational.Baseline{SQL: sql, Floor: "20240928"}

			migrations := migrate.NewMigrations()
			for _, migration := range test.registered {
				migrations.Add(migration)
			}

			err := db.RunMigrationsFromBaseline(baseline, migrations)
			if test.accepted {
				require.NoError(t, err)
				expected := []ledgerRow{{Name: "20240928", GroupID: 1}, {Name: test.registered[0].Name, GroupID: 2}}
				assert.Equal(t, expected, readLedger(t, db))
				return
			}

			require.Error(t, err)
			assert.Equal(t, ez.ECONFLICT, ez.ErrorCode(err))
			// The message names the migration the way bun prints it, name_comment
			assert.Contains(t, ez.ErrorMessage(err), migrations.Sorted()[0].String())
			assert.False(t, tableExists(t, db, "bun_migrations"))
		})
	}
}

func TestRunMigrationsFromBaselineFloorStaysWhileFolderGrows(t *testing.T) {
	sql := dumpBaseline(t)
	db, _ := newBaselineDB(t)
	baseline := relational.Baseline{SQL: sql, Floor: "20240101_one"}

	err := db.RunMigrationsFromBaseline(baseline, colorMigrations())
	require.NoError(t, err)

	// A new migration lands in the folder and the floor stays where it was
	migrations := colorMigrations()
	migrations.Add(migrate.Migration{
		Name: "20240103_three",
		Up: func(ctx context.Context, migrator *migrate.Migrator, _ *migrate.Migration) error {
			_, err := migrator.DB().ExecContext(ctx, "CREATE TABLE shapes (id bigserial PRIMARY KEY)")
			if err != nil {
				return ez.Wrap(err)
			}

			return nil
		},
	})

	err = db.RunMigrationsFromBaseline(baseline, migrations)
	require.NoError(t, err)

	assert.True(t, tableExists(t, db, "shapes"))
	expected := []ledgerRow{
		{Name: "20240101_one", GroupID: 1},
		{Name: "20240102_two", GroupID: 2},
		{Name: "20240103_three", GroupID: 3},
	}
	assert.Equal(t, expected, readLedger(t, db))
}

func TestRunMigrationsFromBaselineRequiresLedgerInSQL(t *testing.T) {
	db, _ := newBaselineDB(t)
	baseline := relational.Baseline{
		SQL:   "CREATE TABLE things (id bigserial PRIMARY KEY, name text NOT NULL);",
		Floor: "20240101_one",
	}

	err := db.RunMigrationsFromBaseline(baseline, colorMigrations())
	require.Error(t, err)
	assert.Equal(t, ez.ECONFLICT, ez.ErrorCode(err))
	assert.Contains(t, ez.ErrorMessage(err), "bun_migrations")
	assert.False(t, tableExists(t, db, "things"))
	assert.False(t, tableExists(t, db, "bun_migrations"))
}

func TestRollbackStopsAtBaseline(t *testing.T) {
	sql := dumpBaseline(t)
	db, _ := newBaselineDB(t)
	baseline := relational.Baseline{SQL: sql, Floor: "20240101_one"}
	migrations := colorMigrations()

	err := db.RunMigrationsFromBaseline(baseline, migrations)
	require.NoError(t, err)

	err = db.RollbackLastMigration(migrations)
	require.NoError(t, err)
	assert.False(t, columnExists(t, db, "things", "color"))
	assert.Equal(t, []ledgerRow{{Name: "20240101_one", GroupID: 1}}, readLedger(t, db))

	// bun has no file for the floor migration, so nothing is left to roll back
	err = db.RollbackLastMigration(migrations)
	require.NoError(t, err)
	assert.True(t, tableExists(t, db, "things"))
	assert.Equal(t, []ledgerRow{{Name: "20240101_one", GroupID: 1}}, readLedger(t, db))
}

func TestRollbackLastMigrationWithoutFiles(t *testing.T) {
	db, _ := newBaselineDB(t)

	err := db.RollbackLastMigration(migrate.NewMigrations())
	require.NoError(t, err)

	// A no-op leaves the database fresh, so a baseline can still load into it
	assert.False(t, tableExists(t, db, "bun_migrations"))
}

func TestRunMigrationsFromBaselineRejectsSQLite(t *testing.T) {
	db, err := sqlite.ConnectToMemoryDatabase()
	require.NoError(t, err)
	defer db.Close() // nolint:errcheck

	err = db.RunMigrationsFromBaseline(relational.Baseline{}, migrate.NewMigrations())
	require.Error(t, err)
	assert.Equal(t, ez.ENOTIMPLEMENTED, ez.ErrorCode(err))
}
