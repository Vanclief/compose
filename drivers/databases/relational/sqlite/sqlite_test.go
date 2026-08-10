package sqlite

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/suite"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/migrate"
	"github.com/vanclief/compose/drivers/databases/relational"
	"github.com/vanclief/ez"
)

type testItem struct {
	bun.BaseModel `bun:"table:test_items"`

	ID        int64          `bun:",pk,autoincrement"`
	Name      string         `bun:",notnull"`
	Meta      map[string]any `bun:"type:jsonb,nullzero"`
	CreatedAt time.Time      `bun:",nullzero"`
}

func testModels() []interface{} {
	return []interface{}{(*testItem)(nil)}
}

func noopMigration(ctx context.Context, migrator *migrate.Migrator, migration *migrate.Migration) error {
	return nil
}

// markerMigration creates a table whose presence proves the migration executed
func markerMigration(ctx context.Context, migrator *migrate.Migrator, migration *migrate.Migration) error {
	_, err := migrator.DB().ExecContext(ctx, "CREATE TABLE migration_marker (id INTEGER PRIMARY KEY)")
	return err
}

// slowMarkerMigration widens the race window before creating the marker
func slowMarkerMigration(ctx context.Context, migrator *migrate.Migrator, migration *migrate.Migration) error {
	time.Sleep(200 * time.Millisecond)
	return markerMigration(ctx, migrator, migration)
}

func failingMigration(ctx context.Context, migrator *migrate.Migrator, migration *migrate.Migration) error {
	return errors.New("migration exploded")
}

// initSchemaWorker simulates one instance booting against a shared database
func initSchemaWorker(path string, migrations *migrate.Migrations) error {
	db, err := ConnectToDatabase(&ConnectionConfig{Path: path})
	if err != nil {
		return err
	}
	defer db.Close() // nolint:errcheck

	return db.InitSchema(testModels(), migrations)
}

type TestSuite struct {
	suite.Suite
}

func TestSuiteRun(t *testing.T) {
	suite.Run(t, new(TestSuite))
}

func (suite *TestSuite) newFileDB() *relational.DB {
	path := filepath.Join(suite.T().TempDir(), "test.db")

	db, err := ConnectToDatabase(&ConnectionConfig{Path: path})
	suite.Require().NoError(err)

	return db
}

func (suite *TestSuite) tableExists(db *relational.DB, name string) bool {
	var count int
	err := db.NewRaw(
		"SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?",
		name,
	).Scan(context.Background(), &count)
	suite.Require().NoError(err)

	return count > 0
}

func (suite *TestSuite) TestConnectToDatabase() {
	db := suite.newFileDB()
	defer db.Close() // nolint:errcheck

	var journalMode string
	err := db.NewRaw("PRAGMA journal_mode").Scan(context.Background(), &journalMode)
	suite.NoError(err)
	suite.Equal("wal", journalMode)
}

func (suite *TestSuite) TestConnectToDatabaseEscapesPath() {
	// Characters that would be misread as DSN parameters if unescaped
	dir := filepath.Join(suite.T().TempDir(), "weird dir#1")
	path := filepath.Join(dir, "agc?.db")

	db, err := ConnectToDatabase(&ConnectionConfig{Path: path})
	suite.Require().NoError(err)
	defer db.Close() // nolint:errcheck

	var journalMode string
	err = db.NewRaw("PRAGMA journal_mode").Scan(context.Background(), &journalMode)
	suite.NoError(err)
	suite.Equal("wal", journalMode)

	// The database file exists at the literal path, not a mangled one
	_, err = os.Stat(path)
	suite.NoError(err)
}

func (suite *TestSuite) TestConnectToDatabaseRequiresPath() {
	db, err := ConnectToDatabase(&ConnectionConfig{})
	suite.Error(err)
	suite.Nil(db)
}

