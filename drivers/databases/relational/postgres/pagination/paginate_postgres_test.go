package pagination

import (
	"context"
	"database/sql/driver"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/vanclief/compose/drivers/databases/relational"
	"github.com/vanclief/compose/drivers/databases/relational/postgres"
	"github.com/vanclief/compose/drivers/databases/relational/postgres/pgtest"
	"github.com/vanclief/ez"
)

// code is text in Go, a driver.Valuer whose column has its own type, such as
// a uuid.
type code string

func (c code) Value() (driver.Value, error) {
	return string(c), nil
}

// pgRecord sorts by created_at by default, with id as the tie-breaker.
type pgRecord struct {
	bun.BaseModel `bun:"table:pagination_records,alias:p"`

	ID        int64     `bun:"id,pk"`
	Name      string    `bun:"name,notnull"`
	Rank      int64     `bun:"rank,notnull"`
	CreatedAt time.Time `bun:"created_at,notnull"`
	Code      code      `bun:"code,type:uuid,notnull"`
}

func (r pgRecord) GetSortField() string        { return "?TableAlias.created_at" }
func (r pgRecord) GetSortValue() interface{}   { return r.CreatedAt }
func (r pgRecord) GetUniqueField() string      { return "?TableAlias.id" }
func (r pgRecord) GetUniqueValue() interface{} { return r.ID }

var pgRecordSortFields = map[string]string{
	"name":       "name",
	"rank":       "rank",
	"created_at": "created_at",
	"code":       "code",
}

// createPostgresDB creates an empty database for the test with pgtest, in the
// UTC time zone, and drops it when the test ends. Each of settings, such as
// "lc_messages TO 'es_MX.UTF-8'", is set on the database before connecting,
// and the test skips when the server refuses one. It skips when PostgreSQL is
// unreachable, unless COMPOSE_TEST_POSTGRES is set.
func createPostgresDB(t *testing.T, settings ...string) *relational.DB {
	t.Helper()

	cfg := postgres.ConnectionConfig{
		Username:     "postgres",
		Host:         "localhost:5432",
		Database:     "postgres",
		MaxOpenConns: 2,
	}

	admin, err := postgres.ConnectToDatabase(&cfg)
	if err != nil {
		if os.Getenv("COMPOSE_TEST_POSTGRES") != "" {
			t.Fatalf("PostgreSQL testing is enabled but the database is unavailable: %v", err)
		}

		t.Skipf("PostgreSQL is not available: %v", err)
	}
	defer admin.Close() // nolint:errcheck

	cfg.Database = "compose_pagination"
	created, err := pgtest.Create(cfg)
	if err != nil {
		t.Fatalf("create test database: %v", err)
	}

	t.Cleanup(func() {
		err := pgtest.Drop(created)
		if err != nil {
			t.Errorf("drop test database: %v", err)
		}
	})

	ctx := context.Background()

	// pgdriver cannot read a year-1 time the server writes in a zone with a
	// seconds offset, such as America/Mexico_City's 0001-12-31 17:23:24-06:36:36 BC
	_, err = admin.ExecContext(ctx, "ALTER DATABASE ? SET timezone TO 'UTC'", bun.Ident(created.Database))
	if err != nil {
		t.Fatalf("set the test database time zone: %v", err)
	}

	for _, setting := range settings {
		_, err = admin.ExecContext(ctx, "ALTER DATABASE ? SET "+setting, bun.Ident(created.Database))
		if err != nil {
			t.Skipf("set %s on the test database: %v", setting, err)
		}
	}

	db, err := postgres.ConnectToDatabase(&created)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}

	// Registered after the drop, so it runs before it
	t.Cleanup(func() {
		db.Close() // nolint:errcheck
	})

	return db
}

// newPostgresDB returns a test database holding the pagination_records rows.
func newPostgresDB(t *testing.T) *relational.DB {
	t.Helper()

	db := createPostgresDB(t)
	ctx := context.Background()

	_, err := db.NewCreateTable().Model((*pgRecord)(nil)).Exec(ctx)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}

	base := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	records := []pgRecord{
		{ID: 1, Name: "d", Rank: 3, CreatedAt: base.Add(4 * time.Minute), Code: "00000000-0000-0000-0000-000000000005"},
		{ID: 2, Name: "b", Rank: 1, CreatedAt: base.Add(2 * time.Minute), Code: "00000000-0000-0000-0000-000000000003"},
		{ID: 3, Name: "e", Rank: 3, CreatedAt: base.Add(2 * time.Minute), Code: "00000000-0000-0000-0000-000000000001"},
		{ID: 4, Name: "a", Rank: 2, CreatedAt: base.Add(1 * time.Minute), Code: "00000000-0000-0000-0000-000000000004"},
		{ID: 5, Name: "c", Rank: 1, CreatedAt: base.Add(3 * time.Minute), Code: "00000000-0000-0000-0000-000000000002"},
	}

	_, err = db.NewInsert().Model(&records).Exec(ctx)
	if err != nil {
		t.Fatalf("insert records: %v", err)
	}

	return db
}

