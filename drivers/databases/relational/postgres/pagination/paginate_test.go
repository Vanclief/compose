package pagination

import (
	"context"
	"database/sql/driver"
	"encoding/base64"
	"encoding/json"
	"io"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
	pgquery "github.com/vanclief/compose/drivers/databases/relational/postgres/query"
	"github.com/vanclief/ez"
)

// record sorts by rank by default, with id as the tie-breaker. Extra is
// scanned but is not a column of records.
type record struct {
	bun.BaseModel `bun:"table:records,alias:r"`

	ID    int64  `bun:"id,pk"`
	Name  string `bun:"name"`
	Rank  int64  `bun:"rank"`
	Extra string `bun:"extra,scanonly"`
}

func (r record) GetSortField() string        { return "?TableAlias.rank" }
func (r record) GetSortValue() interface{}   { return r.Rank }
func (r record) GetUniqueField() string      { return "?TableAlias.id" }
func (r record) GetUniqueValue() interface{} { return r.ID }

// idRecord sorts by its unique field.
type idRecord struct {
	bun.BaseModel `bun:"table:records,alias:r"`

	ID int64 `bun:"id,pk"`
}

func (r idRecord) GetSortField() string        { return "?TableAlias.id" }
func (r idRecord) GetSortValue() interface{}   { return r.ID }
func (r idRecord) GetUniqueField() string      { return "?TableAlias.id" }
func (r idRecord) GetUniqueValue() interface{} { return r.ID }

const selectRecords = `SELECT "r"."id", "r"."name", "r"."rank" FROM "records" AS "r"`

var recordSortFields = map[string]string{
	"name": "name",
	"rank": "rank",
}

// newRecordConn answers selects with three records, one more than a page of
// two.
func newRecordConn() *fakeConn {
	return &fakeConn{
		count:   7,
		columns: []string{"id", "name", "rank"},
		rows: [][]driver.Value{
			{int64(1), "a", int64(9)},
			{int64(2), "b", int64(5)},
			{int64(3), "c", int64(1)},
		},
	}
}

func paginateRecords(conn *fakeConn, request *CursorRequest, opts Options) ([]record, CursorResponse, error) {
	db := newFakeDB(conn)
	items := []record{}

	return Paginate(context.Background(), db.NewSelect().Model(&items), request, record{}, opts)
}

func encodeTestCursor(t *testing.T, cursor Cursor) string {
	t.Helper()

	encoded, err := EncodeCursor(cursor)
	if err != nil {
		t.Fatalf("encode cursor: %v", err)
	}

	return encoded
}

func decodeTestCursor(t *testing.T, encoded string) Cursor {
	t.Helper()

	cursor, err := DecodeCursor(encoded)
	if err != nil {
		t.Fatalf("decode cursor: %v", err)
	}
	if cursor == nil {
		t.Fatal("expected a cursor")
	}

	return *cursor
}

func requireQueries(t *testing.T, conn *fakeConn, want ...string) {
	t.Helper()

	if len(conn.queries) != len(want) {
		t.Fatalf("expected %d queries, got %d: %q", len(want), len(conn.queries), conn.queries)
	}

	for i := range want {
		if conn.queries[i] != want[i] {
			t.Fatalf("query %d:\nexpected %s\ngot      %s", i, want[i], conn.queries[i])
		}
	}
}

func requireCode(t *testing.T, err error, code string) {
	t.Helper()

	if err == nil {
		t.Fatalf("expected %s", code)
	}
	if ez.ErrorCode(err) != code {
		t.Fatalf("expected %s, got %s: %v", code, ez.ErrorCode(err), err)
	}
}

func TestPaginateDefaultSort(t *testing.T) {
	conn := newRecordConn()

	items, resp, err := paginateRecords(conn, &CursorRequest{Limit: 2}, Options{Direction: DESC})
	if err != nil {
		t.Fatalf("paginate: %v", err)
	}

	requireQueries(t, conn, selectRecords+` ORDER BY "r".rank DESC, "r".id DESC LIMIT 3`)

	if len(items) != 2 || items[1].ID != 2 {
		t.Fatalf("expected the first two records, got %+v", items)
	}
	if !resp.HasNextPage || resp.TotalCount != nil {
		t.Fatalf("expected a next page and no count, got %+v", resp)
	}

	want := Cursor{SortField: "?TableAlias.rank", SortValue: int64(5), UniqueField: "?TableAlias.id", UniqueValue: int64(2), Direction: "desc"}
	cursor := decodeTestCursor(t, resp.NextCursor)
	if cursor != want {
		t.Fatalf("expected %+v, got %+v", want, cursor)
	}

	conn = newRecordConn()
	_, _, err = paginateRecords(conn, &CursorRequest{Limit: 2, Cursor: resp.NextCursor}, Options{Direction: DESC})
	if err != nil {
		t.Fatalf("paginate next page: %v", err)
	}

	requireQueries(t, conn, selectRecords+` WHERE (("r".rank, "r".id) < (5, 2)) ORDER BY "r".rank DESC, "r".id DESC LIMIT 3`)
}

func TestPaginateClampsLimit(t *testing.T) {
	conn := newRecordConn()
	request := &CursorRequest{Limit: MAX_PAGE_SIZE * 5}

	_, _, err := paginateRecords(conn, request, Options{Direction: ASC})
	if err != nil {
		t.Fatalf("paginate: %v", err)
	}

	if request.Limit != MAX_PAGE_SIZE {
		t.Fatalf("expected limit %d, got %d", MAX_PAGE_SIZE, request.Limit)
	}
	requireQueries(t, conn, selectRecords+` ORDER BY "r".rank ASC, "r".id ASC LIMIT 1001`)
}

func TestPaginateSameSortAndUniqueField(t *testing.T) {
	conn := &fakeConn{columns: []string{"id"}}
	db := newFakeDB(conn)
	items := []idRecord{}
	cursor := encodeTestCursor(t, Cursor{SortField: "?TableAlias.id", SortValue: int64(2), UniqueField: "?TableAlias.id", UniqueValue: int64(2), Direction: "asc"})

	_, _, err := Paginate(context.Background(), db.NewSelect().Model(&items), &CursorRequest{Limit: 2, Cursor: cursor}, idRecord{}, Options{Direction: ASC})
	if err != nil {
		t.Fatalf("paginate: %v", err)
	}

	requireQueries(t, conn, `SELECT "r"."id" FROM "records" AS "r" WHERE ("r".id > 2) ORDER BY "r".id ASC LIMIT 3`)
}

func TestPaginateCursorWithNumberAboveMaxInt64(t *testing.T) {
	tests := []struct {
		name      string
		sortValue interface{}
		want      string
	}{
		// As a float64 it would reach SQL as 9223372036854776000.
		{name: "uint64", sortValue: uint64(9223372036854775809), want: "9223372036854775809"},
		// The float's shortest digits, as before uint64 decoding.
		{name: "integral float64", sortValue: float64(1 << 63), want: "9223372036854776000"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			conn := newRecordConn()
			cursor := encodeTestCursor(t, Cursor{SortField: "?TableAlias.rank", SortValue: tc.sortValue, UniqueField: "?TableAlias.id", UniqueValue: int64(2), Direction: "desc"})

			_, _, err := paginateRecords(conn, &CursorRequest{Limit: 2, Cursor: cursor}, Options{Direction: DESC})
			if err != nil {
				t.Fatalf("paginate: %v", err)
			}

			requireQueries(t, conn, selectRecords+` WHERE (("r".rank, "r".id) < (`+tc.want+`, 2)) ORDER BY "r".rank DESC, "r".id DESC LIMIT 3`)
		})
	}
}