func (suite *TestSuite) TestConnectToMemoryDatabase() {
	db, err := ConnectToMemoryDatabase()
	suite.Require().NoError(err)
	defer db.Close() // nolint:errcheck

	err = db.CreateTables(testModels())
	suite.Require().NoError(err)

	ctx := context.Background()
	item := &testItem{
		Name:      "in-memory",
		Meta:      map[string]any{"kind": "test"},
		CreatedAt: time.Now().UTC(),
	}

	_, err = db.NewInsert().Model(item).Exec(ctx)
	suite.Require().NoError(err)

	var fetched testItem
	err = db.NewSelect().Model(&fetched).Where("id = ?", item.ID).Scan(ctx)
	suite.NoError(err)
	suite.Equal("in-memory", fetched.Name)
	suite.Equal("test", fetched.Meta["kind"])
}

func (suite *TestSuite) TestInitSchemaFresh() {
	db := suite.newFileDB()
	defer db.Close() // nolint:errcheck

	migrations := migrate.NewMigrations()
	migrations.Add(migrate.Migration{Name: "00000001", Up: noopMigration, Down: noopMigration})
	migrations.Add(migrate.Migration{Name: "00000002", Up: markerMigration, Down: noopMigration})

	err := db.InitSchema(testModels(), migrations)
	suite.Require().NoError(err)

	// The schema comes from the structs
	suite.True(suite.tableExists(db, "test_items"))

	// The migrations were recorded but never executed
	suite.False(suite.tableExists(db, "migration_marker"))

	migrator := migrate.NewMigrator(db.DB, migrations)
	applied, err := migrator.AppliedMigrations(context.Background())
	suite.NoError(err)
	suite.Len(applied, 2)

	// The created schema is usable
	ctx := context.Background()
	item := &testItem{
		Name:      "fresh",
		Meta:      map[string]any{"n": float64(1)},
		CreatedAt: time.Now().UTC(),
	}

	_, err = db.NewInsert().Model(item).Exec(ctx)
	suite.Require().NoError(err)

	var fetched testItem
	err = db.NewSelect().Model(&fetched).Where("id = ?", item.ID).Scan(ctx)
	suite.NoError(err)
	suite.Equal("fresh", fetched.Name)
	suite.Equal(float64(1), fetched.Meta["n"])
	suite.False(fetched.CreatedAt.IsZero())
}

func (suite *TestSuite) TestInitSchemaRunsOnlyPendingMigrations() {
	db := suite.newFileDB()
	defer db.Close() // nolint:errcheck

	baseline := migrate.NewMigrations()
	baseline.Add(migrate.Migration{Name: "00000001", Up: noopMigration, Down: noopMigration})
	baseline.Add(migrate.Migration{Name: "00000002", Up: noopMigration, Down: noopMigration})

	err := db.InitSchema(testModels(), baseline)
	suite.Require().NoError(err)

	// A later release ships a third migration
	upgraded := migrate.NewMigrations()
	upgraded.Add(migrate.Migration{Name: "00000001", Up: markerMigration, Down: noopMigration})
	upgraded.Add(migrate.Migration{Name: "00000002", Up: markerMigration, Down: noopMigration})
	upgraded.Add(migrate.Migration{Name: "00000003", Up: markerMigration, Down: noopMigration})

	err = db.InitSchema(testModels(), upgraded)
	suite.Require().NoError(err)

	// Only the pending migration ran: had 00000001 or 00000002 executed,
	// creating the marker table twice would have errored
	suite.True(suite.tableExists(db, "migration_marker"))

	migrator := migrate.NewMigrator(db.DB, upgraded)
	applied, err := migrator.AppliedMigrations(context.Background())
	suite.NoError(err)
	suite.Len(applied, 3)

	// Re-running with everything applied is a no-op
	err = db.InitSchema(testModels(), upgraded)
	suite.NoError(err)
}

