package pagination

import "github.com/uptrace/bun"

func ApplyOffsetToQuery(query *bun.SelectQuery, limit, offset int) *bun.SelectQuery {
	return query.Limit(limit).Offset(offset)
}