func TestPaginateClientSort(t *testing.T) {
	tests := []struct {
		name       string
		sort       string
		sortFields map[string]string
		firstPage  string
		nextPage   string
		wantCursor Cursor
	}{
		{
			name:       "ascending",
			sort:       "name",
			sortFields: recordSortFields,
			firstPage:  selectRecords + ` ORDER BY "r"."name" ASC, "r".id ASC LIMIT 3`,
			nextPage:   selectRecords + ` WHERE (("r"."name", "r".id) > ('b', 2)) ORDER BY "r"."name" ASC, "r".id ASC LIMIT 3`,
			wantCursor: Cursor{SortField: `?TableAlias."name"`, SortValue: "b", UniqueField: "?TableAlias.id", UniqueValue: int64(2), Direction: "asc"},
		},
		{
			name:       "descending",
			sort:       "-name",
			sortFields: recordSortFields,
			firstPage:  selectRecords + ` ORDER BY "r"."name" DESC, "r".id DESC LIMIT 3`,
			nextPage:   selectRecords + ` WHERE (("r"."name", "r".id) < ('b', 2)) ORDER BY "r"."name" DESC, "r".id DESC LIMIT 3`,
			wantCursor: Cursor{SortField: `?TableAlias."name"`, SortValue: "b", UniqueField: "?TableAlias.id", UniqueValue: int64(2), Direction: "desc"},
		},
		{
			name:       "ascending on the default sort column",
			sort:       "rank",
			sortFields: recordSortFields,
			firstPage:  selectRecords + ` ORDER BY "r"."rank" ASC, "r".id ASC LIMIT 3`,
			nextPage:   selectRecords + ` WHERE (("r"."rank", "r".id) > (5, 2)) ORDER BY "r"."rank" ASC, "r".id ASC LIMIT 3`,
			wantCursor: Cursor{SortField: `?TableAlias."rank"`, SortValue: int64(5), UniqueField: "?TableAlias.id", UniqueValue: int64(2), Direction: "asc"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			conn := newRecordConn()
			request := &CursorRequest{Limit: 2, Sort: tc.sort}
			// The endpoint's direction applies to the default sort only.
			opts := Options{Direction: DESC, SortFields: tc.sortFields}

			_, resp, err := paginateRecords(conn, request, opts)
			if err != nil {
				t.Fatalf("paginate: %v", err)
			}

			requireQueries(t, conn, tc.firstPage)

			cursor := decodeTestCursor(t, resp.NextCursor)
			if cursor != tc.wantCursor {
				t.Fatalf("expected %+v, got %+v", tc.wantCursor, cursor)
			}

			conn = newRecordConn()
			request.Cursor = resp.NextCursor
			_, _, err = paginateRecords(conn, request, opts)
			if err != nil {
				t.Fatalf("paginate next page: %v", err)
			}

			requireQueries(t, conn, tc.nextPage)
		})
	}
}

func TestPaginateRejectsSort(t *testing.T) {
	tests := []struct {
		name       string
		sort       string
		sortFields map[string]string
		wantCode   string
	}{
		{name: "not in allowlist", sort: "email", sortFields: recordSortFields, wantCode: ez.EINVALID},
		{name: "descending not in allowlist", sort: "-email", sortFields: recordSortFields, wantCode: ez.EINVALID},
		{name: "no allowlist", sort: "name", wantCode: ez.EINVALID},
		// A SortFields value must be a column of the model's table.
		{name: "qualified column", sort: "name", sortFields: map[string]string{"name": "u.name"}, wantCode: ez.EINTERNAL},
		{name: "table alias column", sort: "name", sortFields: map[string]string{"name": "?TableAlias.name"}, wantCode: ez.EINTERNAL},
		{name: "expression", sort: "name", sortFields: map[string]string{"name": "lower(name)"}, wantCode: ez.EINTERNAL},
		{name: "missing column", sort: "email", sortFields: map[string]string{"email": "missing"}, wantCode: ez.EINTERNAL},
		{name: "scanonly field", sort: "extra", sortFields: map[string]string{"extra": "extra"}, wantCode: ez.EINTERNAL},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			conn := newRecordConn()

			_, _, err := paginateRecords(conn, &CursorRequest{Sort: tc.sort, IncludeCount: true}, Options{Direction: ASC, SortFields: tc.sortFields, AllowCount: true})
			requireCode(t, err, tc.wantCode)
			requireQueries(t, conn)
		})
	}
}

func TestPaginateRejectsSortColumnTypeTheCursorCannotHold(t *testing.T) {
	tests := []struct {
		column  string
		message string
	}{
		{column: "tags", message: `Sort column "tags" has type []string, which a cursor cannot hold`},
		{column: "any", message: `Sort column "any" has type interface {}, which a cursor cannot hold`},
	}

	for _, tc := range tests {
		t.Run(tc.column, func(t *testing.T) {
			// The type is checked when the sort is resolved, before any query.
			conn := &fakeConn{}
			db := newFakeDB(conn)
			items := []valuerRecord{}
			request := &CursorRequest{Limit: 2, Sort: tc.column}
			opts := Options{Direction: ASC, SortFields: map[string]string{tc.column: tc.column}}

			_, _, err := Paginate(context.Background(), db.NewSelect().Model(&items), request, valuerRecord{}, opts)
			requireCode(t, err, ez.EINTERNAL)
			if ez.ErrorMessage(err) != tc.message {
				t.Fatalf("expected %q, got %q", tc.message, ez.ErrorMessage(err))
			}
			requireQueries(t, conn)
		})
	}
}

func TestPaginateRejectsCursorForDifferentOrder(t *testing.T) {
	defaultDesc := encodeTestCursor(t, Cursor{SortField: "?TableAlias.rank", SortValue: int64(5), UniqueField: "?TableAlias.id", UniqueValue: int64(2), Direction: "desc"})
	nameAsc := encodeTestCursor(t, Cursor{SortField: `?TableAlias."name"`, SortValue: "b", UniqueField: "?TableAlias.id", UniqueValue: int64(2), Direction: "asc"})

	tests := []struct {
		name      string
		cursor    string
		sort      string
		direction OrderDirection
		message   string
	}{
		{name: "default cursor under a client sort", cursor: defaultDesc, sort: "name", direction: DESC},
		{name: "client sort cursor under the default sort", cursor: nameAsc, direction: ASC},
		{name: "ascending cursor under descending client sort", cursor: nameAsc, sort: "-name", direction: ASC},
		{name: "descending cursor under ascending endpoint", cursor: defaultDesc, direction: ASC},
		// The default sort field is pinned as GetSortField returns it and a
		// client sort as rendered from the schema, so with this spelling their
		// cursors differ by the field text.
		{name: "default cursor under client sort of the same column and direction", cursor: defaultDesc, sort: "-rank", direction: DESC},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			conn := newRecordConn()
			request := &CursorRequest{Cursor: tc.cursor, Sort: tc.sort, IncludeCount: true}

			_, _, err := paginateRecords(conn, request, Options{Direction: tc.direction, SortFields: recordSortFields, AllowCount: true})
			requireCode(t, err, ez.EINVALID)
			if ez.ErrorMessage(err) != "Cursor was issued for a different order" {
				t.Fatalf("expected the different order message, got %q", ez.ErrorMessage(err))
			}
			requireQueries(t, conn)
		})
	}
}