func (suite *TestSuite) TestInitSchemaRollsBackOnFailure() {
	db := suite.newFileDB()
	defer db.Close() // nolint:errcheck

	// Duplicate names violate the unique migration-name index inside the
	// bootstrap transaction
	broken := migrate.NewMigrations()
	broken.Add(migrate.Migration{Name: "00000001", Up: noopMigration, Down: noopMigration})
	broken.Add(migrate.Migration{Name: "00000001", Up: noopMigration, Down: noopMigration})

	err := db.InitSchema(testModels(), broken)
	suite.Error(err)

	// The failed bootstrap left nothing behind
	suite.False(suite.tableExists(db, "test_items"))

	// And the database self-heals once the registry is fixed
	fixed := migrate.NewMigrations()
	fixed.Add(migrate.Migration{Name: "00000001", Up: noopMigration, Down: noopMigration})
	fixed.Add(migrate.Migration{Name: "00000002", Up: noopMigration, Down: noopMigration})

	err = db.InitSchema(testModels(), fixed)
	suite.NoError(err)
	suite.True(suite.tableExists(db, "test_items"))
}

func (suite *TestSuite) TestInitSchemaConcurrentBootstrap() {
	path := filepath.Join(suite.T().TempDir(), "test.db")

	newRegistry := func() *migrate.Migrations {
		m := migrate.NewMigrations()
		m.Add(migrate.Migration{Name: "00000001", Up: noopMigration, Down: noopMigration})
		m.Add(migrate.Migration{Name: "00000002", Up: noopMigration, Down: noopMigration})
		return m
	}

	const workers = 4
	errs := make(chan error, workers)

	for i := 0; i < workers; i++ {
		go func() {
			errs <- initSchemaWorker(path, newRegistry())
		}()
	}

	// Losers of the bootstrap race may fail cleanly, but someone must win
	succeeded := 0
	for i := 0; i < workers; i++ {
		err := <-errs
		if err == nil {
			succeeded++
		}
	}
	suite.GreaterOrEqual(succeeded, 1)

	// Whatever raced, the final state is consistent: tables exist and each
	// migration is recorded exactly once
	db, err := ConnectToDatabase(&ConnectionConfig{Path: path})
	suite.Require().NoError(err)
	defer db.Close() // nolint:errcheck

	suite.True(suite.tableExists(db, "test_items"))

	migrator := migrate.NewMigrator(db.DB, newRegistry())
	applied, err := migrator.AppliedMigrations(context.Background())
	suite.NoError(err)
	suite.Len(applied, 2)
}

func (suite *TestSuite) TestInitSchemaAdoptsExistingTables() {
	db := suite.newFileDB()
	defer db.Close() // nolint:errcheck

	// Tables created outside InitSchema with no recorded migrations are
	// treated as matching the current structs and get baselined
	err := db.CreateTables(testModels())
	suite.Require().NoError(err)

	migrations := migrate.NewMigrations()
	migrations.Add(migrate.Migration{Name: "00000001", Up: markerMigration, Down: noopMigration})

	err = db.InitSchema(testModels(), migrations)
	suite.NoError(err)

	// Baselined, not executed
	suite.False(suite.tableExists(db, "migration_marker"))

	migrator := migrate.NewMigrator(db.DB, migrations)
	applied, err := migrator.AppliedMigrations(context.Background())
	suite.NoError(err)
	suite.Len(applied, 1)
}

func (suite *TestSuite) TestInitSchemaAllowsSquashedRegistry() {
	db := suite.newFileDB()
	defer db.Close() // nolint:errcheck

	full := migrate.NewMigrations()
	full.Add(migrate.Migration{Name: "00000001", Up: noopMigration, Down: noopMigration})
	full.Add(migrate.Migration{Name: "00000002", Up: noopMigration, Down: noopMigration})

	err := db.InitSchema(testModels(), full)
	suite.Require().NoError(err)

	// A later release removes an old migration without adding new ones:
	// recorded history exceeds the registry, but nothing is pending
	squashed := migrate.NewMigrations()
	squashed.Add(migrate.Migration{Name: "00000002", Up: noopMigration, Down: noopMigration})

	err = db.InitSchema(testModels(), squashed)
	suite.NoError(err)
}

