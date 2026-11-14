package postgres

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"github.com/uptrace/bun"
	"github.com/vanclief/compose/drivers/databases/relational"
	"github.com/vanclief/ez"
)

type TestSuite struct {
	suite.Suite
	db *relational.DB
}

func (suite *TestSuite) SetupTest() {
	cfg := &ConnectionConfig{
		Username: "postgres",
		Password: "",
		Host:     "localhost:5432",
		Database: "compose_test",
	}

	// Without a local PostgreSQL the suite skips so `go test ./...` stays
	// runnable on any machine. CI sets COMPOSE_TEST_POSTGRES (see
	// .github/workflows/test.yml, which provides the database), turning an
	// unreachable server into a hard failure so regressions cannot hide
	// behind skips.
	db, err := ConnectToDatabase(cfg)
	if err != nil {
		if os.Getenv("COMPOSE_TEST_POSTGRES") != "" {
			suite.T().Fatalf("PostgreSQL testing is enabled but the database is unavailable: %v", err)
		}

		suite.T().Skipf("PostgreSQL is not available: %v", err)
	}

	suite.db = db
}

func TestSuiteRun(t *testing.T) {
	suite.Run(t, new(TestSuite))
}

func (suite *TestSuite) TestConnectToDatabase() {
	cfg := &ConnectionConfig{
		Username: "postgres",
		Password: "",
		Host:     "localhost:5432",
		Database: "compose_test",
	}

	db, err := ConnectToDatabase(cfg)
	suite.Nil(err)
	suite.NotNil(db)
}

func (suite *TestSuite) TestConnectToDatabaseMaxOpenConns() {
	cfg := &ConnectionConfig{
		Username:     "postgres",
		Password:     "",
		Host:         "localhost:5432",
		Database:     "compose_test",
		MaxOpenConns: 3,
		MaxIdleConns: 2,
	}

	db, err := ConnectToDatabase(cfg)
	suite.Require().NoError(err)
	defer db.Close() // nolint:errcheck

	suite.Equal(3, db.Stats().MaxOpenConnections)
}

// The negative-value checks run outside the suite because they must not need
// a database: SetupTest skips the whole suite when PostgreSQL is unreachable.
// The host points at a closed port so a dial attempt would surface as a
// connection error instead of EINVALID.
func TestConnectToDatabaseRejectsNegativeMaxOpenConns(t *testing.T) {
	db, err := ConnectToDatabase(&ConnectionConfig{
		Host:         "127.0.0.1:1",
		MaxOpenConns: -1,
	})
	assert.Error(t, err)
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	assert.Nil(t, db)
}

func TestConnectToDatabaseRejectsNegativeMaxIdleConns(t *testing.T) {
	db, err := ConnectToDatabase(&ConnectionConfig{
		Host:         "127.0.0.1:1",
		MaxIdleConns: -1,
	})
	assert.Error(t, err)
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	assert.Nil(t, db)
}

func TestConnectToDatabaseRejectsNegativeMigrationLockTimeout(t *testing.T) {
	db, err := ConnectToDatabase(&ConnectionConfig{
		Host:                 "127.0.0.1:1",
		MigrationLockTimeout: -1,
	})
	assert.Error(t, err)
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	assert.Nil(t, db)
}

type testRecord struct {
	bun.BaseModel `bun:"table:test_records"`

	ID   int64  `bun:",pk,autoincrement"`
	Name string `bun:",notnull"`
}

func (suite *TestSuite) TestCreateTables() {
	ctx := context.Background()
	models := []interface{}{(*testRecord)(nil)}

	defer func() {
		_, _ = suite.db.NewDropTable().Model((*testRecord)(nil)).IfExists().Exec(ctx)
	}()

	err := suite.db.CreateTables(models)
	suite.NoError(err)

	// Idempotent on re-run
	err = suite.db.CreateTables(models)
	suite.NoError(err)
}

type testNote struct {
	bun.BaseModel `bun:"table:test_notes"`

	ID   int64  `bun:",pk,autoincrement"`
	Text string `bun:",notnull"`
}