// quotedRankRecord is record with GetSortField spelled as a client sort on
// rank renders, so its default-order cursors pin the same field text.
type quotedRankRecord struct {
	bun.BaseModel `bun:"table:records,alias:r"`

	ID   int64  `bun:"id,pk"`
	Name string `bun:"name"`
	Rank int64  `bun:"rank"`
}

func (r quotedRankRecord) GetSortField() string        { return `?TableAlias."rank"` }
func (r quotedRankRecord) GetSortValue() interface{}   { return r.Rank }
func (r quotedRankRecord) GetUniqueField() string      { return "?TableAlias.id" }
func (r quotedRankRecord) GetUniqueValue() interface{} { return r.ID }

func TestPaginateDefaultCursorUnderClientSortOfTheSameFieldText(t *testing.T) {
	defaultDesc := encodeTestCursor(t, Cursor{SortField: `?TableAlias."rank"`, SortValue: int64(5), UniqueField: "?TableAlias.id", UniqueValue: int64(2), Direction: "desc"})

	paginate := func(conn *fakeConn, sort string) error {
		db := newFakeDB(conn)
		items := []quotedRankRecord{}
		request := &CursorRequest{Limit: 2, Cursor: defaultDesc, Sort: sort}

		_, _, err := Paginate(context.Background(), db.NewSelect().Model(&items), request, quotedRankRecord{}, Options{Direction: DESC, SortFields: recordSortFields})
		return err
	}

	t.Run("same direction is accepted", func(t *testing.T) {
		conn := newRecordConn()

		err := paginate(conn, "-rank")
		if err != nil {
			t.Fatalf("paginate: %v", err)
		}

		requireQueries(t, conn, selectRecords+` WHERE (("r"."rank", "r".id) < (5, 2)) ORDER BY "r"."rank" DESC, "r".id DESC LIMIT 3`)
	})

	t.Run("other direction is rejected", func(t *testing.T) {
		conn := newRecordConn()

		err := paginate(conn, "rank")
		requireCode(t, err, ez.EINVALID)
		if ez.ErrorMessage(err) != "Cursor was issued for a different order" {
			t.Fatalf("expected the different order message, got %q", ez.ErrorMessage(err))
		}
		requireQueries(t, conn)
	})
}

func TestPaginateAcceptsLegacyCursorWithoutDirection(t *testing.T) {
	// The payload of a cursor issued before Direction existed.
	legacy := base64.URLEncoding.EncodeToString([]byte(`{"sort_field":"?TableAlias.rank","sort_value":5,"unique_field":"?TableAlias.id","unique_value":2}`))

	tests := []struct {
		direction OrderDirection
		want      string
	}{
		{direction: DESC, want: selectRecords + ` WHERE (("r".rank, "r".id) < (5, 2)) ORDER BY "r".rank DESC, "r".id DESC LIMIT 3`},
		{direction: ASC, want: selectRecords + ` WHERE (("r".rank, "r".id) > (5, 2)) ORDER BY "r".rank ASC, "r".id ASC LIMIT 3`},
	}

	for _, tc := range tests {
		t.Run(string(tc.direction), func(t *testing.T) {
			conn := newRecordConn()

			_, resp, err := paginateRecords(conn, &CursorRequest{Limit: 2, Cursor: legacy}, Options{Direction: tc.direction})
			if err != nil {
				t.Fatalf("paginate with legacy cursor: %v", err)
			}

			requireQueries(t, conn, tc.want)

			if decodeTestCursor(t, resp.NextCursor).Direction != string(tc.direction) {
				t.Fatalf("expected the next cursor to pin %s", tc.direction)
			}
		})
	}
}

func TestPaginateRejectsLegacyCursorUnderClientSort(t *testing.T) {
	// A cursor issued before Direction existed came from the endpoint's
	// default order, so a client sort rejects it: its field text
	// (?TableAlias.rank) differs from the client sort's (?TableAlias."rank").
	legacy := base64.URLEncoding.EncodeToString([]byte(`{"sort_field":"?TableAlias.rank","sort_value":5,"unique_field":"?TableAlias.id","unique_value":2}`))

	for _, sort := range []string{"-rank", "rank"} {
		t.Run(sort, func(t *testing.T) {
			conn := newRecordConn()
			request := &CursorRequest{Limit: 2, Cursor: legacy, Sort: sort}

			_, _, err := paginateRecords(conn, request, Options{Direction: DESC, SortFields: recordSortFields})
			requireCode(t, err, ez.EINVALID)
			requireQueries(t, conn)
		})
	}
}

func TestPaginateChecksCursorBeforeCount(t *testing.T) {
	conn := newRecordConn()

	_, _, err := paginateRecords(conn, &CursorRequest{Limit: 2, Cursor: "not a cursor", IncludeCount: true}, Options{Direction: ASC, AllowCount: true})
	requireCode(t, err, ez.EINVALID)
	requireQueries(t, conn)
}

func TestPaginateRejectsCountWithoutAllowCount(t *testing.T) {
	conn := newRecordConn()

	_, _, err := paginateRecords(conn, &CursorRequest{Limit: 2, IncludeCount: true}, Options{Direction: ASC})
	requireCode(t, err, ez.EINVALID)
	if ez.ErrorMessage(err) != "This list cannot be counted" {
		t.Fatalf("expected the cannot be counted message, got %q", ez.ErrorMessage(err))
	}
	requireQueries(t, conn)
}

func TestPaginateAllowCountWithoutIncludeCount(t *testing.T) {
	conn := newRecordConn()

	_, resp, err := paginateRecords(conn, &CursorRequest{Limit: 2}, Options{Direction: ASC, AllowCount: true})
	if err != nil {
		t.Fatalf("paginate: %v", err)
	}

	requireQueries(t, conn, selectRecords+` ORDER BY "r".rank ASC, "r".id ASC LIMIT 3`)
	if resp.TotalCount != nil {
		t.Fatalf("expected no total count, got %d", *resp.TotalCount)
	}
}

