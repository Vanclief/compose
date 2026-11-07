package query

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
)

type searchTestModel struct {
	bun.BaseModel `bun:"table:users,alias:u"`

	ID   int64
	Name string
}

func newSearchTestDB() *bun.DB {
	sqldb := sql.OpenDB(pgdriver.NewConnector(
		pgdriver.WithDSN("postgres://postgres@localhost:5432/compose_test?sslmode=disable"),
	))

	return bun.NewDB(sqldb, pgdialect.New())
}

func TestSearchEmptyTermLeavesQueryUntouched(t *testing.T) {
	db := newSearchTestDB()
	defer db.Close()

	want := db.NewSelect().Table("users").Where("id > ?", 1).String()

	for _, term := range []string{"", "   ", "\t\n"} {
		got := Search(db.NewSelect().Table("users").Where("id > ?", 1), term, "name").String()
		require.Equal(t, want, got, "term %q", term)
	}

	got := Search(db.NewSelect().Table("users").Where("id > ?", 1), "alice").String()
	require.Equal(t, want, got, "no columns")
}

func TestSearchDropsNULBytes(t *testing.T) {
	// bun v1.3.0 refuses to format a string with a NUL byte, writing the error
	// into the SQL text, and Postgres text cannot hold one anyway.
	db := newSearchTestDB()
	defer db.Close()

	got := Search(db.NewSelect().Table("users"), "al\x00ice", "name").String()
	require.Equal(t, `SELECT * FROM "users" WHERE (name ILIKE '%alice%')`, got)

	want := db.NewSelect().Table("users").String()
	got = Search(db.NewSelect().Table("users"), "\x00 \x00", "name").String()
	require.Equal(t, want, got, "a term of only NUL bytes and spaces")
}

func TestSearchOneColumn(t *testing.T) {
	db := newSearchTestDB()
	defer db.Close()

	got := Search(db.NewSelect().Table("users"), "  Alice ", "name").String()
	require.Equal(t, `SELECT * FROM "users" WHERE (name ILIKE '%Alice%')`, got)
}

func TestSearchColumnsGroupedAndANDed(t *testing.T) {
	db := newSearchTestDB()
	defer db.Close()

	q := db.NewSelect().Table("users").Where("org_id = ?", 7)
	got := Search(q, "ali", "name", "email").Where("deleted_at IS NULL").String()
	require.Equal(t,
		`SELECT * FROM "users" WHERE (org_id = 7) AND (name ILIKE '%ali%' OR email ILIKE '%ali%') AND (deleted_at IS NULL)`,
		got,
	)
}

func TestSearchEscapesLikeMetacharacters(t *testing.T) {
	db := newSearchTestDB()
	defer db.Close()

	got := Search(db.NewSelect().Table("users"), `50%_off\now`, "name").String()
	require.Equal(t, `SELECT * FROM "users" WHERE (name ILIKE '%50\%\_off\\now%')`, got)
}

func TestSearchEscapesQuotesInInlinedPattern(t *testing.T) {
	db := newSearchTestDB()
	defer db.Close()

	got := Search(db.NewSelect().Table("users"), `o'brien`, "name").String()
	require.Equal(t, `SELECT * FROM "users" WHERE (name ILIKE '%o''brien%')`, got)
}

func TestSearchResolvesTableAlias(t *testing.T) {
	db := newSearchTestDB()
	defer db.Close()

	q := db.NewSelect().Model((*searchTestModel)(nil))
	got := Search(q, "ali", "?TableAlias.name").String()
	require.Equal(t,
		`SELECT "u"."id", "u"."name" FROM "users" AS "u" WHERE ("u".name ILIKE '%ali%')`,
		got,
	)
}
