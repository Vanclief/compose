package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/migrate"
	"github.com/vanclief/compose/drivers/databases/relational"
	"github.com/vanclief/compose/drivers/databases/relational/postgres"
	"github.com/vanclief/ez"
)

// testConfig - Points at the local server with a base name no other test or
// test binary uses. It skips when PostgreSQL is unreachable, unless
// COMPOSE_TEST_POSTGRES is set.
func testConfig(t *testing.T) postgres.ConnectionConfig {
	suffix := make([]byte, 6)
	_, err := rand.Read(suffix)
	require.NoError(t, err)

	cfg := postgres.ConnectionConfig{
		Username:     "postgres",
		Password:     "",
		Host:         "localhost:5432",
		Database:     "postgres",
		MaxOpenConns: 2,
	}

	db, err := postgres.ConnectToDatabase(&cfg)
	if err != nil {
		if os.Getenv("COMPOSE_TEST_POSTGRES") != "" {
			t.Fatalf("PostgreSQL testing is enabled but the database is unavailable: %v", err)
		}

		t.Skipf("PostgreSQL is not available: %v", err)
	}

	db.Close() // nolint:errcheck

	cfg.Database = "compose_pgtest_" + hex.EncodeToString(suffix)

	return cfg
}

// createTestDB - Runs Create and drops the database when the test ends
func createTestDB(t *testing.T, cfg postgres.ConnectionConfig) postgres.ConnectionConfig {
	created, err := Create(cfg)
	require.NoError(t, err)

	t.Cleanup(func() {
		err := Drop(created)
		assert.NoError(t, err)
	})

	return created
}

// createNamedDB - Creates a database with exactly the given name, as an
// earlier run would have left it, and drops it when the test ends
func createNamedDB(t *testing.T, cfg postgres.ConnectionConfig, name string) {
	cfg.Database = name

	err := postgres.CreateDatabase(cfg)
	require.NoError(t, err)

	// Not Drop, which refuses a name outside Create's scheme
	t.Cleanup(func() {
		err := onAdminDatabase(cfg, func(db *relational.DB) error {
			_, err := db.ExecContext(context.Background(), "DROP DATABASE IF EXISTS ? WITH (FORCE)", bun.Ident(name))
			return err
		})
		assert.NoError(t, err)
	})
}

// connect - Opens a session on the database cfg points at, closed when the
// test ends
func connect(t *testing.T, cfg postgres.ConnectionConfig) *relational.DB {
	db, err := postgres.ConnectToDatabase(&cfg)
	require.NoError(t, err)

	t.Cleanup(func() {
		db.Close() // nolint:errcheck
	})

	return db
}

// databaseExists - Reports whether the server has a database named name
func databaseExists(t *testing.T, cfg postgres.ConnectionConfig, name string) bool {
	cfg.Database = "postgres"

	db, err := postgres.ConnectToDatabase(&cfg)
	require.NoError(t, err)
	defer db.Close() // nolint:errcheck

	var exists bool
	err = db.NewRaw("SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = ?)", name).Scan(context.Background(), &exists)
	require.NoError(t, err)

	return exists
}

// stampedName - A name in Create's scheme with the given creation time
func stampedName(t *testing.T, base string, createdAt time.Time) string {
	suffix := make([]byte, 8)
	_, err := rand.Read(suffix)
	require.NoError(t, err)

	return fmt.Sprintf("%s_%d_%s", base, createdAt.Unix(), hex.EncodeToString(suffix))
}

// countRows - Counts the rows of a table. A table whose name bun.Ident would
// split or misquote is passed already quoted.
func countRows(t *testing.T, db *relational.DB, table string) int {
	var name any = bun.Ident(table)
	if strings.HasPrefix(table, `"`) {
		name = bun.Safe(table)
	}

	var count int
	err := db.NewRaw("SELECT count(*) FROM ?", name).Scan(context.Background(), &count)
	require.NoError(t, err)

	return count
}

