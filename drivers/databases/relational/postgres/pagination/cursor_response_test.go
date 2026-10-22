package pagination

import (
	"testing"

	"github.com/vanclief/ez"
)

type testItem struct {
	ID int64
}

func (i testItem) GetSortField() string        { return "id" }
func (i testItem) GetSortValue() interface{}   { return i.ID }
func (i testItem) GetUniqueField() string      { return "id" }
func (i testItem) GetUniqueValue() interface{} { return i.ID }

func TestBuildCursorResponseRejectsNonPositiveLimit(t *testing.T) {
	items := []testItem{{ID: 1}, {ID: 2}}

	for _, limit := range []int{0, -1} {
		_, err := BuildCursorResponse(items, limit)
		if err == nil {
			t.Fatalf("limit %d: expected an error", limit)
		}

		if ez.ErrorCode(err) != ez.EINVALID {
			t.Fatalf("limit %d: expected EINVALID, got %s", limit, ez.ErrorCode(err))
		}
	}
}

func TestBuildCursorResponseTrimsToLimit(t *testing.T) {
	items := []testItem{{ID: 1}, {ID: 2}, {ID: 3}}

	resp, err := BuildCursorResponse(items, 2)
	if err != nil {
		t.Fatalf("build response: %v", err)
	}

	got, ok := resp.GetItems().([]testItem)
	if !ok || len(got) != 2 {
		t.Fatalf("expected 2 items, got %#v", resp.GetItems())
	}

	if !resp.HasNextPage || resp.NextCursor == "" {
		t.Fatalf("expected a next page with a cursor, got %+v", resp)
	}

	cursor, err := DecodeCursor(resp.NextCursor)
	if err != nil {
		t.Fatalf("decode cursor: %v", err)
	}

	if cursor.SortValue != int64(2) {
		t.Fatalf("expected cursor at item 2, got %#v", cursor.SortValue)
	}

	resp, err = BuildCursorResponse(items, 3)
	if err != nil {
		t.Fatalf("build response: %v", err)
	}

	if resp.HasNextPage || resp.NextCursor != "" {
		t.Fatalf("expected no next page, got %+v", resp)
	}
}