func (suite *TestSuite) TestVerifySchema() {
	db := suite.newFileDB()
	defer db.Close() // nolint:errcheck

	migrations := migrate.NewMigrations()
	migrations.Add(migrate.Migration{Name: "00000001", Up: noopMigration, Down: noopMigration})

	// Fresh database: nothing recorded yet
	status, err := db.VerifySchema(migrations)
	suite.Require().NoError(err)
	suite.True(status.Fresh)
	suite.False(status.Current())

	err = db.InitSchema(testModels(), migrations)
	suite.Require().NoError(err)

	// Bootstrapped: current
	status, err = db.VerifySchema(migrations)
	suite.Require().NoError(err)
	suite.True(status.Current())

	// A new release ships a migration: pending, no longer current
	grown := migrate.NewMigrations()
	grown.Add(migrate.Migration{Name: "00000001", Up: noopMigration, Down: noopMigration})
	grown.Add(migrate.Migration{Name: "00000002", Up: noopMigration, Down: noopMigration})

	status, err = db.VerifySchema(grown)
	suite.Require().NoError(err)
	suite.False(status.Fresh)
	suite.Equal(1, status.Pending)
	suite.False(status.Current())

	// A squashed registry reports the orphaned record as missing
	err = db.InitSchema(testModels(), grown)
	suite.Require().NoError(err)

	squashed := migrate.NewMigrations()
	squashed.Add(migrate.Migration{Name: "00000002", Up: noopMigration, Down: noopMigration})

	status, err = db.VerifySchema(squashed)
	suite.Require().NoError(err)
	suite.Equal(1, status.Missing)
	suite.False(status.Current())

	err = db.PruneMigrationRecords(squashed)
	suite.Require().NoError(err)

	status, err = db.VerifySchema(squashed)
	suite.Require().NoError(err)
	suite.True(status.Current())
}

func (suite *TestSuite) TestVerifySchemaEmptyRegistry() {
	db := suite.newFileDB()
	defer db.Close() // nolint:errcheck

	// Fresh database, no registry: fresh, not current
	status, err := db.VerifySchema(nil)
	suite.Require().NoError(err)
	suite.True(status.Fresh)
	suite.False(status.Current())

	migrations := migrate.NewMigrations()
	migrations.Add(migrate.Migration{Name: "00000001", Up: noopMigration, Down: noopMigration})

	err = db.InitSchema(testModels(), migrations)
	suite.Require().NoError(err)

	// Recorded history verified against an empty registry must surface as
	// missing, never as current
	status, err = db.VerifySchema(migrate.NewMigrations())
	suite.Require().NoError(err)
	suite.False(status.Fresh)
	suite.Equal(1, status.Missing)
	suite.False(status.Current())
}

// appliedCount reports how many migrations the database has recorded
func (suite *TestSuite) appliedCount(db *relational.DB, migrations *migrate.Migrations) int {
	migrator := migrate.NewMigrator(db.DB, migrations)

	applied, err := migrator.AppliedMigrations(context.Background())
	suite.Require().NoError(err)

	return len(applied)
}

func (suite *TestSuite) TestNilRegistryContracts() {
	db := suite.newFileDB()
	defer db.Close() // nolint:errcheck

	migrations := migrate.NewMigrations()
	migrations.Add(migrate.Migration{Name: "00000001", Up: noopMigration, Down: noopMigration})

	err := db.InitSchema(testModels(), migrations)
	suite.Require().NoError(err)

	// Pruning against nil must refuse instead of reading the whole history
	// as missing and deleting it
	err = db.PruneMigrationRecords(nil)
	suite.Error(err)
	suite.Equal(ez.EINVALID, ez.ErrorCode(err))
	suite.Equal(1, suite.appliedCount(db, migrations))

	// Baseline and run treat nil as a no-op
	err = db.BaselineMigrations(nil)
	suite.NoError(err)
	suite.Equal(1, suite.appliedCount(db, migrations))

	err = db.RunMigrations(nil)
	suite.NoError(err)
	suite.Equal(1, suite.appliedCount(db, migrations))
}

