package postgres

import (
	"context"
	"fmt"

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
}

type Condition struct {
	Column     string
	Comparison Operator
	LogOp      LogicalOperator
	Value      interface{}
}

func (db *DB) QueryBuilder(groups []ConditionGroup) (query string, queryArgs []interface{}, err error) {
	const op = "DB.QueryBuilder"

	for i := range groups {
		groupQuery, groupQueryArgs, err := db.parseConditions(groups[i].Conditions)
		if err != nil {
			return "", nil, ez.Wrap(op, err)
		}

		if groupQuery == "" {
			continue
		}

		if query == "" {
			query = fmt.Sprintf("(%s)", groupQuery)
			queryArgs = groupQueryArgs
		} else {
			query = fmt.Sprintf("%s AND (%s)", query, groupQuery)
			queryArgs = append(queryArgs, groupQueryArgs...)
		}
	}

	return query, queryArgs, nil
}

func (db *DB) parseConditions(conditions []Condition) (query string, queryArgs []interface{}, err error) {
	const op = "DB.parseConditions"

	if len(conditions) == 0 {
		return "", nil, ez.New(op, ez.EINVALID, "Need at least 1 element", nil)
	}

	for i, c := range conditions {
		if i == 0 {
			c.LogOp.Value = ""
		}

		switch arg := c.Value.(type) {
		case int:
		case int64:
			if arg != 0 {
				// LogOp, Column, Comparison: AND column = ?
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
		case bool:
			query += fmt.Sprintf(" %s %s %s ?", c.LogOp.Value, bun.Ident(c.Column), c.Comparison.Value)
			queryArgs = append(queryArgs, c.Value)

		case []int64:
			if len(arg) > 0 {
				query += fmt.Sprintf(" %s %s IN (?)", c.LogOp.Value, bun.Ident(c.Column))
				queryArgs = append(queryArgs, bun.In(c.Value))

			}

		default:
			return "", nil, ez.New(op, ez.EINVALID, "Invalid exact query type", nil)
		}
	}

	return query, queryArgs, nil
}

func (db *DB) AddLimitAndOffset(query *bun.SelectQuery, limit, offset int) *bun.SelectQuery {
	return query.Limit(limit).Offset(offset)
}

type DateFilter struct {
	DateColumn   string `json:"date_column"`
	FromDate     string `json:"from_date"`
	ToDate       string `json:"to_date"`
	FromDateUnix int64  `json:"-"`
	ToDateUnix   int64  `json:"-"`
}

func (db *DB) AddDateFilters(query *bun.SelectQuery, filters []DateFilter) *bun.SelectQuery {
	for _, filter := range filters {
		if filter.DateColumn != "" {
			if filter.FromDateUnix != 0 {
				query = query.
					Where(fmt.Sprintf("%s >= ?", filter.DateColumn), filter.FromDateUnix)
			}

			if filter.ToDateUnix != 0 {
				query = query.
					Where(fmt.Sprintf("%s <= ?", filter.DateColumn), filter.ToDateUnix)
			}
		}
	}

	return query
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
	BetweenOperator            = Operator{"BETWEEN"}
	LikeOperator               = Operator{"LIKE"}
	ILikeOperator              = Operator{"ILIKE"}
	IsNullOperator             = Operator{"IS NULL"}
	IsNotNullOperator          = Operator{"IS NOT NULL"}
	NotOperator                = Operator{"NOT"}
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
