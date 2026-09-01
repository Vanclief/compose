package relational

import (
	"context"
	"database/sql/driver"
	"fmt"
	"reflect"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/schema"
	"github.com/vanclief/ez"
)

func (db *DB) QueryCount(ctx context.Context, model interface{}, query string, conditions ...interface{}) (int, error) {
	return db.NewSelect().
		Model(model).
		Where(query, conditions...).
		Count(ctx)
}

type ConditionGroup struct {
	Conditions []Condition
	LogOp      LogicalOperator
}

// Condition values are only safe with trusted input: Column is interpolated
// raw into SQL (to support expressions like CONCAT(...) or unaccent(...)), so
// it must come from code, never from client input. Only Value is bound as a
// parameter.
//
// Slice values become "column IN (?)" or "NOT IN (?)". For compatibility, = and
// != are accepted as aliases of IN and NOT IN. An empty or nil slice is skipped
// like any other zero value. Elements must be strings, ints, uints wider than a
// byte, floats, bools, or types implementing driver.Valuer such as uuid.UUID.
// Byte slices and types bun expands as raw SQL, such as bun.Safe, are rejected.
type Condition struct {
	Column     string
	Comparison Operator
	LogOp      LogicalOperator
	Value      interface{}
}

// QueryBuilder builds a WHERE clause and its arguments from condition groups.
//
// The builder is frozen and slated for deprecation. It will not gain new
// operators or value types. New queries should use bun's Where, WhereOr,
// WhereGroup and bun.List directly. A formal deprecation notice will follow
// in a later release.
func (db *DB) QueryBuilder(groups []ConditionGroup) (query string, queryArgs []interface{}, err error) {
	for i := range groups {
		groupQuery, groupQueryArgs, err := db.parseConditions(groups[i].Conditions)
		if err != nil {
			return "", nil, ez.Wrap(err)
		}

		if groupQuery == "" {
			continue
		}

		// Default to using an AND
		if groups[i].LogOp == (LogicalOperator{}) {
			groups[i].LogOp = AndOperator
		}

		if query == "" {
			query = fmt.Sprintf("(%s)", groupQuery)
			queryArgs = groupQueryArgs
		} else {
			query = fmt.Sprintf("%s %s (%s)", query, groups[i].LogOp, groupQuery)
			queryArgs = append(queryArgs, groupQueryArgs...)
		}
	}

	return query, queryArgs, nil
}

func (db *DB) parseConditions(conditions []Condition) (query string, queryArgs []interface{}, err error) {
	if len(conditions) == 0 {
		return "", nil, ez.New(ez.EINVALID, "Need at least 1 element", nil)
	}

	for _, c := range conditions {
		if query == "" {
			c.LogOp.Value = ""
		}

		switch arg := c.Value.(type) {
		case int:
			if arg != 0 {
				// LogOp, Column, Comparison: AND column = ?
				query += fmt.Sprintf(" %s %s %s ?", c.LogOp.Value, bun.Ident(c.Column), c.Comparison.Value)
				queryArgs = append(queryArgs, c.Value)
			}
		case int64:
			if arg != 0 {
				query += fmt.Sprintf(" %s %s %s ?", c.LogOp.Value, bun.Ident(c.Column), c.Comparison.Value)
				queryArgs = append(queryArgs, c.Value)
			}
		case string:
			if arg != "" {
				query += fmt.Sprintf(" %s %s %s ?", c.LogOp.Value, bun.Ident(c.Column), c.Comparison.Value)
				parsedVal := c.Value
				if c.Comparison == LikeOperator || c.Comparison == ILikeOperator {
					parsedVal = fmt.Sprintf("%%%s%%", c.Value)
				}
				queryArgs = append(queryArgs, parsedVal)
			}

		case uuid.UUID:
			if arg != uuid.Nil {
				query += fmt.Sprintf(" %s %s %s ?", c.LogOp.Value, bun.Ident(c.Column), c.Comparison.Value)
				queryArgs = append(queryArgs, c.Value)
			}
		case bool:
			query += fmt.Sprintf(" %s %s %s ?", c.LogOp.Value, bun.Ident(c.Column), c.Comparison.Value)
			queryArgs = append(queryArgs, c.Value)

		default:
			// Any slice with a bindable element type becomes an IN / NOT IN list.
			v := reflect.ValueOf(c.Value)
			if v.Kind() != reflect.Slice {
				return "", nil, ez.New(ez.EINVALID, "Query value type is not supported", nil)
			}
			if !isBindableElem(v.Type().Elem()) {
				return "", nil, ez.New(ez.EINVALID, "Slice element type is not supported", nil)
			}

			var op string
			switch c.Comparison {
			case Operator{}, InOperator, EqualOperator:
				op = "IN"
			case NotInOperator, NotEqualOperator:
				op = "NOT IN"
			default:
				return "", nil, ez.New(ez.EINVALID, "Slice values only support IN and NOT IN", nil)
			}

			if v.Len() == 0 {
				continue
			}

			query += fmt.Sprintf(" %s %s %s (?)", c.LogOp.Value, bun.Ident(c.Column), op)
			queryArgs = append(queryArgs, bun.List(c.Value))
		}
	}

	return query, queryArgs, nil
}

var (
	queryAppenderType = reflect.TypeOf((*schema.QueryAppender)(nil)).Elem()
	driverValuerType  = reflect.TypeOf((*driver.Valuer)(nil)).Elem()
)

// isBindableElem reports whether bun binds a slice element of this type as
// data. Types bun expands as raw SQL, such as bun.Safe, are rejected, and so
// are types bun has no appender for, which would panic during formatting.
// Interface element types are rejected outright because bun unwraps each
// element at formatting time, so the static type says nothing about safety.
func isBindableElem(t reflect.Type) bool {
	if t.Kind() == reflect.Interface {
		return false
	}
	if t.Implements(queryAppenderType) {
		return false
	}
	if reflect.PointerTo(t).Implements(queryAppenderType) {
		return false
	}
	if t.Implements(driverValuerType) {
		return true
	}
	if reflect.PointerTo(t).Implements(driverValuerType) {
		return true
	}

	switch t.Kind() {
	case reflect.String, reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return true
	}

	return false
}

type Operator struct {
	Value string
}

func (s Operator) String() string {
	return s.Value
}

var (
	EqualOperator              = Operator{"="}
	GreaterThanOperator        = Operator{">"}
	LessThanOperator           = Operator{"<"}
	GreaterThanOrEqualOperator = Operator{">="}
	LessThanOrEqualOperator    = Operator{"<="}
	NotEqualOperator           = Operator{"!="}
	InOperator                 = Operator{"IN"}
	NotInOperator              = Operator{"NOT IN"}
	LikeOperator               = Operator{"LIKE"}
	ILikeOperator              = Operator{"ILIKE"}
)

type LogicalOperator struct {
	Value string
}

func (s LogicalOperator) String() string {
	return s.Value
}

var (
	AndOperator = LogicalOperator{"AND"}
	OrOperator  = LogicalOperator{"OR"}
)
