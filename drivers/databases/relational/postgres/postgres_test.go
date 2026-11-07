package postgres

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
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
