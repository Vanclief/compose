package relational

import (
	"database/sql/driver"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
)

// testStatus is a named string type used to check that parseConditions
// handles slices of named types, not only []string.
type testStatus string

// testPayload is a named byte slice type used to check that parseConditions
// rejects any slice with a uint8 element kind, not only the exact []byte type.
type testPayload []byte

// testLevel is a uint8-backed enum used to pin the deliberate trade-off that
// uint8-element slices are treated as bytes, following the database/sql
// convention, so callers must widen to an int type to get IN-list semantics.
type testLevel uint8

// testEnum mirrors the string-backed enums in primitives/enums: a named string
// with a value-receiver Valuer. bun binds it through Value, so it must list.
type testEnum string

func (e testEnum) Value() (driver.Value, error) {
	return string(e), nil
}

// normaliseQuery collapses the variable whitespace parseConditions emits so
// tests can compare against a canonical form. It also drops the space
// QueryBuilder leaves between an opening parenthesis and the first column,
// which is a formatting artefact rather than part of the SQL's meaning.
func normaliseQuery(query string) string {
	q := strings.Join(strings.Fields(query), " ")
	return strings.ReplaceAll(q, "( ", "(")
}

func TestParseConditions(t *testing.T) {
	db := &DB{}

	statuses := []string{"active", "pending"}
	namedStatuses := []testStatus{"active", "pending"}
	ids := []int64{1, 2, 3}
	levels := []int{1, 2}
	uuids := []uuid.UUID{
		uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		uuid.MustParse("22222222-2222-2222-2222-222222222222"),
	}
	enumValues := []testEnum{"active", "pending"}
	amounts := []float64{1.5, 2.5}

	// A nil slice must be skipped exactly like an empty one.
	var nilStatuses []string

	tests := []struct {
		name       string
		conditions []Condition
		wantQuery  string
		wantArgs   []interface{}
		wantErr    bool
	}{
		{
			name: "string slice with IN operator",
			conditions: []Condition{
				{Column: "status", Comparison: InOperator, Value: statuses},
			},
			wantQuery: "status IN (?)",
			wantArgs:  []interface{}{bun.List(statuses)},
		},
		{
			name: "string slice with zero-value comparison defaults to IN",
			conditions: []Condition{
				{Column: "status", Value: statuses},
			},
			wantQuery: "status IN (?)",
			wantArgs:  []interface{}{bun.List(statuses)},
		},
		{
			name: "named string slice type",
			conditions: []Condition{
				{Column: "status", Comparison: InOperator, Value: namedStatuses},
			},
			wantQuery: "status IN (?)",
			wantArgs:  []interface{}{bun.List(namedStatuses)},
		},
		{
			name: "int64 slice",
			conditions: []Condition{
				{Column: "id", Comparison: InOperator, Value: ids},
			},
			wantQuery: "id IN (?)",
			wantArgs:  []interface{}{bun.List(ids)},
		},
		{
			name: "uuid slice",
			conditions: []Condition{
				{Column: "id", Comparison: InOperator, Value: uuids},
			},
			wantQuery: "id IN (?)",
			wantArgs:  []interface{}{bun.List(uuids)},
		},
		{
			name: "empty string slice is skipped",
			conditions: []Condition{
				{Column: "status", Comparison: InOperator, Value: []string{}},
			},
			wantQuery: "",
		},
		{
			name: "string slice with NOT IN operator",
			conditions: []Condition{
				{Column: "status", Comparison: NotInOperator, Value: statuses},
			},
			wantQuery: "status NOT IN (?)",
			wantArgs:  []interface{}{bun.List(statuses)},
		},
		{
			name: "string slice with = operator is accepted as IN",
			conditions: []Condition{
				{Column: "status", Comparison: EqualOperator, Value: statuses},
			},
			wantQuery: "status IN (?)",
			wantArgs:  []interface{}{bun.List(statuses)},
		},
		{
			name: "byte slice is an error",
			conditions: []Condition{
				{Column: "payload", Comparison: InOperator, Value: []byte("abc")},
			},
			wantErr: true,
		},
		{
			name: "non-zero int is emitted",
			conditions: []Condition{
				{Column: "count", Comparison: EqualOperator, Value: 5},
			},
			wantQuery: "count = ?",
			wantArgs:  []interface{}{5},
		},
		{
			name: "zero int is skipped",
			conditions: []Condition{
				{Column: "count", Comparison: EqualOperator, Value: 0},
			},
			wantQuery: "",
		},
		{
			name: "string and string slice joined with AND",
			conditions: []Condition{
				{Column: "name", Comparison: EqualOperator, Value: "x"},
				{Column: "status", Comparison: InOperator, LogOp: AndOperator, Value: statuses},
			},
			wantQuery: "name = ? AND status IN (?)",
			wantArgs:  []interface{}{"x", bun.List(statuses)},
		},
		{
			name: "empty slice in the middle does not leave a dangling AND",
			conditions: []Condition{
				{Column: "name", Comparison: EqualOperator, Value: "x"},
				{Column: "status", Comparison: InOperator, LogOp: AndOperator, Value: []string{}},
				{Column: "id", Comparison: EqualOperator, LogOp: AndOperator, Value: int64(7)},
			},
			wantQuery: "name = ? AND id = ?",
			wantArgs:  []interface{}{"x", int64(7)},
		},
		{
			name:       "no conditions is an error",
			conditions: []Condition{},
			wantErr:    true,
		},
		{
			name: "leading empty slice does not leave a dangling AND",
			conditions: []Condition{
				{Column: "status", Comparison: InOperator, Value: []string{}},
				{Column: "id", Comparison: EqualOperator, LogOp: AndOperator, Value: int64(7)},
			},
			wantQuery: "id = ?",
			wantArgs:  []interface{}{int64(7)},
		},
		{
			name: "leading empty string does not leave a dangling AND",
			conditions: []Condition{
				{Column: "name", Comparison: EqualOperator, Value: ""},
				{Column: "id", Comparison: EqualOperator, LogOp: AndOperator, Value: int64(7)},
			},
			wantQuery: "id = ?",
			wantArgs:  []interface{}{int64(7)},
		},
		{
			name: "two leading skipped conditions then OR",
			conditions: []Condition{
				{Column: "name", Comparison: EqualOperator, Value: ""},
				{Column: "status", Comparison: InOperator, LogOp: AndOperator, Value: []string{}},
				{Column: "id", Comparison: EqualOperator, LogOp: OrOperator, Value: int64(7)},
			},
			wantQuery: "id = ?",
			wantArgs:  []interface{}{int64(7)},
		},
		{
			name: "empty slice with = operator is skipped",
			conditions: []Condition{
				{Column: "status", Comparison: EqualOperator, Value: []string{}},
			},
			wantQuery: "",
		},
		{
			name: "empty slice with NOT IN is skipped",
			conditions: []Condition{
				{Column: "status", Comparison: NotInOperator, Value: []string{}},
			},
			wantQuery: "",
		},
		{
			name: "json.RawMessage is an error, not a list",
			conditions: []Condition{
				{Column: "payload", Comparison: InOperator, Value: json.RawMessage(`{"a":1}`)},
			},
			wantErr: true,
		},
		{
			name: "named byte slice type is an error, not a list",
			conditions: []Condition{
				{Column: "payload", Comparison: InOperator, Value: testPayload("abc")},
			},
			wantErr: true,
		},
		{
			name: "uint8 backed enum slice is an error",
			conditions: []Condition{
				{Column: "level", Comparison: InOperator, Value: []testLevel{1, 2}},
			},
			wantErr: true,
		},
		{
			name: "int slice is a list",
			conditions: []Condition{
				{Column: "level", Comparison: InOperator, Value: levels},
			},
			wantQuery: "level IN (?)",
			wantArgs:  []interface{}{bun.List(levels)},
		},
		{
			name: "nil slice is skipped",
			conditions: []Condition{
				{Column: "status", Comparison: InOperator, Value: nilStatuses},
			},
			wantQuery: "",
		},
		{
			name: "int64 slice with = operator is accepted as IN",
			conditions: []Condition{
				{Column: "id", Comparison: EqualOperator, Value: ids},
			},
			wantQuery: "id IN (?)",
			wantArgs:  []interface{}{bun.List(ids)},
		},
		{
			name: "string slice with != operator is accepted as NOT IN",
			conditions: []Condition{
				{Column: "status", Comparison: NotEqualOperator, Value: statuses},
			},
			wantQuery: "status NOT IN (?)",
			wantArgs:  []interface{}{bun.List(statuses)},
		},
		{
			name: "string slice with LIKE operator is an error",
			conditions: []Condition{
				{Column: "status", Comparison: LikeOperator, Value: statuses},
			},
			wantErr: true,
		},
		{
			name: "enum slice with Valuer is a list",
			conditions: []Condition{
				{Column: "status", Comparison: InOperator, Value: enumValues},
			},
			wantQuery: "status IN (?)",
			wantArgs:  []interface{}{bun.List(enumValues)},
		},
		{
			name: "float slice is a list",
			conditions: []Condition{
				{Column: "amount", Comparison: InOperator, Value: amounts},
			},
			wantQuery: "amount IN (?)",
			wantArgs:  []interface{}{bun.List(amounts)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			query, args, err := db.parseConditions(tt.conditions)
			if tt.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			require.Equal(t, tt.wantQuery, normaliseQuery(query))

			if tt.wantArgs == nil {
				require.Empty(t, args)
				return
			}
			require.Equal(t, tt.wantArgs, args)
		})
	}
}

func TestQueryBuilderSkipsEmptyGroups(t *testing.T) {
	statuses := []string{"active", "pending"}

	groups := []ConditionGroup{
		{
			Conditions: []Condition{
				{Column: "tags", Comparison: InOperator, Value: []string{}},
			},
		},
		{
			Conditions: []Condition{
				{Column: "status", Comparison: InOperator, Value: statuses},
			},
			LogOp: AndOperator,
		},
	}

	query, args, err := (&DB{}).QueryBuilder(groups)
	require.NoError(t, err)
	require.Equal(t, "(status IN (?))", normaliseQuery(query))
	require.Equal(t, []interface{}{bun.List(statuses)}, args)
}

func TestQueryBuilderLeadingSkippedCondition(t *testing.T) {
	groups := []ConditionGroup{
		{
			Conditions: []Condition{
				{Column: "status", Comparison: InOperator, Value: []string{}},
				{Column: "id", Comparison: EqualOperator, LogOp: AndOperator, Value: int64(7)},
			},
		},
	}

	query, args, err := (&DB{}).QueryBuilder(groups)
	require.NoError(t, err)
	require.Equal(t, "(id = ?)", normaliseQuery(query))
	require.Equal(t, []interface{}{int64(7)}, args)
}
