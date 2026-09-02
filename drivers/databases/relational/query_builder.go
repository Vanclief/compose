package relational

import (
	"context"
	"fmt"
	"reflect"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
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
// Slice values become "column IN (?)" or "NOT IN (?)"; = and != are accepted as
// aliases. An empty or nil slice is skipped like any other zero value. A byte
// slice is a blob, not a list, and is rejected.
type Condition struct {
	Column     string
	Comparison Operator
	LogOp      LogicalOperator
	Value      interface{}
}

// QueryBuilder builds a WHERE clause and its arguments from condition groups.
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
			// Any other slice becomes an IN / NOT IN list.
			v := reflect.ValueOf(c.Value)
			if v.Kind() != reflect.Slice {
				return "", nil, ez.New(ez.EINVALID, "Query value type is not supported", nil)
			}
			if v.Type().Elem().Kind() == reflect.Uint8 {
				return "", nil, ez.New(ez.EINVALID, "Byte slices are not lists", nil)
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
