package session

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"strings"
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

const testTable = "sessions_test"

// newSQLiteDB opens a WAL SQLite file under t.TempDir with a busy timeout, so
// concurrent writers wait instead of failing.
func newSQLiteDB(t *testing.T) *bun.DB {
	query := url.Values{}
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "journal_mode(WAL)")
	path := filepath.ToSlash(filepath.Join(t.TempDir(), "session.db"))
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
	testStore(t, newBunStore(t, newSQLiteDB(t)))
}

// testPostgresDSN is the database CI provides (see
// .github/workflows/test.yml); TEST_POSTGRES_DSN overrides it.
const testPostgresDSN = "postgres://postgres@localhost:5432/compose_test?sslmode=disable"

func TestBunStorePostgres(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		dsn = testPostgresDSN
	}
	sqldb := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	db := bun.NewDB(sqldb, pgdialect.New())

	// Without a reachable PostgreSQL the test skips so `go test ./...` stays
	// runnable on any machine. CI sets COMPOSE_TEST_POSTGRES, turning an
	// unreachable server into a hard failure so regressions cannot hide
	// behind skips.
	err := db.PingContext(context.Background())
	if err != nil {
		db.Close() // nolint:errcheck
		if os.Getenv("COMPOSE_TEST_POSTGRES") != "" {
			t.Fatalf("PostgreSQL testing is enabled but the database is unavailable: %v", err)
		}
		t.Skipf("PostgreSQL is not available: %v", err)
	}
	t.Cleanup(func() {
		db.NewDropTable().TableExpr("?", bun.Ident(testTable)).IfExists().Exec(context.Background()) // nolint:errcheck
		db.Close()                                                                                   // nolint:errcheck
	})

	_, err = db.NewDropTable().TableExpr("?", bun.Ident(testTable)).IfExists().Exec(context.Background())
	require.NoError(t, err)
	testStore(t, newBunStore(t, db))
}

// TestManagerOnSQLite: the conditional update lets exactly one of many
// concurrent requests rotate against a real database.
func TestManagerOnSQLite(t *testing.T) {
	ctx := context.Background()
	m, clock := newManager(t, Config{Store: newBunStore(t, newSQLiteDB(t))})
	testConcurrentResolve(t, ctx, m, clock)
}

func TestCreateTableIsIdempotent(t *testing.T) {
	ctx := context.Background()
	db := newSQLiteDB(t)
	store := newBunStore(t, db)

	r := newRecord(t, "kept")
	require.NoError(t, store.Insert(ctx, r))

	err := CreateTable(ctx, db, testTable)
	require.NoError(t, err)

	got, err := store.FindByHash(ctx, r.TokenHash)
	require.NoError(t, err)
	assertRecord(t, r, got)
}

func TestCreateTableIndexes(t *testing.T) {
	ctx := context.Background()
	db := newSQLiteDB(t)
	store := newBunStore(t, db)

	for _, column := range []string{"token_hash", "previous_hash", "subject", "expires_at", "last_used_at"} {
		var table, ddl string
		err := db.NewSelect().
			TableExpr("sqlite_master").
			ColumnExpr("tbl_name, sql").
			Where("type = ?", "index").
			Where("name = ?", testTable+"_"+column).
			Scan(ctx, &table, &ddl)
		require.NoError(t, err, column)
		assert.Equal(t, testTable, table)
		assert.Contains(t, ddl, `("`+column+`")`)
		assert.Equal(t, column == "token_hash", strings.HasPrefix(ddl, "CREATE UNIQUE"), column)
	}

	r := newRecord(t, "dup")
	require.NoError(t, store.Insert(ctx, r))
	dup := newRecord(t, "dup2")
	dup.TokenHash = r.TokenHash
	err := store.Insert(ctx, dup)
	assert.Equal(t, ez.EINTERNAL, ez.ErrorCode(err), "token_hash is unique")
}

// TestCreateTableDotted: a schema-qualified table gets its indexes in the
// same schema, as SQLite wants them named.
func TestCreateTableDotted(t *testing.T) {
	ctx := context.Background()
	db := newSQLiteDB(t)
	err := CreateTable(ctx, db, "main.sessions_dotted")
	require.NoError(t, err)

	var tables []string
	err = db.NewSelect().
		TableExpr("main.sqlite_master").
		ColumnExpr("tbl_name").
		Where("type = ?", "index").
		Where("name IN (?)", bun.In([]string{"sessions_dotted_token_hash", "sessions_dotted_previous_hash", "sessions_dotted_subject", "sessions_dotted_expires_at", "sessions_dotted_last_used_at"})).
		Scan(ctx, &tables)
	require.NoError(t, err)
	assert.Equal(t, []string{"sessions_dotted", "sessions_dotted", "sessions_dotted", "sessions_dotted", "sessions_dotted"}, tables)

	err = CreateTable(ctx, db, "main.sessions_dotted")
	require.NoError(t, err, "CreateTable is idempotent on a dotted name")
	store := NewBunStore(db, "main.sessions_dotted")
	r := newRecord(t, "dotted")
	require.NoError(t, store.Insert(ctx, r))
	_, err = store.FindByHash(ctx, r.TokenHash)
	require.NoError(t, err)
}

