package pagination

import (
	"database/sql/driver"
	"fmt"
	"reflect"
	"time"

	"github.com/uptrace/bun/schema"
	"github.com/vanclief/ez"
)

// CursorResponse represents a cursor paginated response. Embed it in a list
// response next to the items; it marshals to next_cursor (omitted on the last
// page), has_next_page and total_count (only when the request set
// include_count on a list whose Options set AllowCount).
type CursorResponse struct {
	NextCursor  string `json:"next_cursor,omitempty"`
	HasNextPage bool   `json:"has_next_page"`
	TotalCount  *int   `json:"total_count,omitempty"`
}

// buildPage trims the extra row fetched past limit and, when it was there,
// issues the cursor of the last item kept. A client sort reads the sort value
// from the order's bun field, the default sort from GetSortValue.
func buildPage[T Paginatable](items []T, limit int, order sortOrder) ([]T, CursorResponse, error) {
	if items == nil {
		items = []T{}
	}

	if len(items) <= limit {
		return items, CursorResponse{}, nil
	}

	items = items[:limit]
	last := items[limit-1]

	var sortValue any
	if order.schemaField != nil {
		var err error
		sortValue, err = fieldValue(order.schemaField, last)
		if err != nil {
			return nil, CursorResponse{}, ez.Wrap(err)
		}
	} else {
		sortValue = last.GetSortValue()
	}

	cursor := Cursor{
		SortField:   order.expr,
		SortValue:   sortValue,
		UniqueField: last.GetUniqueField(),
		UniqueValue: last.GetUniqueValue(),
		Direction:   string(order.direction),
	}

	nextCursor, err := EncodeCursor(cursor)
	if err != nil {
		return nil, CursorResponse{}, ez.Wrap(err)
	}

	return items, CursorResponse{NextCursor: nextCursor, HasNextPage: true}, nil
}

// fieldValue reads field from item, a struct or a pointer to one, as the
// cursor stores it. It reads the field, through a pointer field, and rejects
// the NULL shapes: a nil pointer field, a Valuer whose Value is nil, or a nil
// embedded pointer struct on the field's path is ez.EINTERNAL, since a sortable
// column must be NOT NULL. It then normalises a driver.Valuer's Value, or
// otherwise the field itself, to the basic Go type of its kind, so a type with
// its own MarshalJSON does not reach the cursor in that form, and keeps a
// time.Time unchanged. A Valuer whose Value the cursor cannot hold, such as a
// []byte, is ez.EINTERNAL.
func fieldValue(field *schema.Field, item any) (any, error) {
	v := reflect.ValueOf(item)
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	} else {
		// A Valuer with a pointer receiver needs an addressable struct.
		addressable := reflect.New(v.Type()).Elem()
		addressable.Set(v)
		v = addressable
	}

	// Not field.Value, which allocates nil embedded pointers on the caller's row.
	value, err := v.FieldByIndexErr(field.Index)
	if err != nil {
		msg := fmt.Sprintf("Sort column %s is not loaded on the last row of the page", field.Name)
		return nil, ez.New(ez.EINTERNAL, msg, err)
	}

	nullMsg := fmt.Sprintf("Sort column %s is NULL on the last row of the page; a sortable column must be NOT NULL", field.Name)

	if value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return nil, ez.New(ez.EINTERNAL, nullMsg, nil)
		}
		value = value.Elem()
	}

	valuer, ok := value.Addr().Interface().(driver.Valuer)
	if ok {
		sqlValue, err := valuer.Value()
		if err != nil {
			return nil, ez.New(ez.EINTERNAL, "Failed to read the sort value for the cursor", err)
		}
		if sqlValue == nil {
			return nil, ez.New(ez.EINTERNAL, nullMsg, nil)
		}
		value = reflect.ValueOf(sqlValue)
	}

	switch value.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return value.Int(), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return value.Uint(), nil
	case reflect.Float32, reflect.Float64:
		return value.Float(), nil
	case reflect.String:
		return value.String(), nil
	case reflect.Bool:
		return value.Bool(), nil
	}

	if value.Type() == reflect.TypeFor[time.Time]() {
		return value.Interface(), nil
	}

	// Reachable only through a Valuer: modelField accepts no other type.
	msg := fmt.Sprintf("Sort column %s has a Value of type %s, which a cursor cannot hold", field.Name, value.Type())
	return nil, ez.New(ez.EINTERNAL, msg, nil)
}