// pageAll pages through pagination_records two rows at a time and returns
// the ids in the order the pages listed them.
func pageAll(t *testing.T, db *relational.DB, sort string) []int64 {
	t.Helper()

	var ids []int64
	request := &CursorRequest{Limit: 2, Sort: sort}
	for range 10 {
		items := []pgRecord{}
		page, resp, err := Paginate(context.Background(), db.NewSelect().Model(&items), request, pgRecord{}, Options{Direction: DESC, SortFields: pgRecordSortFields})
		if err != nil {
			t.Fatalf("paginate with cursor %q: %v", request.Cursor, err)
		}

		for _, item := range page {
			ids = append(ids, item.ID)
		}

		if !resp.HasNextPage {
			return ids
		}
		request.Cursor = resp.NextCursor
	}

	t.Fatalf("paging %q did not end, listed %v", sort, ids)
	return nil
}

func TestPaginatePostgresPagesWithIssuedCursors(t *testing.T) {
	db := newPostgresDB(t)

	tests := []struct {
		name string
		sort string
		want []int64
	}{
		{name: "default time sort", want: []int64{1, 5, 3, 2, 4}},
		{name: "string sort", sort: "name", want: []int64{4, 2, 5, 1, 3}},
		{name: "integer sort", sort: "-rank", want: []int64{3, 1, 4, 5, 2}},
		{name: "time sort", sort: "created_at", want: []int64{4, 2, 3, 5, 1}},
		{name: "Valuer sort", sort: "code", want: []int64{3, 5, 2, 4, 1}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := pageAll(t, db, tc.sort)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("expected %v, got %v", tc.want, got)
			}
		})
	}
}

func TestPaginatePostgresRejectsForgedCursorValues(t *testing.T) {
	db := newPostgresDB(t)

	cursor := func(field string, sortValue, uniqueValue any, direction OrderDirection) string {
		return encodeTestCursor(t, Cursor{SortField: field, SortValue: sortValue, UniqueField: "?TableAlias.id", UniqueValue: uniqueValue, Direction: string(direction)})
	}

	tests := []struct {
		name   string
		sort   string
		cursor string
	}{
		{name: "default sort, text for a time", cursor: cursor("?TableAlias.created_at", "garbage", int64(2), DESC)},
		{name: "default sort, number for a time", cursor: cursor("?TableAlias.created_at", int64(1), int64(2), DESC)},
		// Go reads year 0, Postgres has no year 0: a data exception
		{name: "default sort, time Postgres cannot hold", cursor: cursor("?TableAlias.created_at", "0000-01-01T00:00:00Z", int64(2), DESC)},
		{name: "default sort, text for the integer id", cursor: cursor("?TableAlias.created_at", "2026-11-11T10:02:00Z", "abc", DESC)},
		{name: "client sort, number for a text column", sort: "name", cursor: cursor(`?TableAlias."name"`, int64(5), int64(2), ASC)},
		{name: "client sort, text for an integer column", sort: "rank", cursor: cursor(`?TableAlias."rank"`, "abc", int64(2), ASC)},
		{name: "client sort, text for the integer id", sort: "rank", cursor: cursor(`?TableAlias."rank"`, int64(1), "abc", ASC)},
		{name: "client Valuer sort, number for a uuid", sort: "code", cursor: cursor(`?TableAlias."code"`, int64(5), int64(2), ASC)},
		// The Valuer's text passes the Go check, Postgres refuses the uuid
		{name: "client Valuer sort, text that is not a uuid", sort: "code", cursor: cursor(`?TableAlias."code"`, "not-a-uuid", int64(2), ASC)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			items := []pgRecord{}
			request := &CursorRequest{Limit: 2, Sort: tc.sort, Cursor: tc.cursor}

			_, _, err := Paginate(context.Background(), db.NewSelect().Model(&items), request, pgRecord{}, Options{Direction: DESC, SortFields: pgRecordSortFields})
			requireCode(t, err, ez.EINVALID)
		})
	}
}

