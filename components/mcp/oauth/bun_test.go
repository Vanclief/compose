package oauth

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/dialect/sqlitedialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/vanclief/ez"

	// Registers the cgo-free "sqlite" database/sql driver
	_ "modernc.org/sqlite"
)

const testTable = "oauth_records_test"

// newSQLiteDB opens a WAL SQLite file under t.TempDir with a busy timeout, so
// concurrent writers wait instead of failing.
func newSQLiteDB(t *testing.T) *bun.DB {
	query := url.Values{}
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "journal_mode(WAL)")
	path := filepath.ToSlash(filepath.Join(t.TempDir(), "oauth.db"))
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: query.Encode()}).String()

	sqldb, err := sql.Open("sqlite", dsn)
	require.NoError(t, err)
	db := bun.NewDB(sqldb, sqlitedialect.New())
	t.Cleanup(func() {
		db.Close() // nolint:errcheck
	})
	return db
}

// newBunStore creates the test table on db and returns the store over it.
func newBunStore(t *testing.T, db *bun.DB) Store {
	err := CreateTable(context.Background(), db, testTable)
	require.NoError(t, err)
	return NewBunStore(db, testTable)
}

func TestBunStoreSQLite(t *testing.T) {
	db := newSQLiteDB(t)
	testStore(t, newBunStore(t, db))
}

func TestBunStorePostgres(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not set")
	}
	sqldb := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	db := bun.NewDB(sqldb, pgdialect.New())
	t.Cleanup(func() {
		db.NewDropTable().TableExpr("?", bun.Ident(testTable)).IfExists().Exec(context.Background()) // nolint:errcheck
		db.Close()                                                                                   // nolint:errcheck
	})

	_, err := db.NewDropTable().TableExpr("?", bun.Ident(testTable)).IfExists().Exec(context.Background())
	require.NoError(t, err)
	testStore(t, newBunStore(t, db))
}

func TestCreateTableIsIdempotent(t *testing.T) {
	ctx := context.Background()
	db := newSQLiteDB(t)
	store := newBunStore(t, db)

	err := store.Put(ctx, KindClient, "kept", []byte("v"), time.Now().Add(time.Hour))
	require.NoError(t, err)

	err = CreateTable(ctx, db, testTable)
	require.NoError(t, err)

	got, err := store.Get(ctx, KindClient, "kept")
	require.NoError(t, err)
	assert.Equal(t, []byte("v"), got)
}

func TestCreateTableIndex(t *testing.T) {
	ctx := context.Background()
	db := newSQLiteDB(t)
	newBunStore(t, db)

	var table, ddl string
	err := db.NewSelect().
		TableExpr("sqlite_master").
		ColumnExpr("tbl_name, sql").
		Where("type = ?", "index").
		Where("name = ?", testTable+"_expires_at").
		Scan(ctx, &table, &ddl)
	require.NoError(t, err)
	assert.Equal(t, testTable, table)
	assert.Contains(t, ddl, `("expires_at")`)
}

func TestBunStoreMissingTable(t *testing.T) {
	ctx := context.Background()
	store := NewBunStore(newSQLiteDB(t), "no_such_table")

	_, err := store.Get(ctx, KindClient, "id")
	assert.Equal(t, ez.EINTERNAL, ez.ErrorCode(err))
	_, err = store.Take(ctx, KindClient, "id")
	assert.Equal(t, ez.EINTERNAL, ez.ErrorCode(err))
	err = store.Put(ctx, KindClient, "id", []byte("v"), time.Now().Add(time.Hour))
	assert.Equal(t, ez.EINTERNAL, ez.ErrorCode(err))
}

func TestSweep(t *testing.T) {
	ctx := context.Background()
	db := newSQLiteDB(t)
	store := newBunStore(t, db)

	err := store.Put(ctx, KindAccess, "expired-1", []byte("a"), time.Now().Add(-time.Hour))
	require.NoError(t, err)
	err = store.Put(ctx, KindRefresh, "expired-2", []byte("b"), time.Now().Add(-time.Millisecond))
	require.NoError(t, err)
	err = store.Put(ctx, KindAccess, "live", []byte("c"), time.Now().Add(time.Hour))
	require.NoError(t, err)

	count, err := Sweep(ctx, db, testTable)
	require.NoError(t, err)
	assert.Equal(t, int64(2), count)

	var rows int
	err = db.NewSelect().TableExpr("?", bun.Ident(testTable)).ColumnExpr("count(*)").Scan(ctx, &rows)
	require.NoError(t, err)
	assert.Equal(t, 1, rows)

	got, err := store.Get(ctx, KindAccess, "live")
	require.NoError(t, err)
	assert.Equal(t, []byte("c"), got)

	count, err = Sweep(ctx, db, testTable)
	require.NoError(t, err)
	assert.Equal(t, int64(0), count)
}
