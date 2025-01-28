package relational

import (
	"github.com/uptrace/bun"
	"github.com/vanclief/ez"
)

// TODO: Deprecate this function in favor of cursor pagination
func (db *DB) AddKeysetPagination(query *bun.SelectQuery, limit int, cursors ...ConditionGroup) (*bun.SelectQuery, error) {
	const op = "DB.AddKeysetPagination"

	if limit > 0 {
		query = query.Limit(limit + 1)
	}

	if len(cursors) == 0 {
		return query, nil
	}

	queryStr, queryArgs, err := db.QueryBuilder(cursors)
	if err != nil {
		return nil, ez.Wrap(op, err)
	}

	return query.Where(queryStr, queryArgs...), nil
}