// TestCreateTablesConcurrently: instances booting together against a database
// without the tables all create them without an error.
func TestCreateTablesConcurrently(t *testing.T) {
	ctx := context.Background()
	const rounds = 50
	const instances = 8
	cfg := baselineConfig(t)
	cfg.MaxOpenConns = instances
	// Keeps the connections of a round open for the next, so the instances
	// of every round start together instead of dialing one by one
	cfg.MaxIdleConns = instances
	db, _ := connectTestDB(t, cfg)
	models := []interface{}{(*testRecord)(nil), (*testNote)(nil)}

	failed := 0
	for round := 0; round < rounds; round++ {
		for _, model := range models {
			_, err := db.NewDropTable().Model(model).IfExists().Exec(ctx)
			require.NoError(t, err)
		}

		start := make(chan struct{})
		errs := make(chan error, instances)
		var wg sync.WaitGroup
		for i := 0; i < instances; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs <- db.CreateTables(models)
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				failed++
				t.Errorf("round %d: %v", round, err)
				break
			}
		}
	}
	assert.Zero(t, failed, "rounds with a failed CreateTables")
}

type testIndexed struct {
	bun.BaseModel `bun:"table:test_indexed"`

	ID   int64  `bun:",pk,autoincrement"`
	Name string `bun:",notnull"`
}

var _ bun.AfterCreateTableHook = (*testIndexed)(nil)

// AfterCreateTable creates an index through query.DB(), the way bun documents
// for a model's create-table hook
func (*testIndexed) AfterCreateTable(ctx context.Context, query *bun.CreateTableQuery) error {
	_, err := query.DB().NewCreateIndex().
		Model((*testIndexed)(nil)).
		Index("test_indexed_name_idx").
		Column("name").
		IfNotExists().
		Exec(ctx)
	if err != nil {
		return ez.Wrap(err)
	}

	return nil
}

// TestCreateTablesAfterCreateTableHook: a model whose AfterCreateTable hook
// runs a statement through query.DB() gets its table and the hook's index, on
// a pool of several connections and on a pool of one.
func TestCreateTablesAfterCreateTableHook(t *testing.T) {
	for _, conns := range []int{4, 1} {
		t.Run(fmt.Sprintf("%d connections", conns), func(t *testing.T) {
			ctx := context.Background()
			cfg := baselineConfig(t)
			cfg.MaxOpenConns = conns
			db, _ := connectTestDB(t, cfg)

			// A hook waiting for a second connection of a one-connection pool
			// would block forever, so a hang fails the test instead
			done := make(chan error, 1)
			go func() {
				done <- db.CreateTables([]interface{}{(*testIndexed)(nil)})
			}()
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(10 * time.Second):
				t.Fatal("CreateTables did not return within 10 s")
			}

			var exists bool
			err := db.NewRaw("SELECT to_regclass('test_indexed_name_idx') IS NOT NULL").Scan(ctx, &exists)
			require.NoError(t, err)
			assert.True(t, exists, "the hook's index exists")
		})
	}
}

type testMultiIndexed struct {
	bun.BaseModel `bun:"table:test_multi_indexed"`

	ID   int64  `bun:",pk,autoincrement"`
	Name string `bun:",notnull"`
	Code string `bun:",notnull"`
	Note string `bun:",notnull"`
}

var _ bun.AfterCreateTableHook = (*testMultiIndexed)(nil)

// AfterCreateTable creates three indexes through query.DB(), so an instance
// that loses the race to create the table can lose it again on each index
func (*testMultiIndexed) AfterCreateTable(ctx context.Context, query *bun.CreateTableQuery) error {
	for _, column := range []string{"name", "code", "note"} {
		_, err := query.DB().NewCreateIndex().
			Model((*testMultiIndexed)(nil)).
			Index("test_multi_indexed_" + column + "_idx").
			Column(column).
			IfNotExists().
			Exec(ctx)
		if err != nil {
			return ez.Wrap(err)
		}
	}

	return nil
}

// runTogether - Runs call from instances goroutines that start at once, in
// each of rounds rounds after reset, and fails the test when any call fails
func runTogether(t *testing.T, rounds, instances int, reset func(), call func() error) {
	failedRounds := 0
	failedCalls := 0
	for round := 0; round < rounds; round++ {
		reset()

		start := make(chan struct{})
		errs := make(chan error, instances)
		var wg sync.WaitGroup
		for i := 0; i < instances; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs <- call()
			}()
		}
		close(start)
		wg.Wait()
		close(errs)

		failed := 0
		for err := range errs {
			if err == nil {
				continue
			}
			if failed == 0 {
				t.Errorf("round %d: %v", round, err)
			}
			failed++
		}
		if failed > 0 {
			failedRounds++
			failedCalls += failed
		}
	}
	assert.Zero(t, failedRounds, "%d of %d rounds failed, %d of %d calls", failedRounds, rounds, failedCalls, rounds*instances)
}

