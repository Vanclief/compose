package relational

import (
	"github.com/google/uuid"
	"github.com/uptrace/bun"
	"github.com/vanclief/ez"
)

// TODO: Deprecate
func (db *DB) AddLimitAndOffset(query *bun.SelectQuery, limit, offset int) *bun.SelectQuery {
	return query.Limit(limit).Offset(offset)
}

func (db *DB) AddOffsetPagination(query *bun.SelectQuery, limit, offset int) *bun.SelectQuery {
	return query.Limit(limit).Offset(offset)
}

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

func hasValue(i interface{}) bool {
	if i == nil {
		return false
	}

	switch v := i.(type) {
	case int:
		return v != 0
	case int8:
		return v != 0
	case int16:
		return v != 0
	case int32:
		return v != 0
	case int64:
		return v != 0
	case float32:
		return v != 0.0
	case float64:
		return v != 0.0
	case string:
		return v != ""
	case bool:
		return v
	case uuid.UUID:
		return v != uuid.Nil
	default:
		// Not a supported type
		return false
	}
}
