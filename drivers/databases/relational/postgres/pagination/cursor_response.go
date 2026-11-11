package pagination

import (
	"database/sql/driver"
	"fmt"
	"reflect"
	"time"

	"github.com/uptrace/bun"
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
//
// A last item whose sort value stands for NULL is ez.EINTERNAL naming the
// sort column, as fieldValue and nullSortValue tell it: Postgres puts NULLs
// first under DESC and last under ASC, and a cursor holding NULL, or the zero
// time a NULL leaves in a nullzero time.Time field, would match no row,
// ending the list early, or every row before it, listing them again. NULL rows
// elsewhere on the page are listed as they are.
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

		if nullSortValue(sortValue, order.nullZero) {
			return nil, CursorResponse{}, nullSortError(order.expr)
		}
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
// the NULL shapes: a nil pointer field, a Valuer whose Value is nil, or a zero
// time.Time field tagged nullzero, which is bun's declaration that zero means
// NULL, is ez.EINTERNAL, since a sortable column must be NOT NULL. A zero
// time.Time in a field without nullzero, behind a non-nil pointer or as a
// Valuer's Value is a stored year-1 value, and a NULL scanned into an integer
// or string field reads as 0 or "", real values; none of these is caught. A
// nil embedded pointer struct on the field's path is ez.EINTERNAL as not
// loaded. It then normalises a driver.Valuer's Value, or otherwise the field
// itself, to the basic Go type of its kind, so a type with its own MarshalJSON
// does not reach the cursor in that form, and keeps a time.Time unchanged. A
// Valuer whose Value the cursor cannot hold, such as a []byte, is
// ez.EINTERNAL.
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

	// A zero time.Time means NULL only as the field's own value, and only
	// when the field is tagged nullzero
	zeroIsNull := field.NullZero

	if value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return nil, nullSortError(field.Name)
		}
		value = value.Elem()
		zeroIsNull = false
	}

	valuer, ok := value.Addr().Interface().(driver.Valuer)
	if ok {
		sqlValue, err := valuer.Value()
		if err != nil {
			return nil, ez.New(ez.EINTERNAL, "Failed to read the sort value for the cursor", err)
		}
		if sqlValue == nil {
			return nil, nullSortError(field.Name)
		}
		value = reflect.ValueOf(sqlValue)
		zeroIsNull = false
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
		t := value.Interface().(time.Time)
		if zeroIsNull && t.IsZero() {
			return nil, nullSortError(field.Name)
		}
		return t, nil
	}

	// Reachable only through a Valuer: modelField accepts no other type.
	msg := fmt.Sprintf("Sort column %s has a Value of type %s, which a cursor cannot hold", field.Name, value.Type())
	return nil, ez.New(ez.EINTERNAL, msg, nil)
}

// nullSortValue reports whether value, a default sort value as GetSortValue
// returns it, stands for NULL: nil, a nil pointer, a driver.Valuer whose
// Value is nil, a zero bun.NullTime, directly or behind a pointer, since that
// type reads NULL as zero, and a zero time.Time returned directly when nullZero,
// the sort column being a bun field tagged nullzero. A zero time.Time behind a
// non-nil pointer or as a Valuer's Value is a stored year-1 value, and a NULL
// GetSortValue returns as 0 or "" is not caught. A Valuer whose Value fails
// is not NULL: the cursor encodes the value itself, as it always did, and
// Value is called here only to look for NULL.
func nullSortValue(value any, nullZero bool) bool {
	if value == nil {
		return true
	}

	v := reflect.ValueOf(value)
	if v.Kind() == reflect.Pointer && v.IsNil() {
		return true
	}

	valuer, ok := value.(driver.Valuer)
	if ok {
		sqlValue, err := valuer.Value()
		return err == nil && sqlValue == nil
	}

	switch t := value.(type) {
	case time.Time:
		return nullZero && t.IsZero()
	case bun.NullTime:
		return t.IsZero()
	case *bun.NullTime:
		return t.IsZero()
	}

	return false
}

// nullSortError is the ez.EINTERNAL for a page whose last row has a NULL in
// the sort column named column. A column the query does not select reads the
// same way, so the message names both causes.
func nullSortError(column string) error {
	msg := fmt.Sprintf("Sort column %s is NULL on the last row of the page, or the query does not select it; a sortable column must be NOT NULL and selected", column)
	return ez.New(ez.EINTERNAL, msg, nil)
}