func TestPaginateIncludeCount(t *testing.T) {
	cursor := encodeTestCursor(t, Cursor{SortField: "?TableAlias.rank", SortValue: int64(5), UniqueField: "?TableAlias.id", UniqueValue: int64(2), Direction: "desc"})

	run := func(includeCount bool) (*fakeConn, CursorResponse) {
		conn := newRecordConn()
		db := newFakeDB(conn)
		items := []record{}
		q := db.NewSelect().Model(&items).Where("?TableAlias.name <> ?", "x")
		request := &CursorRequest{
			Limit:        2,
			Cursor:       cursor,
			IncludeCount: includeCount,
			Filter:       []pgquery.Filter{{Field: "rank", Value: int64(1), Comparison: ">="}},
		}

		_, resp, err := Paginate(context.Background(), q, request, record{}, Options{Direction: DESC, AllowCount: true})
		if err != nil {
			t.Fatalf("paginate include_count=%v: %v", includeCount, err)
		}

		return conn, resp
	}

	page := selectRecords + ` WHERE ("r".name <> 'x') AND ("rank" >= 1) AND (("r".rank, "r".id) < (5, 2)) ORDER BY "r".rank DESC, "r".id DESC LIMIT 3`

	withoutCount, resp := run(false)
	requireQueries(t, withoutCount, page)
	if resp.TotalCount != nil {
		t.Fatalf("expected no total count, got %d", *resp.TotalCount)
	}

	// The count covers the filtered set, not the rows after the cursor.
	withCount, resp := run(true)
	requireQueries(t, withCount, `SELECT count(*) FROM "records" AS "r" WHERE ("r".name <> 'x') AND ("rank" >= 1)`, page)
	if resp.TotalCount == nil || *resp.TotalCount != 7 {
		t.Fatalf("expected a total count of 7, got %v", resp.TotalCount)
	}
}

func TestPaginateKeepsQueryErrorCode(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	expired, cancelExpired := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer cancelExpired()

	count := `SELECT count(*) FROM "records" AS "r"`
	page := selectRecords + ` ORDER BY "r".rank ASC, "r".id ASC LIMIT 3`

	tests := []struct {
		name         string
		ctx          context.Context
		driverErr    error
		includeCount bool
		wantCode     string
		wantQueries  []string
	}{
		{name: "cancelled page", ctx: cancelled, wantCode: ez.ECANCELED},
		{name: "cancelled count", ctx: cancelled, includeCount: true, wantCode: ez.ECANCELED},
		{name: "expired page", ctx: expired, wantCode: ez.ETIMEOUT},
		{name: "expired count", ctx: expired, includeCount: true, wantCode: ez.ETIMEOUT},
		{name: "driver error on page", ctx: context.Background(), driverErr: io.ErrUnexpectedEOF, wantCode: ez.EINTERNAL, wantQueries: []string{page}},
		{name: "driver error on count", ctx: context.Background(), driverErr: io.ErrUnexpectedEOF, includeCount: true, wantCode: ez.EINTERNAL, wantQueries: []string{count}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			conn := newRecordConn()
			conn.err = tc.driverErr
			db := newFakeDB(conn)
			items := []record{}
			request := &CursorRequest{Limit: 2, IncludeCount: tc.includeCount}

			_, _, err := Paginate(tc.ctx, db.NewSelect().Model(&items), request, record{}, Options{Direction: ASC, AllowCount: true})
			requireCode(t, err, tc.wantCode)
			requireQueries(t, conn, tc.wantQueries...)
		})
	}
}

// team is the table a member belongs to.
type team struct {
	bun.BaseModel `bun:"table:teams,alias:team"`

	ID     int64 `bun:"id,pk"`
	Active bool  `bun:"active"`
}

// member belongs to a team.
type member struct {
	bun.BaseModel `bun:"table:members,alias:m"`

	ID     int64 `bun:"id,pk"`
	Rank   int64 `bun:"rank"`
	TeamID int64 `bun:"team_id"`
	Team   *team `bun:"rel:belongs-to,join:team_id=id"`
}

func (m member) GetSortField() string        { return "?TableAlias.rank" }
func (m member) GetSortValue() interface{}   { return m.Rank }
func (m member) GetUniqueField() string      { return "?TableAlias.id" }
func (m member) GetUniqueValue() interface{} { return m.ID }

func TestPaginateCountDoesNotApplyRelationTwice(t *testing.T) {
	// bun applies a relation's apply function to the query each time it
	// renders it, so a count on the page query itself would leave the join it
	// adds twice in the page query.
	conn := &fakeConn{count: 4}
	db := newFakeDB(conn)
	items := []member{}
	q := db.NewSelect().Model(&items).Relation("Team", func(q *bun.SelectQuery) *bun.SelectQuery {
		return q.Join(`JOIN "regions" AS "region" ON "region"."id" = "team"."id"`)
	})

	_, resp, err := Paginate(context.Background(), q, &CursorRequest{Limit: 2, IncludeCount: true}, member{}, Options{Direction: ASC, AllowCount: true})
	if err != nil {
		t.Fatalf("paginate: %v", err)
	}

	requireQueries(t, conn,
		`SELECT count(*) FROM "members" AS "m" LEFT JOIN "teams" AS "team" ON ("team"."id" = "m"."team_id") JOIN "regions" AS "region" ON "region"."id" = "team"."id"`,
		`SELECT "m"."id", "m"."rank", "m"."team_id", "team"."id" AS "team__id", "team"."active" AS "team__active" FROM "members" AS "m" LEFT JOIN "teams" AS "team" ON ("team"."id" = "m"."team_id") JOIN "regions" AS "region" ON "region"."id" = "team"."id" ORDER BY "m".rank ASC, "m".id ASC LIMIT 3`,
	)
	if resp.TotalCount == nil || *resp.TotalCount != 4 {
		t.Fatalf("expected a total count of 4, got %v", resp.TotalCount)
	}
}

// archivedRecord is a record with soft deletes.
type archivedRecord struct {
	bun.BaseModel `bun:"table:records,alias:r"`

	ID        int64     `bun:"id,pk"`
	Name      string    `bun:"name"`
	Rank      int64     `bun:"rank"`
	DeletedAt time.Time `bun:",soft_delete,nullzero"`
}

func (r archivedRecord) GetSortField() string        { return "?TableAlias.rank" }
func (r archivedRecord) GetSortValue() interface{}   { return r.Rank }
func (r archivedRecord) GetUniqueField() string      { return "?TableAlias.id" }
func (r archivedRecord) GetUniqueValue() interface{} { return r.ID }

func TestPaginateCountOnPinnedConnection(t *testing.T) {
	// The pool has one connection and the query holds it, so a count that
	// asked the pool for another would wait until the deadline.
	conn := newRecordConn()
	db := newFakeDB(conn)
	db.SetMaxOpenConns(1)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	pinned, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("pin connection: %v", err)
	}
	defer pinned.Close()

	items := []record{}
	_, resp, err := Paginate(ctx, pinned.NewSelect().Model(&items), &CursorRequest{Limit: 2, IncludeCount: true}, record{}, Options{Direction: ASC, AllowCount: true})
	if err != nil {
		t.Fatalf("paginate: %v", err)
	}

	requireQueries(t, conn, `SELECT count(*) FROM "records" AS "r"`, selectRecords+` ORDER BY "r".rank ASC, "r".id ASC LIMIT 3`)
	if resp.TotalCount == nil || *resp.TotalCount != 7 {
		t.Fatalf("expected a total count of 7, got %v", resp.TotalCount)
	}
}