func (suite *TestSuite) TestInitSchemaEmptiedRegistryKeepsHistory() {
	db := suite.newFileDB()
	defer db.Close() // nolint:errcheck

	migrations := migrate.NewMigrations()
	migrations.Add(migrate.Migration{Name: "00000001", Up: noopMigration, Down: noopMigration})

	err := db.InitSchema(testModels(), migrations)
	suite.Require().NoError(err)

	// A registry squashed to empty boots with the orphan warning, leaving
	// the history intact for an explicit prune — same as a partial squash.
	// The captured log proves the history analysis actually ran, which the
	// old early-return skipped.
	var logs bytes.Buffer
	previousLogger := log.Logger
	log.Logger = zerolog.New(&logs)
	defer func() {
		log.Logger = previousLogger
	}()

	err = db.InitSchema(testModels(), migrate.NewMigrations())
	suite.NoError(err)
	suite.Contains(logs.String(), "missing from the registry")

	suite.Equal(1, suite.appliedCount(db, migrations))
}

func (suite *TestSuite) TestSchemaLockSurfacesCrashedMigration() {
	db := suite.newFileDB()
	defer db.Close() // nolint:errcheck

	migrations := migrate.NewMigrations()
	migrations.Add(migrate.Migration{Name: "00000001", Up: noopMigration, Down: noopMigration})

	err := db.InitSchema(testModels(), migrations)
	suite.Require().NoError(err)

	// A migrator that crashed mid-execution leaves the lock held while the
	// records already claim the schema is current
	ctx := context.Background()
	migrator := migrate.NewMigrator(db.DB, migrations)
	err = migrator.Lock(ctx)
	suite.Require().NoError(err)

	// The held lock must override what the records say
	status, err := db.VerifySchema(migrations)
	suite.Require().NoError(err)
	suite.True(status.Locked)
	suite.False(status.Current())

	err = db.InitSchema(testModels(), migrations)
	suite.Error(err)
	suite.Equal(ez.ECONFLICT, ez.ErrorCode(err))

	// Once the operator clears the lock, everything reads current again
	err = migrator.Unlock(ctx)
	suite.Require().NoError(err)

	status, err = db.VerifySchema(migrations)
	suite.Require().NoError(err)
	suite.False(status.Locked)
	suite.True(status.Current())

	err = db.InitSchema(testModels(), migrations)
	suite.NoError(err)
}

func (suite *TestSuite) TestInitSchemaKeepsLockWhenRollbackFails() {
	db := suite.newFileDB()
	defer db.Close() // nolint:errcheck

	baseline := migrate.NewMigrations()
	baseline.Add(migrate.Migration{Name: "00000001", Up: noopMigration, Down: noopMigration})

	err := db.InitSchema(testModels(), baseline)
	suite.Require().NoError(err)

	// The new migration fails, and so does its rollback: the schema state
	// is unknown
	broken := migrate.NewMigrations()
	broken.Add(migrate.Migration{Name: "00000001", Up: noopMigration, Down: noopMigration})
	broken.Add(migrate.Migration{Name: "00000002", Up: failingMigration, Down: failingMigration})

	err = db.InitSchema(testModels(), broken)
	suite.Error(err)

	// The lock stays held as the tombstone: status reports it and boots
	// refuse until an operator inspects the schema
	status, err := db.VerifySchema(broken)
	suite.Require().NoError(err)
	suite.True(status.Locked)
	suite.False(status.Current())

	err = db.InitSchema(testModels(), broken)
	suite.Error(err)
	suite.Equal(ez.ECONFLICT, ez.ErrorCode(err))
}

