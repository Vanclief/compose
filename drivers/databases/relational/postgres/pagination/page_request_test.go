package pagination

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/labstack/echo/v5"
	pgquery "github.com/vanclief/compose/drivers/databases/relational/postgres/query"
	"github.com/vanclief/ez"
)

func TestPageRequestValidateLimit(t *testing.T) {
	tests := []struct {
		name      string
		limit     int
		wantLimit int
		wantErr   bool
	}{
		{name: "negative", limit: -1, wantErr: true},
		{name: "zero applies default", limit: 0, wantLimit: DEFAULT_LIMIT},
		{name: "one", limit: 1, wantLimit: 1},
		{name: "max", limit: MAX_PAGE_SIZE, wantLimit: MAX_PAGE_SIZE},
		{name: "above max clamps", limit: MAX_PAGE_SIZE + 1, wantLimit: MAX_PAGE_SIZE},
		{name: "far above max clamps", limit: 1000000, wantLimit: MAX_PAGE_SIZE},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			request := &PageRequest{Limit: tc.limit}

			err := request.Validate()
			if tc.wantErr {
				requireCode(t, err, ez.EINVALID)
				return
			}

			if err != nil {
				t.Fatalf("validate: %v", err)
			}
			if request.Limit != tc.wantLimit {
				t.Fatalf("expected limit %d, got %d", tc.wantLimit, request.Limit)
			}
		})
	}
}

func TestPageRequestReadsOnlyLimitAndCursorFromTheQuery(t *testing.T) {
	type listRequest struct {
		PageRequest
		Name string `json:"-" query:"name"`
	}

	req := httptest.NewRequest(http.MethodGet, "/?limit=10&cursor=abc&sort=-name&include_count=true&name=x", nil)
	c := echo.New().NewContext(req, httptest.NewRecorder())

	var request listRequest
	err := c.Bind(&request)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}

	want := listRequest{PageRequest: PageRequest{Limit: 10, Cursor: "abc"}, Name: "x"}
	if !reflect.DeepEqual(request, want) {
		t.Fatalf("expected %+v, got %+v", want, request)
	}

	// The fields are never body fields, and Filter is server-side only
	var body PageRequest
	err = json.Unmarshal([]byte(`{"limit": 10, "cursor": "abc", "Filter": [{"Field": "x", "Comparison": ">="}]}`), &body)
	if err != nil {
		t.Fatalf("unmarshal page request: %v", err)
	}
	if !reflect.DeepEqual(body, PageRequest{}) {
		t.Fatalf("expected the body to be ignored, got %+v", body)
	}
}

func paginatePageRecords(conn *fakeConn, request *PageRequest, opts Options) ([]record, CursorResponse, error) {
	db := newFakeDB(conn)
	items := []record{}

	return Paginate(context.Background(), db.NewSelect().Model(&items), request, record{}, opts)
}

func TestPaginatePageRequest(t *testing.T) {
	filter := []pgquery.Filter{{Field: "rank", Comparison: ">=", Value: 1}}
	request := &PageRequest{Limit: 2, Filter: filter}

	conn := newRecordConn()
	items, resp, err := paginatePageRecords(conn, request, Options{Direction: DESC})
	if err != nil {
		t.Fatalf("paginate: %v", err)
	}

	requireQueries(t, conn, selectRecords+` WHERE ("rank" >= 1) ORDER BY "r".rank DESC, "r".id DESC LIMIT 3`)

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
	request.Cursor = resp.NextCursor
	_, _, err = paginatePageRecords(conn, request, Options{Direction: DESC})
	if err != nil {
		t.Fatalf("paginate next page: %v", err)
	}

	requireQueries(t, conn, selectRecords+` WHERE ("rank" >= 1) AND (("r".rank, "r".id) < (5, 2)) ORDER BY "r".rank DESC, "r".id DESC LIMIT 3`)
}

func TestPaginatePageRequestLeavesTheEffectiveLimit(t *testing.T) {
	tests := []struct {
		name      string
		limit     int
		wantLimit int
	}{
		{name: "default", limit: 0, wantLimit: DEFAULT_LIMIT},
		{name: "clamped", limit: MAX_PAGE_SIZE * 5, wantLimit: MAX_PAGE_SIZE},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			conn := newRecordConn()
			request := &PageRequest{Limit: tc.limit}

			_, _, err := paginatePageRecords(conn, request, Options{Direction: ASC})
			if err != nil {
				t.Fatalf("paginate: %v", err)
			}

			if request.Limit != tc.wantLimit {
				t.Fatalf("expected limit %d, got %d", tc.wantLimit, request.Limit)
			}
		})
	}

	conn := newRecordConn()
	_, _, err := paginatePageRecords(conn, &PageRequest{Limit: -1}, Options{Direction: ASC})
	requireCode(t, err, ez.EINVALID)
	requireQueries(t, conn)
}

// SortFields and AllowCount act on a CursorRequest only: a PageRequest pages
// in the default order and never counts.
func TestPaginatePageRequestIgnoresSortFieldsAndAllowCount(t *testing.T) {
	conn := newRecordConn()
	opts := Options{Direction: ASC, SortFields: recordSortFields, AllowCount: true}

	_, resp, err := paginatePageRecords(conn, &PageRequest{Limit: 2}, opts)
	if err != nil {
		t.Fatalf("paginate: %v", err)
	}

	requireQueries(t, conn, selectRecords+` ORDER BY "r".rank ASC, "r".id ASC LIMIT 3`)
	if resp.TotalCount != nil {
		t.Fatalf("expected no count, got %d", *resp.TotalCount)
	}
}

// A cursor a client sort issued is refused on a PageRequest, which pages only
// in the default order.
func TestPaginatePageRequestRefusesASortedCursor(t *testing.T) {
	conn := newRecordConn()
	opts := Options{Direction: ASC, SortFields: recordSortFields}

	_, resp, err := paginateRecords(conn, &CursorRequest{Limit: 2, Sort: "name"}, opts)
	if err != nil {
		t.Fatalf("paginate: %v", err)
	}

	conn = newRecordConn()
	_, _, err = paginatePageRecords(conn, &PageRequest{Limit: 2, Cursor: resp.NextCursor}, opts)
	requireCode(t, err, ez.EINVALID)
	requireQueries(t, conn)
}