// A data exception that the caller's own query raises stays ez.EINTERNAL on a
// page with a cursor the server issued, as it is on the first page.
func TestPaginatePostgresKeepsOtherDataExceptionsInternal(t *testing.T) {
	db := newPostgresDB(t)

	tests := []struct {
		name  string
		apply func(q *bun.SelectQuery) *bun.SelectQuery
	}{
		{name: "division by zero", apply: func(q *bun.SelectQuery) *bun.SelectQuery {
			return q.Where("?TableAlias.rank / (?TableAlias.id - ?TableAlias.id) > 0")
		}},
		{name: "text that is not a uuid in the caller's condition", apply: func(q *bun.SelectQuery) *bun.SelectQuery {
			return q.Where("?TableAlias.code = ?", "not-a-uuid")
		}},
	}

	for _, sort := range []string{"", "code"} {
		items := []pgRecord{}
		opts := Options{Direction: DESC, SortFields: pgRecordSortFields}
		_, first, err := Paginate(context.Background(), db.NewSelect().Model(&items), &CursorRequest{Limit: 2, Sort: sort}, pgRecord{}, opts)
		if err != nil {
			t.Fatalf("first page: %v", err)
		}

		for _, tc := range tests {
			t.Run(tc.name+", sort "+sort, func(t *testing.T) {
				for _, cursor := range []string{"", first.NextCursor} {
					items := []pgRecord{}
					request := &CursorRequest{Limit: 2, Sort: sort, Cursor: cursor}

					_, _, err := Paginate(context.Background(), tc.apply(db.NewSelect().Model(&items)), request, pgRecord{}, opts)
					requireCode(t, err, ez.EINTERNAL)
				}
			})
		}
	}
}

// forgedRecord holds as Go strings, or the string Valuer code, columns whose
// own type only Postgres checks, so a forged cursor string for one passes the
// Go check and fails in the page query. It sorts by code by default, with the
// uuid id, a Go string, as the tie-breaker.
type forgedRecord struct {
	bun.BaseModel `bun:"table:pagination_forged,alias:f"`

	ID     string `bun:"id,pk,type:uuid"`
	Code   code   `bun:"code,type:uuid,notnull"`
	Mood   code   `bun:"mood,type:pagination_mood,notnull"`
	At     string `bun:"at,type:timestamptz,notnull"`
	Amount code   `bun:"amount,type:numeric,notnull"`
	Count  string `bun:"count,type:integer,notnull"`
	Label  string `bun:"label,notnull"`
}

func (r forgedRecord) GetSortField() string        { return "?TableAlias.code" }
func (r forgedRecord) GetSortValue() interface{}   { return r.Code }
func (r forgedRecord) GetUniqueField() string      { return "?TableAlias.id" }
func (r forgedRecord) GetUniqueValue() interface{} { return r.ID }

var forgedSortFields = map[string]string{
	"code":   "code",
	"mood":   "mood",
	"at":     "at",
	"amount": "amount",
	"count":  "count",
}

// newForgedDB returns a test database, with settings as createPostgresDB
// takes them, holding three pagination_forged rows.
func newForgedDB(t *testing.T, settings ...string) *relational.DB {
	t.Helper()

	db := createPostgresDB(t, settings...)
	ctx := context.Background()

	_, err := db.ExecContext(ctx, "CREATE TYPE pagination_mood AS ENUM ('calm', 'busy')")
	if err != nil {
		t.Fatalf("create type: %v", err)
	}

	_, err = db.NewCreateTable().Model((*forgedRecord)(nil)).Exec(ctx)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}

	rows := []forgedRecord{
		{ID: "00000000-0000-0000-0000-00000000000a", Code: "00000000-0000-0000-0000-000000000001", Mood: "calm", At: "2026-11-11T10:01:00Z", Amount: "1.5", Count: "1", Label: "uno"},
		{ID: "00000000-0000-0000-0000-00000000000b", Code: "00000000-0000-0000-0000-000000000002", Mood: "busy", At: "2026-11-11T10:02:00Z", Amount: "2.5", Count: "2", Label: "dos"},
		{ID: "00000000-0000-0000-0000-00000000000c", Code: "00000000-0000-0000-0000-000000000003", Mood: "calm", At: "2026-11-11T10:03:00Z", Amount: "3.5", Count: "3", Label: "tres"},
	}
	_, err = db.NewInsert().Model(&rows).Exec(ctx)
	if err != nil {
		t.Fatalf("insert rows: %v", err)
	}

	return db
}

