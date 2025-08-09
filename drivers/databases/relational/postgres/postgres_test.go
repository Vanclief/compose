package postgres

import (
	"testing"

	"github.com/stretchr/testify/suite"
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
		panic(err)
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

func (suite *TestSuite) TestCreateTables(models []interface{}) {
	err := suite.db.CreateTables(models)
	suite.Nil(err)
}

func (suite *TestSuite) TestResetTables(models []interface{}) {
	err := suite.db.ResetTables(models)
	suite.Nil(err)
}