func TestIndexParts(t *testing.T) {
	cases := map[string][2]string{
		"x":      {"", "x"},
		"main.x": {"main", "x"},
		"a.b.c":  {"a.b", "c"},
	}
	for table, want := range cases {
		qualifier, name := indexParts(table)
		assert.Equal(t, want[0], qualifier, table)
		assert.Equal(t, want[1], name, table)
	}
}

func TestBunStoreMissingTable(t *testing.T) {
	ctx := context.Background()
	store := NewBunStore(newSQLiteDB(t), "no_such_table")
	r := newRecord(t, "missing")

	err := store.Insert(ctx, r)
	assert.Equal(t, ez.EINTERNAL, ez.ErrorCode(err))
	_, err = store.FindByHash(ctx, r.TokenHash)
	assert.Equal(t, ez.EINTERNAL, ez.ErrorCode(err))
	err = store.Update(ctx, r)
	assert.Equal(t, ez.EINTERNAL, ez.ErrorCode(err))
	err = store.Delete(ctx, r.ID)
	assert.Equal(t, ez.EINTERNAL, ez.ErrorCode(err))
	err = store.DeleteRevision(ctx, r.ID, r.Revision)
	assert.Equal(t, ez.EINTERNAL, ez.ErrorCode(err))
	err = store.DeleteBySubject(ctx, r.Subject)
	assert.Equal(t, ez.EINTERNAL, ez.ErrorCode(err))
	_, err = store.DeleteExpired(ctx, time.Now(), time.Now())
	assert.Equal(t, ez.EINTERNAL, ez.ErrorCode(err))
}

// queryLog records the SQL of every query run on a bun database.
type queryLog struct {
	queries []string
}

func (l *queryLog) BeforeQuery(ctx context.Context, event *bun.QueryEvent) context.Context {
	return ctx
}

func (l *queryLog) AfterQuery(ctx context.Context, event *bun.QueryEvent) {
	l.queries = append(l.queries, event.Query)
}

// TestBunStoreUpdateIsOneConditionalWrite: Update is a single UPDATE that
// bumps the revision and matches only the revision it was given.
func TestBunStoreUpdateIsOneConditionalWrite(t *testing.T) {
	ctx := context.Background()
	db := newSQLiteDB(t)
	store := newBunStore(t, db)
	r := newRecord(t, "sql")
	require.NoError(t, store.Insert(ctx, r))

	log := &queryLog{}
	db.AddQueryHook(log)
	err := store.Update(ctx, r)
	require.NoError(t, err)

	require.Len(t, log.queries, 1)
	query := log.queries[0]
	assert.True(t, strings.HasPrefix(query, `UPDATE "`+testTable+`" SET `), query)
	assert.True(t, strings.HasSuffix(query, `"revision" = 1 WHERE (id = '`+r.ID+`') AND (revision = 0)`), query)
}

// TestBunStoreDeleteRevisionIsOneConditionalWrite: DeleteRevision is a single
// DELETE that matches only the revision it was given.
func TestBunStoreDeleteRevisionIsOneConditionalWrite(t *testing.T) {
	ctx := context.Background()
	db := newSQLiteDB(t)
	store := newBunStore(t, db)
	r := newRecord(t, "sql")
	require.NoError(t, store.Insert(ctx, r))

	log := &queryLog{}
	db.AddQueryHook(log)
	err := store.DeleteRevision(ctx, r.ID, r.Revision)
	require.NoError(t, err)

	require.Len(t, log.queries, 1)
	assert.Equal(t, `DELETE FROM "`+testTable+`" WHERE (id = '`+r.ID+`') AND (revision = 0)`, log.queries[0])
}

// TestBunStoreDeleteExpiredIsOneDelete: DeleteExpired is a single DELETE,
// with the idle rule only when idleBefore is set.
func TestBunStoreDeleteExpiredIsOneDelete(t *testing.T) {
	ctx := context.Background()
	db := newSQLiteDB(t)
	store := newBunStore(t, db)
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

	log := &queryLog{}
	db.AddQueryHook(log)
	_, err := store.DeleteExpired(ctx, now, now.Add(-time.Hour))
	require.NoError(t, err)
	_, err = store.DeleteExpired(ctx, now, time.Time{})
	require.NoError(t, err)

	require.Len(t, log.queries, 2)
	assert.Equal(t, `DELETE FROM "`+testTable+`" WHERE (expires_at IS NOT NULL AND expires_at <= '2026-10-22 12:00:00+00:00') OR (last_used_at <= '2026-10-22 11:00:00+00:00')`, log.queries[0])
	assert.Equal(t, `DELETE FROM "`+testTable+`" WHERE (expires_at IS NOT NULL AND expires_at <= '2026-10-22 12:00:00+00:00')`, log.queries[1])
}