func TestPaginateCountKeepsSoftDeleteScope(t *testing.T) {
	selectArchived := `SELECT "r"."id", "r"."name", "r"."rank", "r"."deleted_at" FROM "records" AS "r"`
	order := ` ORDER BY "r".rank ASC, "r".id ASC LIMIT 3`

	tests := []struct {
		name      string
		scope     func(q *bun.SelectQuery) *bun.SelectQuery
		wantCount string
		wantPage  string
	}{
		{
			name:      "where deleted",
			scope:     (*bun.SelectQuery).WhereDeleted,
			wantCount: `SELECT count(*) FROM "records" AS "r" WHERE "r"."deleted_at" IS NOT NULL`,
			wantPage:  selectArchived + ` WHERE "r"."deleted_at" IS NOT NULL` + order,
		},
		{
			name:      "all with deleted",
			scope:     (*bun.SelectQuery).WhereAllWithDeleted,
			wantCount: `SELECT count(*) FROM "records" AS "r"`,
			wantPage:  selectArchived + order,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			conn := newRecordConn()
			db := newFakeDB(conn)
			items := []archivedRecord{}
			q := tc.scope(db.NewSelect().Model(&items))

			_, _, err := Paginate(context.Background(), q, &CursorRequest{Limit: 2, IncludeCount: true}, archivedRecord{}, Options{Direction: ASC, AllowCount: true})
			if err != nil {
				t.Fatalf("paginate: %v", err)
			}

			requireQueries(t, conn, tc.wantCount, tc.wantPage)
		})
	}
}

func TestPaginateSoftDeleteScopeStaysOutsideTopLevelOr(t *testing.T) {
	// bun wraps the query's own conditions in parentheses before adding the
	// soft-delete scope, so a top-level OR cannot pull deleted rows into a
	// page or its count. The cursor is still bound to the last OR branch, which
	// is why Paginate's doc asks for OR conditions inside WhereGroup.
	conn := newRecordConn()
	db := newFakeDB(conn)
	items := []archivedRecord{}
	q := db.NewSelect().Model(&items).Where("?TableAlias.name = ?", "a").WhereOr("?TableAlias.name = ?", "b")

	_, _, err := Paginate(context.Background(), q, &CursorRequest{Limit: 2, IncludeCount: true}, archivedRecord{}, Options{Direction: ASC, AllowCount: true})
	if err != nil {
		t.Fatalf("paginate: %v", err)
	}

	where := ` WHERE (("r".name = 'a') OR ("r".name = 'b')) AND "r"."deleted_at" IS NULL`
	requireQueries(t, conn,
		`SELECT count(*) FROM "records" AS "r"`+where,
		`SELECT "r"."id", "r"."name", "r"."rank", "r"."deleted_at" FROM "records" AS "r"`+where+` ORDER BY "r".rank ASC, "r".id ASC LIMIT 3`,
	)
}

func TestPaginateCountKeepsWherePK(t *testing.T) {
	conn := newRecordConn()
	db := newFakeDB(conn)
	items := []record{{ID: 5}}

	_, _, err := Paginate(context.Background(), db.NewSelect().Model(&items).WherePK(), &CursorRequest{Limit: 2, IncludeCount: true}, record{}, Options{Direction: ASC, AllowCount: true})
	if err != nil {
		t.Fatalf("paginate: %v", err)
	}

	requireQueries(t, conn,
		`SELECT count(*) FROM "records" AS "r" WHERE "r"."id" IN (5)`,
		selectRecords+` WHERE "r"."id" IN (5) ORDER BY "r".rank ASC, "r".id ASC LIMIT 3`,
	)
}

func TestPaginateCountKeepsStoredQueryError(t *testing.T) {
	conn := newRecordConn()
	db := newFakeDB(conn)
	items := []record{}
	q := db.NewSelect().Model(&items).Err(ez.New(ez.ECONFLICT, "Query built from a stale filter", nil))

	_, _, err := Paginate(context.Background(), q, &CursorRequest{Limit: 2, IncludeCount: true}, record{}, Options{Direction: ASC, AllowCount: true})
	requireCode(t, err, ez.ECONFLICT)
	requireQueries(t, conn)
}

// status is stored as an integer but marshals to its name.
type status int64

func (s status) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[status]string{1: "open", 2: "closed"}[s])
}

type statusRecord struct {
	bun.BaseModel `bun:"table:records,alias:r"`

	ID     int64  `bun:"id,pk"`
	Status status `bun:"status"`
}

func (r statusRecord) GetSortField() string        { return "?TableAlias.id" }
func (r statusRecord) GetSortValue() interface{}   { return r.ID }
func (r statusRecord) GetUniqueField() string      { return "?TableAlias.id" }
func (r statusRecord) GetUniqueValue() interface{} { return r.ID }

func TestPaginateClientSortCursorStoresTheColumnValue(t *testing.T) {
	conn := &fakeConn{
		columns: []string{"id", "status"},
		rows:    [][]driver.Value{{int64(1), int64(1)}, {int64(2), int64(1)}, {int64(3), int64(2)}},
	}
	db := newFakeDB(conn)
	items := []statusRecord{}
	request := &CursorRequest{Limit: 2, Sort: "status"}
	opts := Options{Direction: ASC, SortFields: map[string]string{"status": "status"}}

	_, resp, err := Paginate(context.Background(), db.NewSelect().Model(&items), request, statusRecord{}, opts)
	if err != nil {
		t.Fatalf("paginate: %v", err)
	}

	cursor := decodeTestCursor(t, resp.NextCursor)
	if cursor.SortValue != int64(1) {
		t.Fatalf("expected the cursor to store the integer 1, got %#v", cursor.SortValue)
	}

	conn.queries = nil
	request.Cursor = resp.NextCursor
	_, _, err = Paginate(context.Background(), db.NewSelect().Model(&items), request, statusRecord{}, opts)
	if err != nil {
		t.Fatalf("paginate next page: %v", err)
	}

	requireQueries(t, conn, `SELECT "r"."id", "r"."status" FROM "records" AS "r" WHERE (("r"."status", "r".id) > (1, 2)) ORDER BY "r"."status" ASC, "r".id ASC LIMIT 3`)
}

func TestPaginateClientSortWithoutDefaultSortColumn(t *testing.T) {
	// The query selects only id and name, so CreatedAt, which createdRecord's
	// GetSortValue dereferences, stays nil on every row.
	conn := &fakeConn{
		columns: []string{"id", "name"},
		rows:    [][]driver.Value{{int64(1), "a"}, {int64(2), "b"}, {int64(3), "c"}},
	}
	db := newFakeDB(conn)
	items := []createdRecord{}
	request := &CursorRequest{Limit: 2, Sort: "name"}
	opts := Options{Direction: DESC, SortFields: map[string]string{"name": "name"}}

	page, resp, err := Paginate(context.Background(), db.NewSelect().Model(&items).Column("id", "name"), request, createdRecord{}, opts)
	if err != nil {
		t.Fatalf("paginate: %v", err)
	}

	requireQueries(t, conn, `SELECT "r"."id", "r"."name" FROM "records" AS "r" ORDER BY "r"."name" ASC, "r".id ASC LIMIT 3`)

	if len(page) != 2 || page[1].Name != "b" || !resp.HasNextPage {
		t.Fatalf("expected the first two records with a next page, got %+v and %+v", page, resp)
	}

	want := Cursor{SortField: `?TableAlias."name"`, SortValue: "b", UniqueField: "?TableAlias.id", UniqueValue: int64(2), Direction: "asc"}
	cursor := decodeTestCursor(t, resp.NextCursor)
	if cursor != want {
		t.Fatalf("expected %+v, got %+v", want, cursor)
	}
}

