package relational

import (
	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

// TODO: Deprecate
func (db *DB) AddLimitAndOffset(query *bun.SelectQuery, limit, offset int) *bun.SelectQuery {
	return query.Limit(limit).Offset(offset)
}

func (db *DB) AddOffsetPagination(query *bun.SelectQuery, limit, offset int) *bun.SelectQuery {
	return query.Limit(limit).Offset(offset)
}

type KeysetCursor struct {
	Column string
	Value  interface{}
}

func (db *DB) AddKeysetPagination(query *bun.SelectQuery, limit int, cursors ...KeysetCursor) *bun.SelectQuery {
	if len(cursors) == 0 {
		return query.Limit(limit)
	}

	setCursor := false

	for _, cursor := range cursors {
		if cursor.Column != "" && hasValue(cursor.Value) {
			query = query.Where(cursor.Column+" < ?", cursor.Value)
			setCursor = true
		}
	}

	if setCursor {
		// We want to query + 1 to know if there are more pages
		query = query.Limit(limit + 1)
	}

	return query
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