func TestCreate(t *testing.T) {
	cfg := testConfig(t)
	created := createTestDB(t, cfg)

	pattern := regexp.MustCompile(`^` + regexp.QuoteMeta(cfg.Database) + `_[0-9]+_[0-9a-f]{16}$`)
	assert.Regexp(t, pattern, created.Database)
	assert.True(t, databaseExists(t, cfg, created.Database))

	db := connect(t, created)

	var synchronousCommit string
	err := db.NewRaw("SHOW synchronous_commit").Scan(context.Background(), &synchronousCommit)
	require.NoError(t, err)
	assert.Equal(t, "off", synchronousCommit)
}

func TestCreateLowercasesTheName(t *testing.T) {
	cfg := testConfig(t)
	base := cfg.Database
	cfg.Database = strings.ToUpper(base)

	created := createTestDB(t, cfg)

	assert.True(t, strings.HasPrefix(created.Database, base+"_"))
	assert.Equal(t, strings.ToLower(created.Database), created.Database)
}

func TestCreateAndSweepRejectABaseNameTheStampDoesNotFit(t *testing.T) {
	cfg := testConfig(t)

	// 35 bytes is the longest base whose stamped name fits PostgreSQL's 63
	longest := cfg.Database + strings.Repeat("x", maxBaseNameLength-len(cfg.Database))
	tooLong := longest + "x"

	fits := cfg
	fits.Database = longest
	created := createTestDB(t, fits)
	assert.Len(t, created.Database, 63)

	var name string
	err := connect(t, created).NewRaw("SELECT current_database()").Scan(context.Background(), &name)
	require.NoError(t, err)
	assert.Equal(t, created.Database, name, "PostgreSQL kept the whole name")

	// 34 bytes that lowercase to 51
	growsWhenLowercased := strings.Repeat("Ⱥ", 17)

	for _, base := range []string{tooLong, growsWhenLowercased, ""} {
		rejected := cfg
		rejected.Database = base

		_, err = Create(rejected)
		require.Error(t, err)
		assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))

		err = Sweep(context.Background(), rejected, time.Hour)
		require.Error(t, err)
		assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	}
}

func TestDrop(t *testing.T) {
	cfg := testConfig(t)

	created := createTestDB(t, cfg)

	// A session still connected must not keep the database alive
	db := connect(t, created)
	err := db.Ping()
	require.NoError(t, err)

	err = Drop(created)
	require.NoError(t, err)
	assert.False(t, databaseExists(t, cfg, created.Database))

	err = Drop(created)
	assert.NoError(t, err)
}

func TestDropRefusesANameCreateDidNotGenerate(t *testing.T) {
	cfg := testConfig(t)

	// The mistake to survive: the base config passed where Create's belongs
	createNamedDB(t, cfg, cfg.Database)

	notInScheme := cfg.Database + "_keepme"
	createNamedDB(t, cfg, notInScheme)

	for _, name := range []string{cfg.Database, notInScheme} {
		refused := cfg
		refused.Database = name

		err := Drop(refused)
		require.Error(t, err)
		assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
		assert.True(t, databaseExists(t, cfg, name))
	}
}

func TestTruncate(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	db := connect(t, createTestDB(t, cfg))

	_, err := db.ExecContext(ctx, "CREATE TABLE parents (id bigserial PRIMARY KEY, name text NOT NULL)")
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, "CREATE TABLE children (id bigserial PRIMARY KEY, parent_id bigint NOT NULL REFERENCES parents (id))")
	require.NoError(t, err)

	// Names that only work quoted
	_, err = db.ExecContext(ctx, `CREATE TABLE "Mixed Case" (id bigserial PRIMARY KEY)`)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `CREATE TABLE "dotted.name" (id bigserial PRIMARY KEY)`)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `CREATE TABLE "quo""ted" (id bigserial PRIMARY KEY)`)
	require.NoError(t, err)

	// A table that belongs to an extension, as PostGIS's spatial_ref_sys does
	_, err = db.ExecContext(ctx, "CREATE TABLE extension_rows (id bigserial PRIMARY KEY)")
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, "ALTER EXTENSION plpgsql ADD TABLE extension_rows")
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, "INSERT INTO extension_rows DEFAULT VALUES")
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `INSERT INTO "dotted.name" DEFAULT VALUES`)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `INSERT INTO "quo""ted" DEFAULT VALUES`)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, "INSERT INTO parents (name) VALUES ('a'), ('b')")
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `INSERT INTO "Mixed Case" DEFAULT VALUES`)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, "INSERT INTO children (parent_id) VALUES (1), (2)")
	require.NoError(t, err)

	err = migrate.NewMigrator(db.DB, migrate.NewMigrations()).Init(ctx)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, "INSERT INTO bun_migrations (name, group_id) VALUES ('20240101_baseline', 1)")
	require.NoError(t, err)

	err = Truncate(ctx, db)
	require.NoError(t, err)

	assert.Equal(t, 0, countRows(t, db, "parents"))
	assert.Equal(t, 0, countRows(t, db, "children"))
	assert.Equal(t, 0, countRows(t, db, "Mixed Case"))
	assert.Equal(t, 0, countRows(t, db, `"dotted.name"`))
	assert.Equal(t, 0, countRows(t, db, `"quo""ted"`))
	assert.Equal(t, 1, countRows(t, db, "extension_rows"))
	assert.Equal(t, 1, countRows(t, db, "bun_migrations"))

	var id int64
	err = db.NewRaw("INSERT INTO parents (name) VALUES ('c') RETURNING id").Scan(ctx, &id)
	require.NoError(t, err)
	assert.Equal(t, int64(1), id)
}

