package pagination

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/vanclief/compose/drivers/databases/relational"
	"github.com/vanclief/ez"
)

// datedRecord sorts by created_at by default, a nullable column scanned into
// a time.Time tagged nullzero, bun's declaration that its zero time is NULL.
type datedRecord struct {
	bun.BaseModel `bun:"table:pagination_dated,alias:d"`

	ID        int64     `bun:"id,pk"`
	CreatedAt time.Time `bun:"created_at,nullzero"`
}

func (r datedRecord) GetSortField() string        { return "?TableAlias.created_at" }
func (r datedRecord) GetSortValue() interface{}   { return r.CreatedAt }
func (r datedRecord) GetUniqueField() string      { return "?TableAlias.id" }
func (r datedRecord) GetUniqueValue() interface{} { return r.ID }

// datedPlainRecord is datedRecord without nullzero, so the zero time a NULL
// leaves in it reads as year 1.
type datedPlainRecord struct {
	bun.BaseModel `bun:"table:pagination_dated,alias:d"`

	ID        int64     `bun:"id,pk"`
	CreatedAt time.Time `bun:"created_at"`
}

func (r datedPlainRecord) GetSortField() string        { return "?TableAlias.created_at" }
func (r datedPlainRecord) GetSortValue() interface{}   { return r.CreatedAt }
func (r datedPlainRecord) GetUniqueField() string      { return "?TableAlias.id" }
func (r datedPlainRecord) GetUniqueValue() interface{} { return r.ID }

// datedPointerRecord is datedRecord with a *time.Time, nil for NULL.
type datedPointerRecord struct {
	bun.BaseModel `bun:"table:pagination_dated,alias:d"`

	ID        int64      `bun:"id,pk"`
	CreatedAt *time.Time `bun:"created_at"`
}

func (r datedPointerRecord) GetSortField() string        { return "?TableAlias.created_at" }
func (r datedPointerRecord) GetSortValue() interface{}   { return r.CreatedAt }
func (r datedPointerRecord) GetUniqueField() string      { return "?TableAlias.id" }
func (r datedPointerRecord) GetUniqueValue() interface{} { return r.ID }

// yearOneRecord sorts by created_at, a NOT NULL column without nullzero, in
// which bun writes a zero time.Time as 0001-01-01 00:00:00+00.
type yearOneRecord struct {
	bun.BaseModel `bun:"table:pagination_year_one,alias:y"`

	ID        int64     `bun:"id,pk"`
	CreatedAt time.Time `bun:"created_at,notnull"`
}

func (r yearOneRecord) GetSortField() string        { return "?TableAlias.created_at" }
func (r yearOneRecord) GetSortValue() interface{}   { return r.CreatedAt }
func (r yearOneRecord) GetUniqueField() string      { return "?TableAlias.id" }
func (r yearOneRecord) GetUniqueValue() interface{} { return r.ID }

// newDatedDB returns a test database holding eight pagination_dated rows,
// ids 2, 5 and 7 with a NULL created_at. Postgres sorts NULLs first under
// DESC and last under ASC.
func newDatedDB(t *testing.T) *relational.DB {
	t.Helper()

	db := newPostgresDB(t)
	ctx := context.Background()

	_, err := db.ExecContext(ctx, "CREATE TABLE pagination_dated (id bigint PRIMARY KEY, created_at timestamptz)")
	if err != nil {
		t.Fatalf("create table: %v", err)
	}

	_, err = db.ExecContext(ctx, `INSERT INTO pagination_dated (id, created_at)
		SELECT id, CASE WHEN id IN (2, 5, 7) THEN NULL ELSE timestamptz '2026-11-11 10:00:00+00' + id * interval '1 minute' END
		FROM generate_series(1, 8) AS id`)
	if err != nil {
		t.Fatalf("insert rows: %v", err)
	}

	return db
}

// pageDated pages through the model's table limit rows at a time, under the
// client sort sort on created_at or, when sort is empty, the default sort in
// direction, and returns the ids listed until the list ends or a page fails,
// with that page's error.
func pageDated[T Paginatable](t *testing.T, db *relational.DB, model T, sort string, direction OrderDirection, limit int) ([]int64, error) {
	t.Helper()

	var ids []int64
	request := &CursorRequest{Limit: limit, Sort: sort}
	opts := Options{Direction: direction, SortFields: map[string]string{"created_at": "created_at"}}
	for range 10 {
		items := []T{}
		page, resp, err := Paginate(context.Background(), db.NewSelect().Model(&items), request, model, opts)
		if err != nil {
			return ids, err
		}

		for _, item := range page {
			ids = append(ids, item.GetUniqueValue().(int64))
		}

		if !resp.HasNextPage {
			return ids, nil
		}
		request.Cursor = resp.NextCursor
	}

	t.Fatalf("paging did not end, listed %v", ids)
	return nil, nil
}

