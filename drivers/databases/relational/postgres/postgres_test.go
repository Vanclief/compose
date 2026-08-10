package postgres

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/suite"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/migrate"
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

func (suite *TestSuite) TestInitSchemaFresh() {
	ctx := context.Background()
	models := []interface{}{(*testRecord)(nil)}

	cleanup := func() {
		_, _ = suite.db.NewDropTable().Model((*testRecord)(nil)).IfExists().Exec(ctx)
		_, _ = suite.db.NewRaw("DROP TABLE IF EXISTS bun_migrations").Exec(ctx)
		_, _ = suite.db.NewRaw("DROP TABLE IF EXISTS bun_migration_locks").Exec(ctx)
	}
	cleanup()
	defer cleanup()

	migrations := migrate.NewMigrations()
	migrations.Add(migrate.Migration{Name: "00000001"})

	err := suite.db.InitSchema(models, migrations)
	suite.Require().NoError(err)

	// The schema comes from the structs and the migration is baselined
	migrator := migrate.NewMigrator(suite.db.DB, migrations)
	applied, err := migrator.AppliedMigrations(ctx)
	suite.NoError(err)
	suite.Len(applied, 1)

	// The created schema is usable, and a second boot is a no-op
	_, err = suite.db.NewInsert().Model(&testRecord{Name: "fresh"}).Exec(ctx)
	suite.NoError(err)

	err = suite.db.InitSchema(models, migrations)
	suite.NoError(err)
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