func TestPaginateScansIntoQueryModel(t *testing.T) {
	conn := newRecordConn()
	db := newFakeDB(conn)
	items := []record{}

	page, _, err := Paginate(context.Background(), db.NewSelect().Model(&items), &CursorRequest{Limit: 2}, record{}, Options{Direction: ASC})
	if err != nil {
		t.Fatalf("paginate: %v", err)
	}

	if len(page) != 2 || page[0].Name != "a" || !reflect.DeepEqual(items, page) {
		t.Fatalf("expected the query model to hold the trimmed page, got items %+v and page %+v", items, page)
	}
}

func TestPaginateScansIntoPointerQueryModel(t *testing.T) {
	conn := newRecordConn()
	db := newFakeDB(conn)
	items := []*record{}
	request := &CursorRequest{Limit: 2, Sort: "name"}
	opts := Options{Direction: ASC, SortFields: recordSortFields}

	page, resp, err := Paginate(context.Background(), db.NewSelect().Model(&items), request, &record{}, opts)
	if err != nil {
		t.Fatalf("paginate: %v", err)
	}

	if len(page) != 2 || len(items) != 2 || page[1].Name != "b" {
		t.Fatalf("expected the first two records, got page %+v and items %+v", page, items)
	}

	want := Cursor{SortField: `?TableAlias."name"`, SortValue: "b", UniqueField: "?TableAlias.id", UniqueValue: int64(2), Direction: "asc"}
	cursor := decodeTestCursor(t, resp.NextCursor)
	if cursor != want {
		t.Fatalf("expected %+v, got %+v", want, cursor)
	}

	conn.queries = nil
	request.Cursor = resp.NextCursor
	_, _, err = Paginate(context.Background(), db.NewSelect().Model(&items), request, &record{}, opts)
	if err != nil {
		t.Fatalf("paginate next page: %v", err)
	}

	requireQueries(t, conn, selectRecords+` WHERE (("r"."name", "r".id) > ('b', 2)) ORDER BY "r"."name" ASC, "r".id ASC LIMIT 3`)
}

func TestPaginateScansNonSliceQueryModelIntoNewSlice(t *testing.T) {
	// Model((*T)(nil)) gives the query a table, so a client sort renders, but
	// no slice to scan into.
	conn := newRecordConn()
	db := newFakeDB(conn)
	request := &CursorRequest{Limit: 2, Sort: "name", IncludeCount: true}
	opts := Options{Direction: DESC, SortFields: recordSortFields, AllowCount: true}

	page, resp, err := Paginate(context.Background(), db.NewSelect().Model((*record)(nil)), request, record{}, opts)
	if err != nil {
		t.Fatalf("paginate: %v", err)
	}

	requireQueries(t, conn,
		`SELECT count(*) FROM "records" AS "r"`,
		selectRecords+` ORDER BY "r"."name" ASC, "r".id ASC LIMIT 3`,
	)
	if len(page) != 2 || page[1].Name != "b" || !resp.HasNextPage || resp.TotalCount == nil || *resp.TotalCount != 7 {
		t.Fatalf("expected a counted page of two records with a next page, got %+v and %+v", page, resp)
	}

	cursor := decodeTestCursor(t, resp.NextCursor)
	if cursor.SortField != `?TableAlias."name"` || cursor.SortValue != "b" {
		t.Fatalf("expected a cursor on name b, got %+v", cursor)
	}
}

func TestPaginateScansWithoutQueryModel(t *testing.T) {
	conn := &fakeConn{columns: []string{"id"}, rows: [][]driver.Value{{int64(1)}, {int64(2)}}}
	db := newFakeDB(conn)

	page, resp, err := Paginate(context.Background(), db.NewSelect().TableExpr("records"), &CursorRequest{Limit: 2}, testItem{}, Options{Direction: ASC})
	if err != nil {
		t.Fatalf("paginate: %v", err)
	}

	requireQueries(t, conn, `SELECT * FROM records ORDER BY id ASC LIMIT 3`)
	if len(page) != 2 || page[1].ID != 2 || resp.HasNextPage {
		t.Fatalf("expected two records on the last page, got %+v and %+v", page, resp)
	}
}

func TestPaginateRejectsInvalidOrder(t *testing.T) {
	tests := []struct {
		name       string
		request    *CursorRequest
		sortFields map[string]string
	}{
		{name: "default sort", request: &CursorRequest{IncludeCount: true}},
		{name: "client sort", request: &CursorRequest{Sort: "name", IncludeCount: true}, sortFields: recordSortFields},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			conn := newRecordConn()
			opts := Options{Direction: OrderDirection("sideways"), SortFields: tc.sortFields, AllowCount: true}

			_, _, err := paginateRecords(conn, tc.request, opts)
			requireCode(t, err, ez.EINTERNAL)
			requireQueries(t, conn)
		})
	}
}

func TestPaginateEscapesFilterField(t *testing.T) {
	conn := &fakeConn{columns: []string{"id"}}
	db := newFakeDB(conn)
	request := &CursorRequest{
		Limit: 10,
		Filter: []pgquery.Filter{
			{
				Field:      "created_at drop table users",
				Value:      int64(123),
				Comparison: ">=",
			},
		},
	}

	_, _, err := Paginate(context.Background(), db.NewSelect().TableExpr("records"), request, testItem{}, Options{Direction: ASC})
	if err != nil {
		t.Fatalf("paginate: %v", err)
	}

	expected := `"created_at drop table users" >= 123`
	if len(conn.queries) != 1 || !strings.Contains(conn.queries[0], expected) {
		t.Fatalf("expected SQL to contain %q, got %q", expected, conn.queries)
	}
}

func TestPaginateRejectsUnsafeFilterComparison(t *testing.T) {
	conn := &fakeConn{columns: []string{"id"}}
	db := newFakeDB(conn)
	request := &CursorRequest{
		Limit:        10,
		IncludeCount: true,
		Filter: []pgquery.Filter{
			{
				Field:      "created_at",
				Value:      int64(123),
				Comparison: ">= or true",
			},
		},
	}

	_, _, err := Paginate(context.Background(), db.NewSelect().TableExpr("records"), request, testItem{}, Options{Direction: ASC, AllowCount: true})
	requireCode(t, err, ez.EINTERNAL)
	requireQueries(t, conn)
}

func TestPaginateRejectsClientSortWithoutQueryModel(t *testing.T) {
	conn := newRecordConn()
	db := newFakeDB(conn)
	request := &CursorRequest{Limit: 2, Sort: "name", IncludeCount: true}
	opts := Options{Direction: ASC, SortFields: recordSortFields, AllowCount: true}

	_, _, err := Paginate(context.Background(), db.NewSelect().TableExpr("records"), request, record{}, opts)
	requireCode(t, err, ez.EINTERNAL)
	requireQueries(t, conn)
}

