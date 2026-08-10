package sqlite

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/suite"
	"github.com/uptrace/bun"
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
	// Characters that would be misread as DSN parameters if unescaped,
	// restricted to ones every platform allows in filenames (the '?' case
	// is covered by TestDatabaseDSN, since Windows forbids it on disk)
	dir := filepath.Join(suite.T().TempDir(), "weird dir#1")
	path := filepath.Join(dir, "agc #1%.db")

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

func (suite *TestSuite) TestDatabaseDSN() {
	// Unix paths pass through with pragmas attached
	dsn := databaseDSN("/tmp/data/test.db", 5000)
	suite.Equal(
		"file:///tmp/data/test.db?_pragma=busy_timeout%285000%29&_pragma=journal_mode%28WAL%29&_pragma=foreign_keys%281%29&_pragma=synchronous%28NORMAL%29",
		dsn,
	)

	// A Windows drive-letter path gains the leading slash SQLite URIs
	// require, instead of the drive being misread as a URI authority
	dsn = databaseDSN("C:/Users/franco/agc.db", 5000)
	suite.True(strings.HasPrefix(dsn, "file:///C:/Users/franco/agc.db?"), dsn)

	// URI-hostile characters are escaped, not parsed
	dsn = databaseDSN("/tmp/weird dir#1/agc?.db", 5000)
	suite.True(strings.HasPrefix(dsn, "file:///tmp/weird%20dir%231/agc%3F.db?"), dsn)
}

func (suite *TestSuite) TestConnectToDatabaseRequiresPath() {
	db, err := ConnectToDatabase(&ConnectionConfig{})
	suite.Error(err)
	suite.Nil(db)
}

func (suite *TestSuite) TestConnectToDatabaseRejectsNegativeBusyTimeout() {
	path := filepath.Join(suite.T().TempDir(), "test.db")
	db, err := ConnectToDatabase(&ConnectionConfig{
		Path:        path,
		BusyTimeout: -1,
	})
	suite.Error(err)
	suite.Nil(db)
}

func (suite *TestSuite) TestConnectToDatabaseMaxOpenConns() {
	path := filepath.Join(suite.T().TempDir(), "test.db")
	db, err := ConnectToDatabase(&ConnectionConfig{
		Path:         path,
		MaxOpenConns: 1,
	})
	suite.Require().NoError(err)
	defer db.Close() // nolint:errcheck

	suite.Equal(1, db.Stats().MaxOpenConnections)
}

func (suite *TestSuite) TestConnectToDatabaseRejectsNegativeMaxOpenConns() {
	path := filepath.Join(suite.T().TempDir(), "test.db")
	db, err := ConnectToDatabase(&ConnectionConfig{
		Path:         path,
		MaxOpenConns: -1,
	})
	suite.Error(err)
	suite.Nil(db)
}

func (suite *TestSuite) TestCreateExtensionsNotSupported() {
	db := suite.newFileDB()
	defer db.Close() // nolint:errcheck

	// Requesting no extensions is fine on any dialect
	err := db.CreateExtensions([]string{})
	suite.NoError(err)

	// Requesting one on SQLite must fail loudly instead of no-oping
	err = db.CreateExtensions([]string{"uuid-ossp"})
	suite.Error(err)
	suite.Equal(ez.ENOTIMPLEMENTED, ez.ErrorCode(err))
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

func (suite *TestSuite) TestCreateTables() {
	db := suite.newFileDB()
	defer db.Close() // nolint:errcheck

	err := db.CreateTables(testModels())
	suite.Require().NoError(err)
	suite.True(suite.tableExists(db, "test_items"))

	// Idempotent on re-run
	err = db.CreateTables(testModels())
	suite.NoError(err)

	// The created schema is usable, including JSON and time round-trips
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

func (suite *TestSuite) TestResetTables() {
	db := suite.newFileDB()
	defer db.Close() // nolint:errcheck

	err := db.CreateTables(testModels())
	suite.Require().NoError(err)

	ctx := context.Background()
	_, err = db.NewInsert().Model(&testItem{Name: "before reset"}).Exec(ctx)
	suite.Require().NoError(err)

	err = db.ResetTables(testModels())
	suite.NoError(err)

	// The table survives empty rather than staying dropped
	suite.True(suite.tableExists(db, "test_items"))

	count, err := db.NewSelect().Model((*testItem)(nil)).Count(ctx)
	suite.NoError(err)
	suite.Equal(0, count)
}