func TestPaginatePostgresRefusesNullSortValueOnTheLastRow(t *testing.T) {
	db := newDatedDB(t)

	tests := []struct {
		name      string
		direction OrderDirection
		limit     int
		wantIDs   []int64
		wantCode  string
	}{
		// The first page is three NULL rows cut to two. Its cursor used to
		// match no row, so the list ended after 7 and 5.
		{name: "descending, page ending on NULL", direction: DESC, limit: 2, wantCode: ez.EINTERNAL},
		// The first page is the five dated rows and NULL row 2. Its cursor
		// used to match every dated row again for a time.Time, so the list
		// repeated them, and no row for a *time.Time, so it lost 5 and 7.
		{name: "ascending, page ending on NULL", direction: ASC, limit: 6, wantCode: ez.EINTERNAL},
		// The first page holds the NULL rows but ends on a dated one
		{name: "descending, NULL rows inside a page", direction: DESC, limit: 4, wantIDs: []int64{7, 5, 2, 8, 6, 4, 3, 1}},
		// Not caught: no page ends on a NULL row, the cursor's condition is
		// NULL for the NULL rows, so the list ends without 2, 5 and 7.
		{name: "ascending, NULL rows after a cursor", direction: ASC, limit: 2, wantIDs: []int64{1, 3, 4, 6, 8}},
	}

	models := map[string]func(t *testing.T, direction OrderDirection, limit int) ([]int64, error){
		"nullzero time.Time": func(t *testing.T, direction OrderDirection, limit int) ([]int64, error) {
			return pageDated(t, db, datedRecord{}, "", direction, limit)
		},
		"*time.Time": func(t *testing.T, direction OrderDirection, limit int) ([]int64, error) {
			return pageDated(t, db, datedPointerRecord{}, "", direction, limit)
		},
	}

	for modelName, page := range models {
		for _, tc := range tests {
			t.Run(modelName+", "+tc.name, func(t *testing.T) {
				ids, err := page(t, tc.direction, tc.limit)

				if tc.wantCode == "" {
					if err != nil {
						t.Fatalf("paginate: %v", err)
					}
				} else {
					requireCode(t, err, tc.wantCode)
					want := "Sort column ?TableAlias.created_at is NULL on the last row of the page, or the query does not select it; a sortable column must be NOT NULL and selected"
					if ez.ErrorMessage(err) != want {
						t.Fatalf("expected %q, got %q", want, ez.ErrorMessage(err))
					}
				}

				if !slices.Equal(ids, tc.wantIDs) {
					t.Fatalf("expected ids %v before the end, got %v", tc.wantIDs, ids)
				}
			})
		}
	}
}

func TestPaginatePostgresRefusesNullClientSortValueOnTheLastRow(t *testing.T) {
	db := newDatedDB(t)
	items := []datedRecord{}
	request := &CursorRequest{Limit: 2, Sort: "-created_at"}
	opts := Options{Direction: ASC, SortFields: map[string]string{"created_at": "created_at"}}

	_, _, err := Paginate(context.Background(), db.NewSelect().Model(&items), request, datedRecord{}, opts)
	requireCode(t, err, ez.EINTERNAL)
	want := "Sort column created_at is NULL on the last row of the page, or the query does not select it; a sortable column must be NOT NULL and selected"
	if ez.ErrorMessage(err) != want {
		t.Fatalf("expected %q, got %q", want, ez.ErrorMessage(err))
	}
}

func TestPaginatePostgresDoesNotCatchNullInATimeFieldWithoutNullZero(t *testing.T) {
	db := newDatedDB(t)

	// Not caught: without nullzero the NULL rows read as year 1, so the first
	// page ends on one and its cursor matches no row, as before v1.33.0
	ids, err := pageDated(t, db, datedPlainRecord{}, "", DESC, 2)
	if err != nil {
		t.Fatalf("paginate: %v", err)
	}
	if !slices.Equal(ids, []int64{7, 5}) {
		t.Fatalf("expected the list to end after 7 and 5, got %v", ids)
	}
}

func TestPaginatePostgresPagesYearOneTimes(t *testing.T) {
	db := newPostgresDB(t)
	ctx := context.Background()

	_, err := db.NewCreateTable().Model((*yearOneRecord)(nil)).Exec(ctx)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}

	dated := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	rows := []yearOneRecord{
		{ID: 1},
		{ID: 2, CreatedAt: dated.Add(2 * time.Minute)},
		{ID: 3},
		{ID: 4, CreatedAt: dated.Add(4 * time.Minute)},
	}
	_, err = db.NewInsert().Model(&rows).Exec(ctx)
	if err != nil {
		t.Fatalf("insert rows: %v", err)
	}

	var stored int
	err = db.NewSelect().Model((*yearOneRecord)(nil)).ColumnExpr("count(*)").Where("created_at = timestamptz '0001-01-01 00:00:00+00'").Scan(ctx, &stored)
	if err != nil || stored != 2 {
		t.Fatalf("expected bun to store two year-1 times, got %d, %v", stored, err)
	}

	tests := []struct {
		name      string
		sort      string
		direction OrderDirection
		want      []int64
	}{
		{name: "default sort ascending", direction: ASC, want: []int64{1, 3, 2, 4}},
		{name: "default sort descending", direction: DESC, want: []int64{4, 2, 3, 1}},
		{name: "client sort ascending", sort: "created_at", want: []int64{1, 3, 2, 4}},
		{name: "client sort descending", sort: "-created_at", want: []int64{4, 2, 3, 1}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			direction := tc.direction
			if direction == "" {
				direction = ASC
			}

			ids, err := pageDated(t, db, yearOneRecord{}, tc.sort, direction, 1)
			if err != nil {
				t.Fatalf("paginate: %v", err)
			}
			if !slices.Equal(ids, tc.want) {
				t.Fatalf("expected %v, got %v", tc.want, ids)
			}
		})
	}
}