func TestPaginateRejectsQueryModelOfAnotherType(t *testing.T) {
	conn := newRecordConn()
	db := newFakeDB(conn)
	items := []*record{}

	_, _, err := Paginate(context.Background(), db.NewSelect().Model(&items), &CursorRequest{Limit: 2, IncludeCount: true}, record{}, Options{Direction: ASC, AllowCount: true})
	requireCode(t, err, ez.EINTERNAL)
	if ez.ErrorMessage(err) != "Query model is *[]*pagination.record, Paginate needs *[]pagination.record" {
		t.Fatalf("expected the message to name both types, got %q", ez.ErrorMessage(err))
	}
	requireQueries(t, conn)
}

// timeRecord sorts by created_at by default.
type timeRecord struct {
	bun.BaseModel `bun:"table:records,alias:r"`

	ID        int64     `bun:"id,pk"`
	CreatedAt time.Time `bun:"created_at"`
}

func (r timeRecord) GetSortField() string        { return "?TableAlias.created_at" }
func (r timeRecord) GetSortValue() interface{}   { return r.CreatedAt }
func (r timeRecord) GetUniqueField() string      { return "?TableAlias.id" }
func (r timeRecord) GetUniqueValue() interface{} { return r.ID }

func TestPaginateRejectsCursorValueOfAnotherType(t *testing.T) {
	rankCursor := func(sortValue, uniqueValue any) string {
		return encodeTestCursor(t, Cursor{SortField: "?TableAlias.rank", SortValue: sortValue, UniqueField: "?TableAlias.id", UniqueValue: uniqueValue, Direction: "desc"})
	}
	nameCursor := func(sortValue, uniqueValue any) string {
		return encodeTestCursor(t, Cursor{SortField: `?TableAlias."name"`, SortValue: sortValue, UniqueField: "?TableAlias.id", UniqueValue: uniqueValue, Direction: "asc"})
	}
	clientRankCursor := func(sortValue, uniqueValue any) string {
		return encodeTestCursor(t, Cursor{SortField: `?TableAlias."rank"`, SortValue: sortValue, UniqueField: "?TableAlias.id", UniqueValue: uniqueValue, Direction: "asc"})
	}

	tests := []struct {
		name   string
		cursor string
		sort   string
	}{
		{name: "default sort, string sort value", cursor: rankCursor("5", int64(2))},
		{name: "default sort, bool sort value", cursor: rankCursor(true, int64(2))},
		{name: "default sort, null sort value", cursor: rankCursor(nil, int64(2))},
		{name: "default sort, string unique value", cursor: rankCursor(int64(5), "2")},
		{name: "default sort, bool unique value", cursor: rankCursor(int64(5), false)},
		{name: "client string sort, number sort value", cursor: nameCursor(int64(5), int64(2)), sort: "name"},
		{name: "client string sort, string unique value", cursor: nameCursor("b", "2"), sort: "name"},
		{name: "client number sort, string sort value", cursor: clientRankCursor("5", int64(2)), sort: "rank"},
		{name: "client number sort, bool unique value", cursor: clientRankCursor(int64(5), true), sort: "rank"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			conn := newRecordConn()
			request := &CursorRequest{Limit: 2, Cursor: tc.cursor, Sort: tc.sort, IncludeCount: true}

			_, _, err := paginateRecords(conn, request, Options{Direction: DESC, SortFields: recordSortFields, AllowCount: true})
			requireCode(t, err, ez.EINVALID)
			if ez.ErrorMessage(err) != "invalid cursor data" {
				t.Fatalf("expected the invalid cursor message, got %q", ez.ErrorMessage(err))
			}
			// Neither the count nor the page runs
			requireQueries(t, conn)
		})
	}
}

// uuidRecord sorts by rank by default, with a uuid.UUID, which has its own
// MarshalText, as the tie-breaker.
type uuidRecord struct {
	bun.BaseModel `bun:"table:records,alias:r"`

	ID   uuid.UUID `bun:"id,pk"`
	Rank int64     `bun:"rank"`
}

func (r uuidRecord) GetSortField() string        { return "?TableAlias.rank" }
func (r uuidRecord) GetSortValue() interface{}   { return r.Rank }
func (r uuidRecord) GetUniqueField() string      { return "?TableAlias.id" }
func (r uuidRecord) GetUniqueValue() interface{} { return r.ID }

func TestPaginateChecksCursorValuesByIssuedType(t *testing.T) {
	issued, err := json.Marshal(time.Date(2026, 11, 11, 10, 30, 0, 123456000, time.FixedZone("", -6*3600)))
	if err != nil {
		t.Fatalf("marshal time: %v", err)
	}
	issuedTime := strings.Trim(string(issued), `"`)
	id := uuid.MustParse("6f1c2a8e-3b7d-4e5f-9a0b-1c2d3e4f5a6b").String()

	// timeRecord's default sort
	byTime := func(sortValue any) Cursor {
		return Cursor{SortField: "?TableAlias.created_at", SortValue: sortValue, UniqueField: "?TableAlias.id", UniqueValue: int64(2), Direction: "asc"}
	}
	// uuidRecord's default sort
	byRank := func(uniqueValue any) Cursor {
		return Cursor{SortField: "?TableAlias.rank", SortValue: int64(5), UniqueField: "?TableAlias.id", UniqueValue: uniqueValue, Direction: "asc"}
	}
	// A client sort on valuerRecord
	byColumn := func(column string, sortValue any) Cursor {
		return Cursor{SortField: `?TableAlias."` + column + `"`, SortValue: sortValue, UniqueField: "?TableAlias.id", UniqueValue: int64(2), Direction: "asc"}
	}

	tests := []struct {
		name   string
		model  Paginatable
		cursor Cursor
		sort   string
		valid  bool
	}{
		{name: "default time sort, issued time", model: timeRecord{}, cursor: byTime(issuedTime), valid: true},
		{name: "default time sort, text", model: timeRecord{}, cursor: byTime("yesterday")},
		{name: "default time sort, number", model: timeRecord{}, cursor: byTime(int64(1700000000))},
		{name: "MarshalText unique value, issued text", model: uuidRecord{}, cursor: byRank(id), valid: true},
		{name: "MarshalText unique value, number", model: uuidRecord{}, cursor: byRank(int64(7))},
		{name: "client time sort, issued time", model: valuerRecord{}, cursor: byColumn("created_at", issuedTime), sort: "created_at", valid: true},
		{name: "client time sort, text", model: valuerRecord{}, cursor: byColumn("created_at", "yesterday"), sort: "created_at"},
		{name: "client time sort, number", model: valuerRecord{}, cursor: byColumn("created_at", int64(1700000000)), sort: "created_at"},
		// A Valuer is held to the kind its Value returns on a zero field
		{name: "client string Valuer sort, text", model: valuerRecord{}, cursor: byColumn("by_value", "value b"), sort: "by_value", valid: true},
		{name: "client string Valuer sort, number", model: valuerRecord{}, cursor: byColumn("by_value", int64(5)), sort: "by_value"},
		{name: "client pointer string Valuer sort, number", model: valuerRecord{}, cursor: byColumn("by_struct", int64(5)), sort: "by_struct"},
		{name: "client uuid sort, text", model: valuerRecord{}, cursor: byColumn("uuid", id), sort: "uuid", valid: true},
		{name: "client uuid sort, number", model: valuerRecord{}, cursor: byColumn("uuid", int64(5)), sort: "uuid"},
		{name: "client int Valuer sort, number", model: valuerRecord{}, cursor: byColumn("int_value", int64(5)), sort: "int_value", valid: true},
		{name: "client int Valuer sort, text", model: valuerRecord{}, cursor: byColumn("int_value", "5"), sort: "int_value"},
		// A zero sql.NullString's Value is nil, so its kind cannot be told
		{name: "client Valuer sort with a nil zero Value, number", model: valuerRecord{}, cursor: byColumn("nullable", int64(5)), sort: "nullable", valid: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			conn := &fakeConn{}
			db := newFakeDB(conn)
			request := &CursorRequest{Limit: 2, Cursor: encodeTestCursor(t, tc.cursor), Sort: tc.sort}
			opts := Options{Direction: ASC, SortFields: map[string]string{tc.sort: tc.sort}}

			var err error
			switch tc.model.(type) {
			case timeRecord:
				_, _, err = Paginate(context.Background(), db.NewSelect().Model(&[]timeRecord{}), request, timeRecord{}, opts)
			case uuidRecord:
				_, _, err = Paginate(context.Background(), db.NewSelect().Model(&[]uuidRecord{}), request, uuidRecord{}, opts)
			default:
				_, _, err = Paginate(context.Background(), db.NewSelect().Model(&[]valuerRecord{}), request, valuerRecord{}, opts)
			}

			if tc.valid {
				if err != nil {
					t.Fatalf("paginate: %v", err)
				}
				if len(conn.queries) != 1 {
					t.Fatalf("expected the page query, got %q", conn.queries)
				}
				return
			}

			requireCode(t, err, ez.EINVALID)
			requireQueries(t, conn)
		})
	}
}