func (suite *TestSuite) TestInitSchemaReleasesLockWhenRollbackSucceeds() {
	db := suite.newFileDB()
	defer db.Close() // nolint:errcheck

	baseline := migrate.NewMigrations()
	baseline.Add(migrate.Migration{Name: "00000001", Up: noopMigration, Down: noopMigration})

	err := db.InitSchema(testModels(), baseline)
	suite.Require().NoError(err)

	// The migration fails but rolls back cleanly: no lock is left behind
	// and retrying works without manual cleanup
	broken := migrate.NewMigrations()
	broken.Add(migrate.Migration{Name: "00000001", Up: noopMigration, Down: noopMigration})
	broken.Add(migrate.Migration{Name: "00000002", Up: failingMigration, Down: noopMigration})

	err = db.InitSchema(testModels(), broken)
	suite.Error(err)

	status, err := db.VerifySchema(broken)
	suite.Require().NoError(err)
	suite.False(status.Locked)
	suite.Equal(1, status.Pending)

	// A fixed release retries successfully
	fixed := migrate.NewMigrations()
	fixed.Add(migrate.Migration{Name: "00000001", Up: noopMigration, Down: noopMigration})
	fixed.Add(migrate.Migration{Name: "00000002", Up: markerMigration, Down: noopMigration})

	err = db.InitSchema(testModels(), fixed)
	suite.NoError(err)
	suite.True(suite.tableExists(db, "migration_marker"))
}

func (suite *TestSuite) TestSchemaLockIgnoresOtherRegistries() {
	db := suite.newFileDB()
	defer db.Close() // nolint:errcheck

	migrations := migrate.NewMigrations()
	migrations.Add(migrate.Migration{Name: "00000001", Up: noopMigration, Down: noopMigration})

	err := db.InitSchema(testModels(), migrations)
	suite.Require().NoError(err)

	// A custom migrator with its own migrations table holds its own lock
	// in the shared locks table
	ctx := context.Background()
	other := migrate.NewMigrator(db.DB, migrate.NewMigrations(), migrate.WithTableName("custom_migrations"))
	err = other.Init(ctx)
	suite.Require().NoError(err)

	err = other.Lock(ctx)
	suite.Require().NoError(err)
	defer other.Unlock(ctx) // nolint:errcheck

	// Another registry's lock must not make this schema appear locked
	status, err := db.VerifySchema(migrations)
	suite.Require().NoError(err)
	suite.False(status.Locked)
	suite.True(status.Current())

	err = db.InitSchema(testModels(), migrations)
	suite.NoError(err)
}

func (suite *TestSuite) TestInitSchemaSquashThenPruneFlow() {
	db := suite.newFileDB()
	defer db.Close() // nolint:errcheck

	full := migrate.NewMigrations()
	full.Add(migrate.Migration{Name: "00000001", Up: noopMigration, Down: noopMigration})
	full.Add(migrate.Migration{Name: "00000002", Up: noopMigration, Down: noopMigration})

	err := db.InitSchema(testModels(), full)
	suite.Require().NoError(err)

	// A release squashes the registry: boot still works (with a warning)
	squashed := migrate.NewMigrations()
	squashed.Add(migrate.Migration{Name: "00000002", Up: noopMigration, Down: noopMigration})

	err = db.InitSchema(testModels(), squashed)
	suite.Require().NoError(err)

	// A later release adds a new migration: without pruning, the stale
	// record makes it indistinguishable from a rename, so boot refuses
	grown := migrate.NewMigrations()
	grown.Add(migrate.Migration{Name: "00000002", Up: noopMigration, Down: noopMigration})
	grown.Add(migrate.Migration{Name: "00000003", Up: markerMigration, Down: noopMigration})

	err = db.InitSchema(testModels(), grown)
	suite.Error(err)
	suite.False(suite.tableExists(db, "migration_marker"))

	// The explicit prune declares the squash, after which the new
	// migration runs normally
	err = db.PruneMigrationRecords(grown)
	suite.Require().NoError(err)

	err = db.InitSchema(testModels(), grown)
	suite.NoError(err)
	suite.True(suite.tableExists(db, "migration_marker"))
}

