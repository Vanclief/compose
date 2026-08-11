package postgres

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/suite"
	"github.com/uptrace/bun"
	"github.com/vanclief/compose/drivers/databases/relational"
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
	suite.Equal(0, count)
}
