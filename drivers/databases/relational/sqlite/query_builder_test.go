package sqlite

import (
	"context"

	"github.com/vanclief/compose/drivers/databases/relational"
)

// TestQueryBuilderExecutesSliceFilters pushes the SQL that QueryBuilder emits
// for slice values through the real SQLite dialect and driver, rather than
// only checking the formatted SQL. It covers the legacy "=" on a slice, which
// maps to IN, optional empty slices at the head and tail of a group, which
// must be skipped without leaving a dangling AND, and required empty slices,
// which must behave as an empty list: IN matches nothing and NOT IN matches
// every row.
func (suite *TestSuite) TestQueryBuilderExecutesSliceFilters() {
	db := suite.newFileDB()
	defer db.Close() // nolint:errcheck

	suite.seedItems(db, "a", "b", "c")
	ctx := context.Background()

	tests := []struct {
		name   string
		groups []relational.ConditionGroup
		want   []string
	}{
		{
			name: "optional empty slices are skipped around a legacy = list",
			groups: []relational.ConditionGroup{{
				Conditions: []relational.Condition{
					{Column: "name", Comparison: relational.InOperator, Value: []string{}, Optional: true},
					{Column: "name", Comparison: relational.EqualOperator, LogOp: relational.AndOperator, Value: []string{"a", "b"}},
					{Column: "id", Comparison: relational.NotInOperator, LogOp: relational.AndOperator, Value: []int64{}, Optional: true},
				},
			}},
			want: []string{"a", "b"},
		},
		{
			name: "NOT IN excludes the listed rows",
			groups: []relational.ConditionGroup{{
				Conditions: []relational.Condition{
					{Column: "name", Comparison: relational.NotInOperator, Value: []string{"a"}},
				},
			}},
			want: []string{"b", "c"},
		},
		{
			name: "required empty IN matches nothing",
			groups: []relational.ConditionGroup{{
				Conditions: []relational.Condition{
					{Column: "name", Comparison: relational.InOperator, Value: []string{}},
				},
			}},
			want: []string{},
		},
		{
			name: "required empty NOT IN matches every row",
			groups: []relational.ConditionGroup{{
				Conditions: []relational.Condition{
					{Column: "name", Comparison: relational.NotInOperator, Value: []string{}},
				},
			}},
			want: []string{"a", "b", "c"},
		},
	}

	for _, tt := range tests {
		suite.Run(tt.name, func() {
			query, args, err := db.QueryBuilder(tt.groups)
			suite.Require().NoError(err)

			var got []testItem
			err = db.NewSelect().Model(&got).Where(query, args...).Order("name ASC").Scan(ctx)
			suite.Require().NoError(err)
			suite.Equal(tt.want, itemNames(got))
		})
	}
}

// seedItems creates the test schema and inserts one item per name, so a test
// can start from known rows without holding the setup errors in its own scope.
func (suite *TestSuite) seedItems(db *relational.DB, names ...string) {
	err := db.CreateTables(testModels())
	suite.Require().NoError(err)

	items := make([]testItem, 0, len(names))
	for _, name := range names {
		items = append(items, testItem{Name: name})
	}

	_, err = db.NewInsert().Model(&items).Exec(context.Background())
	suite.Require().NoError(err)
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
