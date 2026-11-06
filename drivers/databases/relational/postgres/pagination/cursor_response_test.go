package pagination

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
	"github.com/vanclief/ez"
)

type testItem struct {
	ID int64
}

func (i testItem) GetSortField() string        { return "id" }
func (i testItem) GetSortValue() interface{}   { return i.ID }
func (i testItem) GetUniqueField() string      { return "id" }
func (i testItem) GetUniqueValue() interface{} { return i.ID }

func TestBuildPageTrimsToLimit(t *testing.T) {
	items := []testItem{{ID: 1}, {ID: 2}, {ID: 3}}
	order := sortOrder{expr: "id", direction: ASC}

	page, resp, err := buildPage(items, 2, order)
	if err != nil {
		t.Fatalf("build page: %v", err)
	}

	if len(page) != 2 {
		t.Fatalf("expected 2 items, got %#v", page)
	}

	if !resp.HasNextPage || resp.NextCursor == "" || resp.TotalCount != nil {
		t.Fatalf("expected a next page with a cursor and no count, got %+v", resp)
	}

	cursor, err := DecodeCursor(resp.NextCursor)
	if err != nil {
		t.Fatalf("decode cursor: %v", err)
	}

	want := Cursor{SortField: "id", SortValue: int64(2), UniqueField: "id", UniqueValue: int64(2), Direction: "asc"}
	if *cursor != want {
		t.Fatalf("expected %+v, got %+v", want, *cursor)
	}

	page, resp, err = buildPage(items, 3, order)
	if err != nil {
		t.Fatalf("build page: %v", err)
	}

	if len(page) != 3 || resp.HasNextPage || resp.NextCursor != "" {
		t.Fatalf("expected the last page, got %d items and %+v", len(page), resp)
	}
}

func TestBuildPageReturnsEmptySliceForNoRows(t *testing.T) {
	page, resp, err := buildPage[testItem](nil, 2, sortOrder{expr: "id", direction: ASC})
	if err != nil {
		t.Fatalf("build page: %v", err)
	}

	if page == nil || len(page) != 0 || resp.HasNextPage {
		t.Fatalf("expected an empty, non-nil page, got %#v and %+v", page, resp)
	}
}

// valueValuer implements driver.Valuer on the value.
type valueValuer string

func (v valueValuer) Value() (driver.Value, error) {
	return "value " + string(v), nil
}

// structValuer is a struct, not a basic kind, so only its pointer-receiver
// Value makes it a sort column.
type structValuer struct {
	S string
}

func (v *structValuer) Value() (driver.Value, error) {
	return "struct " + v.S, nil
}

// pointerValuer implements driver.Valuer on the pointer.
type pointerValuer string

func (v *pointerValuer) Value() (driver.Value, error) {
	return "pointer " + string(*v), nil
}

// bytesValuer implements driver.Valuer with a []byte Value.
type bytesValuer string

func (v bytesValuer) Value() (driver.Value, error) {
	return []byte(v), nil
}

// intValuer implements driver.Valuer with an int Value.
type intValuer int

func (v intValuer) Value() (driver.Value, error) {
	return int(v), nil
}

// int32Valuer implements driver.Valuer with an int32 Value.
type int32Valuer int32

func (v int32Valuer) Value() (driver.Value, error) {
	return int32(v), nil
}

type valuerRecord struct {
	bun.BaseModel `bun:"table:records,alias:r"`

	ID         int64          `bun:"id,pk"`
	ByValue    valueValuer    `bun:"by_value"`
	ByPtr      pointerValuer  `bun:"by_ptr"`
	ByStruct   structValuer   `bun:"by_struct"`
	UUID       uuid.UUID      `bun:"uuid"`
	Status     *status        `bun:"status"`
	ValuePtr   *valueValuer   `bun:"value_ptr"`
	Nullable   sql.NullString `bun:"nullable"`
	Bytes      bytesValuer    `bun:"bytes"`
	IntValue   intValuer      `bun:"int_value"`
	Int32Value int32Valuer    `bun:"int32_value"`
	Tags       []string       `bun:"tags,array"`
	Any        any            `bun:"any"`
	CreatedAt  time.Time      `bun:"created_at"`
	UpdatedAt  *time.Time     `bun:"updated_at"`
}

func (r valuerRecord) GetSortField() string        { return "?TableAlias.id" }
func (r valuerRecord) GetSortValue() interface{}   { return r.ID }
func (r valuerRecord) GetUniqueField() string      { return "?TableAlias.id" }
func (r valuerRecord) GetUniqueValue() interface{} { return r.ID }