// TestCreateTablesConcurrentlyWithHookIndexes: instances booting together
// all create a model whose AfterCreateTable hook creates several indexes
func TestCreateTablesConcurrentlyWithHookIndexes(t *testing.T) {
	ctx := context.Background()
	const instances = 8
	cfg := baselineConfig(t)
	cfg.MaxOpenConns = instances
	cfg.MaxIdleConns = instances
	db, _ := connectTestDB(t, cfg)
	models := []interface{}{(*testMultiIndexed)(nil)}

	reset := func() {
		_, err := db.NewDropTable().Model((*testMultiIndexed)(nil)).IfExists().Exec(ctx)
		require.NoError(t, err)
	}
	runTogether(t, 50, instances, reset, func() error {
		return db.CreateTables(models)
	})
}

// TestCreateExtensionsConcurrently: instances booting together all create an
// extension the database does not have yet
func TestCreateExtensionsConcurrently(t *testing.T) {
	ctx := context.Background()
	const instances = 8
	cfg := baselineConfig(t)
	cfg.MaxOpenConns = instances
	cfg.MaxIdleConns = instances
	db, _ := connectTestDB(t, cfg)

	var available bool
	err := db.NewRaw("SELECT EXISTS (SELECT 1 FROM pg_available_extensions WHERE name = 'pg_trgm')").Scan(ctx, &available)
	require.NoError(t, err)
	if !available {
		t.Skip("The pg_trgm extension is not installed on this PostgreSQL server")
	}

	reset := func() {
		_, err := db.ExecContext(ctx, "DROP EXTENSION IF EXISTS pg_trgm")
		require.NoError(t, err)
	}
	runTogether(t, 50, instances, reset, func() error {
		return db.CreateExtensions([]string{"pg_trgm"})
	})
}

func (suite *TestSuite) TestResetTables() {
	ctx := context.Background()
	models := []interface{}{(*testRecord)(nil)}

	defer func() {
		_, _ = suite.db.NewDropTable().Model((*testRecord)(nil)).IfExists().Exec(ctx)
	}()

	err := suite.db.CreateTables(models)
	suite.Require().NoError(err)

	_, err = suite.db.NewInsert().Model(&testRecord{Name: "before reset"}).Exec(ctx)
	suite.Require().NoError(err)

	err = suite.db.ResetTables(models)
	suite.NoError(err)

	count, err := suite.db.NewSelect().Model((*testRecord)(nil)).Count(ctx)
	suite.NoError(err)
	suite.Equal(int64(0), count)
}

type testDocument struct {
	bun.BaseModel `bun:"table:test_documents"`

	ID   int64             `bun:",pk,autoincrement"`
	Data map[string]string `bun:"type:jsonb"`
}

// TestJSONWithEscapedBackslashRoundTrips covers jsonfmt.AppendJSON through
// ConnectToDatabase: bun v1.3.0's own formatter renders the text \u0000 inside
// a JSON value as \\\u0000, which Postgres rejects for jsonb.
func (suite *TestSuite) TestJSONWithEscapedBackslashRoundTrips() {
	ctx := context.Background()
	models := []interface{}{(*testDocument)(nil)}

	err := suite.db.ResetTables(models)
	suite.Require().NoError(err)

	doc := &testDocument{Data: map[string]string{"text": `x\u0000y`, "quote": `it's \'`, "nul": "a\x00b"}}
	_, err = suite.db.NewInsert().Model(doc).Exec(ctx)
	suite.Require().NoError(err)

	var fetched testDocument
	err = suite.db.NewSelect().Model(&fetched).Where("id = ?", doc.ID).Scan(ctx)
	suite.Require().NoError(err)
	suite.Equal(`x\u0000y`, fetched.Data["text"])
	suite.Equal(`it's \'`, fetched.Data["quote"])
	suite.Equal(`a\u0000b`, fetched.Data["nul"], "a NUL byte is stored as the text of its escape, as bun does")
}
