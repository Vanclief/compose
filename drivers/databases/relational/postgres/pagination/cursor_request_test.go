package pagination

import (
	"encoding/json"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/vanclief/ez"
)

func TestCursorRequestJSONIgnoresEveryField(t *testing.T) {
	// The client fields are query parameters, never body fields, and Filter
	// is server-side only, so a JSON body sets nothing.
	body := []byte(`{
		"limit": 10,
		"cursor": "abc",
		"sort": "-name",
		"include_count": true,
		"filters": [
			{
				"field": "created_at drop table users",
				"value": 123,
				"comparison": ">="
			}
		],
		"Filter": [{"Field": "x", "Comparison": ">="}]
	}`)

	var request CursorRequest
	err := json.Unmarshal(body, &request)
	if err != nil {
		t.Fatalf("unmarshal cursor request: %v", err)
	}

	if !reflect.DeepEqual(request, CursorRequest{}) {
		t.Fatalf("expected the body to be ignored, got %+v", request)
	}
}

func TestCursorRequestBindsQueryParameters(t *testing.T) {
	// echo binds a GET's query string with the query tag, through an
	// embedding request struct as the apps declare them.
	type listRequest struct {
		CursorRequest
		Name string `json:"-" query:"name"`
	}

	req := httptest.NewRequest(http.MethodGet, "/?limit=10&cursor=abc&sort=-name&include_count=true&name=x", nil)
	c := echo.New().NewContext(req, httptest.NewRecorder())

	var request listRequest
	err := c.Bind(&request)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}

	if request.Limit != 10 || request.Cursor != "abc" || request.Sort != "-name" || !request.IncludeCount || request.Name != "x" {
		t.Fatalf("expected the query parameters to be read, got %+v", request)
	}
}

func TestCursorRequestXMLIgnoresEveryField(t *testing.T) {
	// echo binds an XML body with encoding/xml, which does not honour
	// json:"-", so every field carries xml:"-" too.
	body := []byte(`<CursorRequest>
		<Limit>10</Limit>
		<Filter><Field>password_hash</Field><Comparison>; drop</Comparison></Filter>
	</CursorRequest>`)

	var request CursorRequest
	err := xml.Unmarshal(body, &request)
	if err != nil {
		t.Fatalf("unmarshal cursor request: %v", err)
	}

	if !reflect.DeepEqual(request, CursorRequest{}) {
		t.Fatalf("expected the body to be ignored, got %+v", request)
	}
}

func TestCursorRequestValidateLimit(t *testing.T) {
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
			request := &CursorRequest{Limit: tc.limit}

			err := request.Validate()
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected validation error")
				}
				if ez.ErrorCode(err) != ez.EINVALID {
					t.Fatalf("expected %s, got %s", ez.EINVALID, ez.ErrorCode(err))
				}
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

func TestCursorRequestOrder(t *testing.T) {
	db := newFakeDB(&fakeConn{})
	table := db.Table(reflect.TypeOf(record{}))
	allowlist := map[string]string{
		"name": "name",
		"rank": "rank",
	}
	defaultOrder := sortOrder{expr: "?TableAlias.rank", direction: DESC}

	tests := []struct {
		name       string
		sort       string
		sortFields map[string]string
		want       sortOrder
		wantCode   string
	}{
		{name: "empty sort without allowlist", sort: "", want: defaultOrder},
		{name: "empty sort with allowlist", sort: "", sortFields: allowlist, want: defaultOrder},
		{name: "ascending", sort: "name", sortFields: allowlist, want: sortOrder{expr: `?TableAlias."name"`, schemaField: table.FieldMap["name"], direction: ASC}},
		{name: "descending", sort: "-name", sortFields: allowlist, want: sortOrder{expr: `?TableAlias."name"`, schemaField: table.FieldMap["name"], direction: DESC}},
		{name: "another column", sort: "rank", sortFields: allowlist, want: sortOrder{expr: `?TableAlias."rank"`, schemaField: table.FieldMap["rank"], direction: ASC}},
		{name: "not in allowlist", sort: "email", sortFields: allowlist, wantCode: ez.EINVALID},
		{name: "descending not in allowlist", sort: "-email", sortFields: allowlist, wantCode: ez.EINVALID},
		{name: "double prefix", sort: "--name", sortFields: allowlist, wantCode: ez.EINVALID},
		{name: "column expression as sort", sort: "?TableAlias.name", sortFields: allowlist, wantCode: ez.EINVALID},
		{name: "no allowlist", sort: "name", wantCode: ez.EINVALID},
		{name: "not a column of the model", sort: "name", sortFields: map[string]string{"name": "missing"}, wantCode: ez.EINTERNAL},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			request := &CursorRequest{Sort: tc.sort}

			got, err := request.order(db, record{}, Options{Direction: DESC, SortFields: tc.sortFields})
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("order: %v", err)
				}
				if got != tc.want {
					t.Fatalf("expected %+v, got %+v", tc.want, got)
				}
				return
			}

			if err == nil {
				t.Fatalf("expected %s", tc.wantCode)
			}
			if ez.ErrorCode(err) != tc.wantCode {
				t.Fatalf("expected %s, got %s: %v", tc.wantCode, ez.ErrorCode(err), err)
			}
		})
	}
}

func TestCursorRequestOrderAcceptsSortColumnTypesTheCursorHolds(t *testing.T) {
	db := newFakeDB(&fakeConn{})
	table := db.Table(reflect.TypeOf(valuerRecord{}))

	tests := []struct {
		name   string
		column string
	}{
		// by_value, by_ptr and value_ptr are string kinds, so only the struct
		// and array Valuers exercise the Valuer branch of the type check.
		{name: "value receiver Valuer", column: "by_value"},
		{name: "pointer receiver Valuer", column: "by_ptr"},
		{name: "struct with a pointer receiver Valuer", column: "by_struct"},
		{name: "array kind with a Valuer", column: "uuid"},
		{name: "pointer to a Valuer", column: "value_ptr"},
		{name: "sql.NullString", column: "nullable"},
		{name: "time.Time", column: "created_at"},
		{name: "pointer to time.Time", column: "updated_at"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			request := &CursorRequest{Sort: tc.column}

			got, err := request.order(db, valuerRecord{}, Options{Direction: DESC, SortFields: map[string]string{tc.column: tc.column}})
			if err != nil {
				t.Fatalf("order: %v", err)
			}

			want := sortOrder{expr: `?TableAlias."` + tc.column + `"`, schemaField: table.FieldMap[tc.column], direction: ASC}
			if got != want {
				t.Fatalf("expected %+v, got %+v", want, got)
			}
		})
	}
}

func TestCursorRequestValidateIgnoresSort(t *testing.T) {
	// The sort allowlist is an argument of Paginate, so Validate cannot check Sort.
	request := &CursorRequest{Sort: "name"}

	err := request.Validate()
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func TestCursorRequestOrderRejectsUnknownDirection(t *testing.T) {
	db := newFakeDB(&fakeConn{})

	for _, direction := range []OrderDirection{"", "DESC", "sideways"} {
		request := &CursorRequest{}

		_, err := request.order(db, record{}, Options{Direction: direction})
		if ez.ErrorCode(err) != ez.EINTERNAL {
			t.Fatalf("direction %q: expected %s, got %v", direction, ez.EINTERNAL, err)
		}
	}
}