func TestBuildPageClientSortValue(t *testing.T) {
	table := newFakeDB(&fakeConn{}).Table(reflect.TypeOf(valuerRecord{}))
	open := status(1)
	valuer := valueValuer("b")
	createdAt := time.Date(2026, 11, 5, 15, 52, 17, 123456789, time.UTC)
	row := valuerRecord{
		ID:         1,
		ByValue:    "a",
		ByPtr:      "b",
		ByStruct:   structValuer{S: "s"},
		UUID:       uuid.MustParse("0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0"),
		Status:     &open,
		ValuePtr:   &valuer,
		Nullable:   sql.NullString{String: "c", Valid: true},
		IntValue:   2,
		Int32Value: 3,
		CreatedAt:  createdAt,
	}

	tests := []struct {
		column string
		want   any
	}{
		{column: "by_value", want: "value a"},
		{column: "by_ptr", want: "pointer b"},
		{column: "by_struct", want: "struct s"},
		// An array kind with a Valuer, stored as its Value, the text form.
		{column: "uuid", want: "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0"},
		{column: "status", want: int64(1)},
		{column: "value_ptr", want: "value b"},
		{column: "nullable", want: "c"},
		{column: "int_value", want: int64(2)},
		{column: "int32_value", want: int64(3)},
		// A time.Time is stored as its JSON form, which Postgres reads back as a timestamp.
		{column: "created_at", want: "2026-11-05T15:52:17.123456789Z"},
	}

	for _, tc := range tests {
		t.Run(tc.column, func(t *testing.T) {
			order := sortOrder{expr: "?TableAlias." + tc.column, schemaField: table.FieldMap[tc.column], direction: ASC}

			_, resp, err := buildPage([]valuerRecord{row, row}, 1, order)
			if err != nil {
				t.Fatalf("build page: %v", err)
			}

			cursor, err := DecodeCursor(resp.NextCursor)
			if err != nil {
				t.Fatalf("decode cursor: %v", err)
			}
			if cursor.SortValue != tc.want {
				t.Fatalf("expected %#v, got %#v", tc.want, cursor.SortValue)
			}
		})
	}
}

func TestBuildPageRejectsClientSortValueTheCursorCannotHold(t *testing.T) {
	table := newFakeDB(&fakeConn{}).Table(reflect.TypeOf(valuerRecord{}))
	row := valuerRecord{ID: 1, Bytes: "a"}

	tests := []struct {
		column  string
		message string
	}{
		{column: "status", message: "Sort column status is NULL on the last row of the page; a sortable column must be NOT NULL"},
		{column: "nullable", message: "Sort column nullable is NULL on the last row of the page; a sortable column must be NOT NULL"},
		{column: "bytes", message: "Sort column bytes has a Value of type []uint8, which a cursor cannot hold"},
	}

	for _, tc := range tests {
		t.Run(tc.column, func(t *testing.T) {
			order := sortOrder{expr: "?TableAlias." + tc.column, schemaField: table.FieldMap[tc.column], direction: ASC}

			_, resp, err := buildPage([]valuerRecord{row, row}, 1, order)
			if ez.ErrorCode(err) != ez.EINTERNAL {
				t.Fatalf("expected %s, got %v", ez.EINTERNAL, err)
			}
			if ez.ErrorMessage(err) != tc.message {
				t.Fatalf("expected %q, got %q", tc.message, ez.ErrorMessage(err))
			}
			if resp.NextCursor != "" {
				t.Fatalf("expected no cursor, got %q", resp.NextCursor)
			}
		})
	}
}

// Audit is embedded by pointer, so a row whose relation was not loaded has it nil.
type Audit struct {
	UpdatedAt int64 `bun:"updated_at"`
}

type embeddedRecord struct {
	bun.BaseModel `bun:"table:records,alias:r"`

	ID int64 `bun:"id,pk"`
	*Audit
}

func (r embeddedRecord) GetSortField() string        { return "?TableAlias.id" }
func (r embeddedRecord) GetSortValue() interface{}   { return r.ID }
func (r embeddedRecord) GetUniqueField() string      { return "?TableAlias.id" }
func (r embeddedRecord) GetUniqueValue() interface{} { return r.ID }

func TestBuildPageRejectsNilEmbeddedClientSortValue(t *testing.T) {
	table := newFakeDB(&fakeConn{}).Table(reflect.TypeOf(embeddedRecord{}))
	order := sortOrder{expr: "?TableAlias.updated_at", schemaField: table.FieldMap["updated_at"], direction: ASC}
	items := []*embeddedRecord{{ID: 1}, {ID: 2}}

	_, resp, err := buildPage(items, 1, order)
	if ez.ErrorCode(err) != ez.EINTERNAL {
		t.Fatalf("expected %s, got %v", ez.EINTERNAL, err)
	}
	if ez.ErrorMessage(err) != "Sort column updated_at is not loaded on the last row of the page" {
		t.Fatalf("expected the not loaded message, got %q", ez.ErrorMessage(err))
	}
	if resp.NextCursor != "" {
		t.Fatalf("expected no cursor, got %q", resp.NextCursor)
	}
	if items[0].Audit != nil {
		t.Fatalf("expected the caller's row to keep a nil Audit, got %+v", items[0].Audit)
	}
}

