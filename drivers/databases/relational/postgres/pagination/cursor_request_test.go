package pagination

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	pgquery "github.com/vanclief/compose/drivers/databases/relational/postgres/query"
)

type cursorRequestTestModel struct{}

func (cursorRequestTestModel) GetSortField() string {
	return "id"
}

func (cursorRequestTestModel) GetSortValue() interface{} {
	return nil
}

func (cursorRequestTestModel) GetUniqueField() string {
	return "id"
}

func (cursorRequestTestModel) GetUniqueValue() interface{} {
	return nil
}

func TestCursorRequestDoesNotUnmarshalClientFilters(t *testing.T) {
	body := []byte(`{
		"limit": 10,
		"cursor": "",
		"filters": [
			{
				"field": "created_at drop table users",
				"value": 123,
				"comparison": ">="
			}
		]
	}`)

	var request CursorRequest
	err := json.Unmarshal(body, &request)
	if err != nil {
		t.Fatalf("unmarshal cursor request: %v", err)
	}

	if len(request.Filter) != 0 {
		t.Fatalf("expected client filters to be ignored, got %d", len(request.Filter))
	}
}

func TestApplyCursorToQueryEscapesFilterField(t *testing.T) {
	db := newCursorRequestTestDB()
	defer db.Close()

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
	selectQuery := db.NewSelect().TableExpr("records")

	result, err := ApplyCursorToQuery(selectQuery, request, cursorRequestTestModel{}, ASC)
	if err != nil {
		t.Fatalf("apply cursor to query: %v", err)
	}

	sql := result.String()
	expected := `"created_at drop table users" >= 123`
	if !strings.Contains(sql, expected) {
		t.Fatalf("expected SQL to contain %q, got %q", expected, sql)
	}
}

func TestApplyCursorToQueryRejectsUnsafeFilterComparison(t *testing.T) {
	db := newCursorRequestTestDB()
	defer db.Close()

	request := &CursorRequest{
		Limit: 10,
		Filter: []pgquery.Filter{
			{
				Field:      "created_at",
				Value:      int64(123),
				Comparison: ">= or true",
			},
		},
	}
	selectQuery := db.NewSelect().TableExpr("records")

	_, err := ApplyCursorToQuery(selectQuery, request, cursorRequestTestModel{}, ASC)
	if err == nil {
		t.Fatal("expected unsafe filter comparison to return an error")
	}
}

func newCursorRequestTestDB() *bun.DB {
	sqldb := sql.OpenDB(pgdriver.NewConnector(
		pgdriver.WithDSN("postgres://postgres@localhost:5432/compose_test?sslmode=disable"),
	))

	return bun.NewDB(sqldb, pgdialect.New())
}
