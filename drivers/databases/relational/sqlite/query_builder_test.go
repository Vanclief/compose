package sqlite

import (
	"context"

	"github.com/vanclief/compose/drivers/databases/relational"
)

// TestQueryBuilderExecutesSliceFilters pushes the bun.List that QueryBuilder
// emits for slice values through the real SQLite dialect and driver, rather
// than only checking the formatted SQL. It covers the legacy "=" on a slice,
// which maps to IN, and empty slices at the head and tail of a group, which
// must be skipped without leaving a dangling AND.
func (suite *TestSuite) TestQueryBuilderExecutesSliceFilters() {
	db := suite.newFileDB()
	defer db.Close() // nolint:errcheck

	err := db.CreateTables(testModels())
	suite.Require().NoError(err)

	ctx := context.Background()
	items := []testItem{{Name: "a"}, {Name: "b"}, {Name: "c"}}
	_, err = db.NewInsert().Model(&items).Exec(ctx)
	suite.Require().NoError(err)

	groups := []relational.ConditionGroup{
		{
			Conditions: []relational.Condition{
				{Column: "name", Comparison: relational.InOperator, Value: []string{}},
				{Column: "name", Comparison: relational.EqualOperator, LogOp: relational.AndOperator, Value: []string{"a", "b"}},
				{Column: "id", Comparison: relational.NotInOperator, LogOp: relational.AndOperator, Value: []int64{}},
			},
		},
	}

	query, args, err := db.QueryBuilder(groups)
	suite.Require().NoError(err)

	var got []testItem
	err = db.NewSelect().Model(&got).Where(query, args...).Order("name ASC").Scan(ctx)
	suite.Require().NoError(err)
	suite.Equal([]string{"a", "b"}, itemNames(got))

	groups = []relational.ConditionGroup{
		{
			Conditions: []relational.Condition{
				{Column: "name", Comparison: relational.NotInOperator, Value: []string{"a"}},
			},
		},
	}

	query, args, err = db.QueryBuilder(groups)
	suite.Require().NoError(err)

	got = nil
	err = db.NewSelect().Model(&got).Where(query, args...).Order("name ASC").Scan(ctx)
	suite.Require().NoError(err)
	suite.Equal([]string{"b", "c"}, itemNames(got))
}

// itemNames returns the names of items in order, so a query result can be
// compared against the expected rows without caring about generated IDs.
func itemNames(items []testItem) []string {
	names := make([]string, 0, len(items))
	for _, item := range items {
		names = append(names, item.Name)
	}
	return names
}