// valueAuditRecord embeds Audit by value.
type valueAuditRecord struct {
	bun.BaseModel `bun:"table:records,alias:r"`

	ID int64 `bun:"id,pk"`
	Audit
}

func (r valueAuditRecord) GetSortField() string        { return "?TableAlias.id" }
func (r valueAuditRecord) GetSortValue() interface{}   { return r.ID }
func (r valueAuditRecord) GetUniqueField() string      { return "?TableAlias.id" }
func (r valueAuditRecord) GetUniqueValue() interface{} { return r.ID }

func TestBuildPageEmbeddedClientSortValue(t *testing.T) {
	t.Run("by value", func(t *testing.T) {
		table := newFakeDB(&fakeConn{}).Table(reflect.TypeOf(valueAuditRecord{}))
		order := sortOrder{expr: "?TableAlias.updated_at", schemaField: table.FieldMap["updated_at"], direction: ASC}
		items := []valueAuditRecord{{ID: 1, Audit: Audit{UpdatedAt: 7}}, {ID: 2, Audit: Audit{UpdatedAt: 8}}}

		_, resp, err := buildPage(items, 1, order)
		if err != nil {
			t.Fatalf("build page: %v", err)
		}

		cursor := decodeTestCursor(t, resp.NextCursor)
		if cursor.SortValue != int64(7) {
			t.Fatalf("expected 7, got %#v", cursor.SortValue)
		}
	})

	t.Run("by pointer", func(t *testing.T) {
		table := newFakeDB(&fakeConn{}).Table(reflect.TypeOf(embeddedRecord{}))
		order := sortOrder{expr: "?TableAlias.updated_at", schemaField: table.FieldMap["updated_at"], direction: ASC}
		items := []*embeddedRecord{{ID: 1, Audit: &Audit{UpdatedAt: 7}}, {ID: 2, Audit: &Audit{UpdatedAt: 8}}}

		_, resp, err := buildPage(items, 1, order)
		if err != nil {
			t.Fatalf("build page: %v", err)
		}

		cursor := decodeTestCursor(t, resp.NextCursor)
		if cursor.SortValue != int64(7) {
			t.Fatalf("expected 7, got %#v", cursor.SortValue)
		}
	})
}

// createdRecord sorts by created_at by default, and its GetSortValue
// dereferences CreatedAt, which a query that does not select it leaves nil.
type createdRecord struct {
	bun.BaseModel `bun:"table:records,alias:r"`

	ID        int64      `bun:"id,pk"`
	Name      string     `bun:"name"`
	CreatedAt *time.Time `bun:"created_at"`
}

func (r createdRecord) GetSortField() string        { return "?TableAlias.created_at" }
func (r createdRecord) GetSortValue() interface{}   { return r.CreatedAt.Unix() }
func (r createdRecord) GetUniqueField() string      { return "?TableAlias.id" }
func (r createdRecord) GetUniqueValue() interface{} { return r.ID }

func TestBuildPageClientSortDoesNotReadDefaultSortValue(t *testing.T) {
	table := newFakeDB(&fakeConn{}).Table(reflect.TypeOf(createdRecord{}))
	order := sortOrder{expr: `?TableAlias."name"`, schemaField: table.FieldMap["name"], direction: ASC}
	items := []createdRecord{{ID: 1, Name: "a"}, {ID: 2, Name: "b"}, {ID: 3, Name: "c"}}

	_, resp, err := buildPage(items, 2, order)
	if err != nil {
		t.Fatalf("build page: %v", err)
	}

	if !resp.HasNextPage {
		t.Fatalf("expected a next page, got %+v", resp)
	}

	cursor := decodeTestCursor(t, resp.NextCursor)
	if cursor.SortValue != "b" {
		t.Fatalf("expected the cursor to store name b, got %#v", cursor.SortValue)
	}
}

func TestCursorResponseJSON(t *testing.T) {
	type listResponse struct {
		CursorResponse
		Items []int `json:"items"`
	}

	zero := 0
	total := 42

	tests := []struct {
		name string
		resp CursorResponse
		want string
	}{
		{name: "last page", resp: CursorResponse{}, want: `{"has_next_page":false,"items":[]}`},
		{name: "next page", resp: CursorResponse{NextCursor: "abc", HasNextPage: true}, want: `{"next_cursor":"abc","has_next_page":true,"items":[]}`},
		{name: "with count", resp: CursorResponse{NextCursor: "abc", HasNextPage: true, TotalCount: &total}, want: `{"next_cursor":"abc","has_next_page":true,"total_count":42,"items":[]}`},
		{name: "zero count", resp: CursorResponse{TotalCount: &zero}, want: `{"has_next_page":false,"total_count":0,"items":[]}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(listResponse{CursorResponse: tc.resp, Items: []int{}})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}

			if string(got) != tc.want {
				t.Fatalf("expected %s, got %s", tc.want, got)
			}
		})
	}
}