func TestPaginateChecksNoTypeWhenZeroModelPanics(t *testing.T) {
	// createdRecord's GetSortValue dereferences a nil CreatedAt on a zero model.
	conn := &fakeConn{}
	db := newFakeDB(conn)
	cursor := encodeTestCursor(t, Cursor{SortField: "?TableAlias.created_at", SortValue: int64(1700000000), UniqueField: "?TableAlias.id", UniqueValue: int64(2), Direction: "desc"})

	_, _, err := Paginate(context.Background(), db.NewSelect().Model(&[]createdRecord{}), &CursorRequest{Limit: 2, Cursor: cursor}, createdRecord{}, Options{Direction: DESC})
	if err != nil {
		t.Fatalf("paginate: %v", err)
	}

	requireQueries(t, conn, `SELECT "r"."id", "r"."name", "r"."created_at" FROM "records" AS "r" WHERE (("r".created_at, "r".id) < (1700000000, 2)) ORDER BY "r".created_at DESC, "r".id DESC LIMIT 3`)
}

// fakePgError carries a SQLSTATE and a position the way pgdriver.Error does.
type fakePgError struct {
	code     string
	position string
}

func (e fakePgError) Field(k byte) string {
	switch k {
	case 'C':
		return e.code
	case 'P':
		return e.position
	}

	return ""
}

func (e fakePgError) Error() string {
	return "ERROR #" + e.code
}

// TestPaginateDataExceptionFromTheCursorIsInvalid fails the page query with a
// data exception at a chosen position of the query it rendered. Which errors
// Postgres reports where is in the Postgres tests.
func TestPaginateDataExceptionFromTheCursorIsInvalid(t *testing.T) {
	cursor := encodeTestCursor(t, Cursor{SortField: `?TableAlias."name"`, SortValue: "abc", UniqueField: "?TableAlias.id", UniqueValue: int64(2), Direction: "asc"})
	empty := encodeTestCursor(t, Cursor{SortField: `?TableAlias."name"`, SortValue: "", UniqueField: "?TableAlias.id", UniqueValue: int64(2), Direction: "asc"})
	// The filter puts non-ASCII text before the cursor's literal, so a
	// position in characters differs from one in bytes
	filter := []pgquery.Filter{{Field: "name", Comparison: ">=", Value: "ñandú"}}

	// at returns the 1-based character position of literal in query, or, with
	// bytes, its byte position
	at := func(query, literal string, bytes bool) string {
		index := strings.Index(query, literal)
		if index < 0 {
			t.Fatalf("%s is not in %s", literal, query)
		}
		if bytes {
			return strconv.Itoa(index + 1)
		}
		return strconv.Itoa(utf8.RuneCountInString(query[:index]) + 1)
	}

	tests := []struct {
		name         string
		cursor       string
		code         string
		position     func(query string) string
		includeCount bool
		wantCode     string
	}{
		{name: "at the cursor's literal", cursor: cursor, code: "22P02", position: func(q string) string { return at(q, "'abc'", false) }, wantCode: ez.EINVALID},
		{name: "at the cursor's empty literal", cursor: empty, code: "22P02", position: func(q string) string { return at(q, "''", false) }, wantCode: ez.EINVALID},
		{name: "unparsable position", cursor: cursor, code: "22P02", position: func(string) string { return "x" }, wantCode: ez.EINTERNAL},
		{name: "at the byte position of the cursor's literal", cursor: cursor, code: "22P02", position: func(q string) string { return at(q, "'abc'", true) }, wantCode: ez.EINTERNAL},
		{name: "at the caller's own literal", cursor: cursor, code: "22P02", position: func(q string) string { return at(q, "'ñandú'", false) }, wantCode: ez.EINTERNAL},
		{name: "without a position", cursor: cursor, code: "22012", position: func(string) string { return "" }, wantCode: ez.EINTERNAL},
		{name: "position past the query", cursor: cursor, code: "22P02", position: func(q string) string { return strconv.Itoa(utf8.RuneCountInString(q) + 1) }, wantCode: ez.EINTERNAL},
		{name: "position zero", cursor: cursor, code: "22P02", position: func(string) string { return "0" }, wantCode: ez.EINTERNAL},
		{name: "other class at the cursor's literal", cursor: cursor, code: "42883", position: func(q string) string { return at(q, "'abc'", false) }, wantCode: ez.EINTERNAL},
		{name: "page without a cursor", code: "22P02", position: func(q string) string { return at(q, "'ñandú'", false) }, wantCode: ez.EINTERNAL},
		{name: "on the count", cursor: cursor, code: "22P02", position: func(q string) string { return at(q, "'abc'", false) }, includeCount: true, wantCode: ez.EINTERNAL},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := Options{Direction: ASC, AllowCount: true, SortFields: map[string]string{"name": "name"}}

			// Render the page query once on a connection that answers it
			conn := newRecordConn()
			_, _, err := paginateRecords(conn, &CursorRequest{Limit: 2, Sort: "name", Cursor: tc.cursor, Filter: filter}, opts)
			if err != nil {
				t.Fatalf("paginate: %v", err)
			}
			page := conn.queries[len(conn.queries)-1]

			conn = newRecordConn()
			conn.err = fakePgError{code: tc.code, position: tc.position(page)}
			request := &CursorRequest{Limit: 2, Sort: "name", Cursor: tc.cursor, Filter: filter, IncludeCount: tc.includeCount}

			_, _, err = paginateRecords(conn, request, opts)
			requireCode(t, err, tc.wantCode)
		})
	}
}
