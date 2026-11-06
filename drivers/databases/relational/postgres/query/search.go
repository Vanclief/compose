package query

import (
	"strings"

	"github.com/uptrace/bun"
)

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// Search narrows q to rows where any of the columns contains term, case
// insensitively. The term is trimmed, and an empty term or no columns leaves
// q unchanged. Otherwise the columns become one parenthesised condition,
// (a ILIKE ? OR b ILIKE ?), added with Where so it ANDs with the rest of the
// WHERE clause.
//
// The LIKE metacharacters \, % and _ in term are escaped with a backslash, so
// they match literally; this relies on backslash being the Postgres default
// LIKE escape character, so the query sets no ESCAPE clause.
//
// Columns are only safe with trusted input: each one is interpolated raw into
// SQL (so bun resolves placeholders such as ?TableAlias, and expressions
// work), so it must come from code, never from client input. The term is not
// transformed, so an expression such as unaccent(name) only matches a term
// the caller has unaccented too. A column must not contain a bare ?, which
// bun would read as a placeholder.
// The pattern is not a bound parameter: bun inlines it into the SQL text as a
// string literal escaped for Postgres, and that literal keeps the LIKE
// backslashes only while standard_conforming_strings is on (the Postgres
// default).
//
// ILIKE with a leading wildcard cannot use a btree index. A pg_trgm GIN index
// on the column makes it indexed:
//
//	CREATE EXTENSION IF NOT EXISTS pg_trgm;
//	CREATE INDEX users_name_trgm_idx ON users USING gin (name gin_trgm_ops);
func Search(q *bun.SelectQuery, term string, columns ...string) *bun.SelectQuery {
	term = strings.TrimSpace(term)
	if term == "" || len(columns) == 0 {
		return q
	}

	pattern := "%" + likeEscaper.Replace(term) + "%"

	clauses := make([]string, len(columns))
	args := make([]interface{}, len(columns))
	for i, column := range columns {
		clauses[i] = column + " ILIKE ?"
		args[i] = pattern
	}

	return q.Where(strings.Join(clauses, " OR "), args...)
}