// pageForged runs one page of pagination_forged under sort with cursor, the
// caller's own condition apply, if any, after a condition on label holding
// non-ASCII text, so Postgres's error position, in characters, differs from
// the byte offset of the literal it points at.
func pageForged(db *relational.DB, sort, cursor string, apply func(q *bun.SelectQuery) *bun.SelectQuery) (CursorResponse, error) {
	items := []forgedRecord{}
	q := db.NewSelect().Model(&items).Where("?TableAlias.label <> ?", "ñandú ✓")
	if apply != nil {
		q = apply(q)
	}

	request := &CursorRequest{Limit: 1, Sort: sort, Cursor: cursor}
	_, resp, err := Paginate(context.Background(), q, request, forgedRecord{}, Options{Direction: ASC, SortFields: forgedSortFields})

	return resp, err
}

func TestPaginatePostgresRejectsForgedCursorStrings(t *testing.T) {
	db := newForgedDB(t)

	validID := "00000000-0000-0000-0000-00000000000b"
	cursor := func(field string, sortValue, uniqueValue any) string {
		return encodeTestCursor(t, Cursor{SortField: field, SortValue: sortValue, UniqueField: "?TableAlias.id", UniqueValue: uniqueValue, Direction: string(ASC)})
	}

	forged := map[string][]string{
		"code":   {"it's", `back\slash`, "new\nline", "ñandú", ""},
		"mood":   {"happy", "it's", `back\slash`, "ñandú", ""},
		"at":     {"2026-01-01 10:00 Foo/Bar", "it's", "new\nline", ""},
		"amount": {"1e999999", "abc", `back\slash`, ""},
		"count":  {"abc", "1.5", "99999999999", "ñandú", ""},
	}

	for sort, values := range forged {
		for _, value := range values {
			t.Run(fmt.Sprintf("client sort %s, %q", sort, value), func(t *testing.T) {
				_, err := pageForged(db, sort, cursor(`?TableAlias."`+sort+`"`, value, validID), nil)
				requireCode(t, err, ez.EINVALID)
			})
		}
	}

	t.Run("default sort, a forged uuid", func(t *testing.T) {
		_, err := pageForged(db, "", cursor("?TableAlias.code", "it's ñ", validID), nil)
		requireCode(t, err, ez.EINVALID)
	})

	// The sort value is valid, so the exception is at the unique value
	t.Run("default sort, a forged unique uuid", func(t *testing.T) {
		_, err := pageForged(db, "", cursor("?TableAlias.code", "00000000-0000-0000-0000-000000000002", "abc"), nil)
		requireCode(t, err, ez.EINVALID)
	})

	t.Run("client sort, a forged unique uuid", func(t *testing.T) {
		_, err := pageForged(db, "count", cursor(`?TableAlias."count"`, "2", "abc"), nil)
		requireCode(t, err, ez.EINVALID)
	})
}

// A bad literal in the caller's own condition stays ez.EINTERNAL on a page
// with an issued cursor, even when the literal starts with the cursor's value.
func TestPaginatePostgresKeepsTheCallersBadLiteralInternal(t *testing.T) {
	db := newForgedDB(t)

	first, err := pageForged(db, "count", "", nil)
	if err != nil {
		t.Fatalf("first page: %v", err)
	}

	// The cursor holds the string "1", whose literal '1' starts the literal
	// '1''x' of the second value
	for _, literal := range []string{"1x", "1'x"} {
		_, err = pageForged(db, "count", first.NextCursor, func(q *bun.SelectQuery) *bun.SelectQuery {
			return q.Where("?TableAlias.amount <> ?", literal)
		})
		requireCode(t, err, ez.EINTERNAL)
	}
}

// Postgres translates its messages by lc_messages, not its error code or
// position, so a forged cursor is ez.EINVALID under any locale.
func TestPaginatePostgresRejectsForgedCursorStringsUnderAnotherLocale(t *testing.T) {
	db := newForgedDB(t, "lc_messages TO 'es_MX.UTF-8'")

	var message string
	_, err := db.NewRaw("SELECT 'x'::uuid").Exec(context.Background())
	if err != nil {
		message = err.Error()
	}
	if !strings.Contains(message, "sintaxis") {
		t.Skipf("the server does not translate its messages: %q", message)
	}

	cursor := encodeTestCursor(t, Cursor{SortField: `?TableAlias."code"`, SortValue: "abc", UniqueField: "?TableAlias.id", UniqueValue: "00000000-0000-0000-0000-00000000000b", Direction: string(ASC)})
	_, err = pageForged(db, "code", cursor, nil)
	requireCode(t, err, ez.EINVALID)
}