func TestTruncateWithoutTables(t *testing.T) {
	cfg := testConfig(t)
	db := connect(t, createTestDB(t, cfg))

	err := Truncate(context.Background(), db)
	assert.NoError(t, err)
}

func TestSweep(t *testing.T) {
	cfg := testConfig(t)
	base := cfg.Database
	old := time.Now().Add(-2 * time.Hour)

	oldIdle := stampedName(t, base, old)
	createNamedDB(t, cfg, oldIdle)

	fresh := stampedName(t, base, time.Now())
	createNamedDB(t, cfg, fresh)

	oldInUse := stampedName(t, base, old)
	createNamedDB(t, cfg, oldInUse)
	inUse := cfg
	inUse.Database = oldInUse
	err := connect(t, inUse).Ping()
	require.NoError(t, err)

	notInScheme := base + "_keepme"
	createNamedDB(t, cfg, notInScheme)

	otherBase := stampedName(t, base+"_other", old)
	createNamedDB(t, cfg, otherBase)

	err = Sweep(context.Background(), cfg, time.Hour)
	require.NoError(t, err)

	assert.False(t, databaseExists(t, cfg, oldIdle), "old idle database")
	assert.True(t, databaseExists(t, cfg, fresh), "fresh database")
	assert.True(t, databaseExists(t, cfg, oldInUse), "old database with an open session")
	assert.True(t, databaseExists(t, cfg, notInScheme), "database outside the name scheme")
	assert.True(t, databaseExists(t, cfg, otherBase), "database under another base name")
}

func TestSweepCancelled(t *testing.T) {
	cfg := testConfig(t)

	oldIdle := stampedName(t, cfg.Database, time.Now().Add(-2*time.Hour))
	createNamedDB(t, cfg, oldIdle)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := Sweep(ctx, cfg, time.Hour)
	assert.Error(t, err)
	assert.True(t, databaseExists(t, cfg, oldIdle))
}

// A sweep whose context ends between the listing and the drops stops at the
// next name and reports that once
func TestDropStaleStopsOnceTheContextIsDone(t *testing.T) {
	cfg := testConfig(t)
	old := time.Now().Add(-2 * time.Hour)

	first := stampedName(t, cfg.Database, old)
	createNamedDB(t, cfg, first)

	second := stampedName(t, cfg.Database, old)
	createNamedDB(t, cfg, second)

	admin := cfg
	admin.Database = "postgres"
	db := connect(t, admin)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	pattern := regexp.MustCompile(`^` + regexp.QuoteMeta(cfg.Database) + `_([0-9]+)_[0-9a-f]{16}$`)

	err := dropStale(ctx, db, pattern, []string{first, second}, time.Now().Unix())
	require.Error(t, err)

	joined, ok := err.(interface{ Unwrap() []error })
	require.True(t, ok)
	assert.Len(t, joined.Unwrap(), 1, "the sweep stops instead of failing every remaining drop")

	assert.True(t, databaseExists(t, cfg, first))
	assert.True(t, databaseExists(t, cfg, second))
}