func (suite *TestSuite) TestInitSchemaConcurrentPendingMigrations() {
	path := filepath.Join(suite.T().TempDir(), "test.db")

	baseline := migrate.NewMigrations()
	baseline.Add(migrate.Migration{Name: "00000001", Up: noopMigration, Down: noopMigration})

	db, err := ConnectToDatabase(&ConnectionConfig{Path: path})
	suite.Require().NoError(err)

	err = db.InitSchema(testModels(), baseline)
	suite.Require().NoError(err)
	suite.Require().NoError(db.Close())

	// A slow pending migration maximizes the race window
	upgraded := func() *migrate.Migrations {
		m := migrate.NewMigrations()
		m.Add(migrate.Migration{Name: "00000001", Up: noopMigration, Down: noopMigration})
		m.Add(migrate.Migration{Name: "00000002", Up: slowMarkerMigration, Down: noopMigration})
		return m
	}

	const workers = 4
	errs := make(chan error, workers)

	for i := 0; i < workers; i++ {
		go func() {
			errs <- initSchemaWorker(path, upgraded())
		}()
	}

	// Coordinating concurrent InitSchema calls is the caller's job; compose
	// only guarantees no corruption — losers fail fast on the lock instead
	// of marking or rolling back the winner's work
	succeeded := 0
	for i := 0; i < workers; i++ {
		err = <-errs
		if err == nil {
			succeeded++
		}
	}
	suite.GreaterOrEqual(succeeded, 1)

	db, err = ConnectToDatabase(&ConnectionConfig{Path: path})
	suite.Require().NoError(err)
	defer db.Close() // nolint:errcheck

	// The migration ran exactly once and was not rolled back by a loser
	suite.True(suite.tableExists(db, "migration_marker"))

	migrator := migrate.NewMigrator(db.DB, upgraded())
	applied, err := migrator.AppliedMigrations(context.Background())
	suite.NoError(err)
	suite.Len(applied, 2)
}

func (suite *TestSuite) TestInitSchemaRefusesRenamedMigrations() {
	db := suite.newFileDB()
	defer db.Close() // nolint:errcheck

	original := migrate.NewMigrations()
	original.Add(migrate.Migration{Name: "00000001", Up: noopMigration, Down: noopMigration})
	original.Add(migrate.Migration{Name: "00000002", Up: noopMigration, Down: noopMigration})

	err := db.InitSchema(testModels(), original)
	suite.Require().NoError(err)

	// Renaming an applied migration makes it look missing under the old
	// name and pending under the new one; running it would replay its
	// changes, so InitSchema must refuse
	renamed := migrate.NewMigrations()
	renamed.Add(migrate.Migration{Name: "00000001", Up: noopMigration, Down: noopMigration})
	renamed.Add(migrate.Migration{Name: "00000003", Up: markerMigration, Down: noopMigration})

	err = db.InitSchema(testModels(), renamed)
	suite.Error(err)

	// And nothing was replayed
	suite.False(suite.tableExists(db, "migration_marker"))
}

func (suite *TestSuite) TestInitSchemaWithoutMigrations() {
	db := suite.newFileDB()
	defer db.Close() // nolint:errcheck

	err := db.InitSchema(testModels(), nil)
	suite.NoError(err)
	suite.True(suite.tableExists(db, "test_items"))

	// The unified path records bookkeeping even without migrations, so a
	// registry added later starts from known state
	suite.True(suite.tableExists(db, "bun_migrations"))

	// Idempotent on re-run
	err = db.InitSchema(testModels(), nil)
	suite.NoError(err)
}
