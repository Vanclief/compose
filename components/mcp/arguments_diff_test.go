package mcp

import (
	"encoding/json"
	"math"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// referenceApplyInputSchema is applyInputSchema as it was in v1.38.0, which
// decoded the arguments twice, once with every number as a float64 and once
// with unmarshalExact, and applied the defaults to both.
func referenceApplyInputSchema(args json.RawMessage, resolved *jsonschema.Resolved) (json.RawMessage, string) {
	object := map[string]any{}
	exact := map[string]any{}
	if len(args) > 0 {
		err := json.Unmarshal(args, &object)
		if err != nil {
			return nil, `validating "arguments": unmarshaling arguments: ` + err.Error()
		}
		err = unmarshalExact(args, &exact)
		if err != nil {
			return nil, `validating "arguments": unmarshaling arguments: ` + err.Error()
		}
	}
	if object == nil {
		object = map[string]any{}
	}
	if exact == nil {
		exact = map[string]any{}
	}
	var value any = object
	err := resolved.ApplyDefaults(&value)
	if err != nil {
		return nil, "validating \"arguments\": applying schema defaults:\n" + err.Error()
	}
	err = resolved.Validate(&value)
	if err != nil {
		return nil, `validating "arguments": ` + err.Error()
	}
	var input any = exact
	err = resolved.ApplyDefaults(&input)
	if err != nil {
		return nil, "validating \"arguments\": applying schema defaults:\n" + err.Error()
	}
	data, err := json.Marshal(referenceExactNumbers(input))
	if err != nil {
		return nil, `validating "arguments": marshalling with defaults: ` + err.Error()
	}
	return data, ""
}

// referenceExactNumbers is exactNumbers as it was in v1.38.0.
func referenceExactNumbers(v any) any {
	switch t := v.(type) {
	case json.Number:
		f, err := t.Float64()
		if err != nil || (!strings.ContainsAny(t.String(), ".eE") && math.Abs(f) >= 1<<53) {
			return t
		}
		return f
	case map[string]any:
		for key, item := range t {
			t[key] = referenceExactNumbers(item)
		}
	case []any:
		for i, item := range t {
			t[i] = referenceExactNumbers(item)
		}
	}
	return v
}

// diffNumbers are numbers in the spellings an agent may write, among them
// integers a float64 cannot hold and numbers no float64 holds.
var diffNumbers = []string{
	"0", "-0", "1", "-1", "3", "1.0", "3.0", "2.50", "0.1", "1e3", "1E+2", "1e-2", "-1.5e-3", "1e-400",
	"9007199254740991", "9007199254740992", "9007199254740993", "-9007199254740993", "9007199254740993.0",
	"9007199254740993e0", "18446744073709551615", "100000000000000000000000", "123456789012345678901234567890",
	"1" + strings.Repeat("0", 300), "1.7976931348623157e308", "4.9e-324",
	"1e400", "-1e400", "1.7976931348623159e308", "1" + strings.Repeat("0", 400),
}

// diffStrings are JSON strings, escapes and other scripts among them.
var diffStrings = []string{`""`, `"x"`, `"a\"b"`, `"x\\"`, `"1e400"`, `"\u00e9"`, `"\u0000"`, `"日本"`, `"9007199254740993"`, `"\ud800"`}

// diffKeys are JSON keys of properties no schema below names, among them
// a name of one in another case and an escaped one.
var diffKeys = []string{`"a"`, `"b"`, `"A"`, `"x y"`, `""`, `"é"`, `"Count"`, `"PAGE"`, `"Name"`, `"ARGS"`, `"a\u0000b"`, `"\u0061"`}

// diffValue is a random JSON value nested at most depth levels.
func diffValue(r *rand.Rand, depth int) string {
	kind := r.IntN(7)
	if depth == 0 && kind >= 5 {
		kind = r.IntN(5)
	}
	switch kind {
	case 0:
		return "null"
	case 1:
		return []string{"true", "false"}[r.IntN(2)]
	case 2, 3:
		return diffNumbers[r.IntN(len(diffNumbers))]
	case 4:
		return diffStrings[r.IntN(len(diffStrings))]
	case 5:
		items := make([]string, r.IntN(4))
		for i := range items {
			items[i] = diffValue(r, depth-1)
		}
		return "[" + strings.Join(items, ",") + "]"
	}
	return diffObject(r, depth-1, nil)
}

// diffObject is a random JSON object with free keys, a repeated one at
// times, and the members given.
func diffObject(r *rand.Rand, depth int, members []string) string {
	for range r.IntN(4) {
		members = append(members, diffKeys[r.IntN(len(diffKeys))]+":"+diffValue(r, depth))
	}
	r.Shuffle(len(members), func(i, j int) {
		members[i], members[j] = members[j], members[i]
	})
	if len(members) > 0 && r.IntN(4) == 0 {
		members = append(members, members[r.IntN(len(members))])
	}
	space := []string{",", ", ", " ,\n"}[r.IntN(3)]
	return "{" + strings.Join(members, space) + "}"
}

// openSchema has numbers with bounds, nested objects with defaults and no
// limit on other properties.
func openSchema(t *testing.T) *jsonschema.Resolved {
	t.Helper()
	schema := &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"count": {Type: "integer", Minimum: jsonschema.Ptr(1.0)},
			"ratio": {Type: "number", Maximum: jsonschema.Ptr(10.0)},
			"name":  {Type: "string"},
			"page":  {Type: "integer", Default: json.RawMessage("1")},
			"big":   {Default: json.RawMessage("9007199254740993")},
			"list":  {Default: json.RawMessage(`[1, 2.50, 1e3, -0, 9007199254740993]`)},
			"opts": {Type: "object", Properties: map[string]*jsonschema.Schema{
				"limit":  {Type: "integer", Default: json.RawMessage("20")},
				"nested": {Default: json.RawMessage(`{"n": 1.0, "m": [100000000000000000000000]}`)},
			}},
		},
	}
	resolved, err := schema.Resolve(&jsonschema.ResolveOptions{ValidateDefaults: true})
	require.NoError(t, err)
	return resolved
}

// openDocument is a random arguments object for openSchema with at most one
// property the schema names, so at most one breaks it: the validator reports
// the first property it finds broken, in Go's map order.
func openDocument(r *rand.Rand) string {
	var members []string
	switch r.IntN(7) {
	case 0:
		members = append(members, `"count":`+diffValue(r, 0))
	case 1:
		members = append(members, `"ratio":`+diffValue(r, 0))
	case 2:
		members = append(members, `"name":`+diffValue(r, 0))
	case 3:
		members = append(members, `"page":`+diffValue(r, 0))
	case 4:
		var opts []string
		if r.IntN(2) == 0 {
			opts = append(opts, `"limit":`+diffValue(r, 0))
		}
		members = append(members, `"opts":`+diffObject(r, 2, opts))
	case 5:
		members = append(members, `"opts":`+diffValue(r, 2))
	}
	return diffObject(r, 3, members)
}

// callDocument is a random arguments object for call_function's schema
// with at most one fault: name missing or not a string, args not an
// object, or one property more.
func callDocument(r *rand.Rand) string {
	name := `"name":"numbers.send"`
	args := `"args":` + diffObject(r, 3, nil)
	var extra []string
	switch r.IntN(6) {
	case 0:
		name = ""
	case 1:
		name = `"name":` + diffValue(r, 1)
	case 2:
		args = `"args":` + diffValue(r, 2)
	case 3:
		extra = append(extra, diffKeys[r.IntN(len(diffKeys))]+":"+diffValue(r, 1))
	case 4:
		args = ""
	}
	members := extra
	for _, member := range []string{name, args} {
		if member != "" {
			members = append(members, member)
		}
	}
	r.Shuffle(len(members), func(i, j int) {
		members[i], members[j] = members[j], members[i]
	})
	return "{" + strings.Join(members, ",") + "}"
}

// diffTable holds arguments for openSchema that are not a JSON object, are
// not JSON, end in more data or nest deep, besides objects with numbers in
// odd places, a repeated key dropping one among them, and strings that hold
// what looks like a number.
func diffTable() []string {
	nest := func(depth int) string {
		return `{"a":` + strings.Repeat("[", depth) + strings.Repeat("]", depth) + `}`
	}
	return []string{
		"", " ", "\n", "null", " null ", "{}", " {} ", "[]", `"x"`, "5", "true", "1e400",
		"{", "}", "{}{}", "{} {}", "{}x", `{"a":1}}`, `{"a":1,}`, `{"a" 1}`, `{a:1}`, "\ufeff{}", "{\"a\":\"\xff\"}",
		`{"a":01}`, `{"a":.5}`, `{"a":1.}`, `{"a":-}`, `{"a":+1}`, `{"a":NaN}`, `{"a":Infinity}`, `{"a":1e}`,
		`{"a":1e400}`, `{"a":1e400,}`, `{"a":1e400} x`, `{"a":[1e400, 1e401]}`, `{"count":1e400,"ratio":11}`,
		`{"ratio":11,"a":1e400}`, `{"a":{"b":[1,{"c":-1e400}]}}`, `{"a":1e400,"a":1}`, `{"a":"x","a":1e400}`,
		`{"a":[1e400],"a":1}`, `{"a":{"b":-1e400},"a":2}`, `{"a":{"b":1e400,"b":1}}`, `{"a":[{"b":1e400,"b":1}]}`,
		`{"a":` + strings.Repeat("9", 309) + `,"a":1}`, `{"a":` + strings.Repeat("9", 308) + `,"a":1}`,
		`{"a":-` + strings.Repeat("9", 308) + `,"a":1}`, `{"a":0.` + strings.Repeat("0", 400) + `1,"a":1}`,
		`{"a":"1e400"}`, `{"1e400":1}`, `{"a":"\"1e400"}`, `{"a":"x\\","b":1e400}`, `{"a":"x\\\"1e400"}`,
		`{"a":"` + strings.Repeat("9", 400) + `"}`, `{"a":1.7976931348623157e308,"a":1}`, `{"a":1.7976931348623159e308,"a":1}`,
		`{"a":1E400,"a":1}`, `{"a":1e+400,"a":1}`, `{"a":1e-400,"a":1}`, `{"count":1e400,"count":2}`,
		`{"count":9007199254740993}`, `{"count":9007199254740993,"count":2}`, `{"count":2,"count":9007199254740993}`,
		`{"count":0}`, `{"count":1.5}`, `{"count":-0}`, `{"count":"1"}`, `{"Count":1,"count":2}`, `{"ratio":10.0000000000000001}`,
		`{"a":[9007199254740993,[18446744073709551615,{"b":-9007199254740993}]],"page":9007199254740993}`,
		`{"opts":{"limit":9007199254740993,"x":[1e3,1.0,-0]}}`, `{"opts":null}`, `{"opts":[]}`, `{"page":null}`,
		nest(1000), nest(9998), nest(9999), nest(10000), nest(10001), nest(20000),
	}
}

// callTable holds arguments for call_function's schema, each with at most
// one property it does not name: the validator lists those in Go's map
// order.
func callTable() []string {
	return []string{
		"", "null", "{}", "[]", `{"name":"numbers.send"}`, `{"name":"numbers.send","args":{"id":9007199254740993}}`,
		`{"name":"numbers.send","args":{"a":1e400,"a":1}}`, `{"name":"numbers.send","args":{"id":1.0,"ids":[1e3,-0,2.50]}}`,
		`{"name":"numbers.send","args":null}`, `{"name":"numbers.send","args":[]}`, `{"name":"numbers.send","Name":"x"}`,
		`{"name":"numbers.send","name":5}`, `{"name":5,"name":"numbers.send"}`, `{"args":{}}`, `{"name":null}`,
	}
}

// TestApplyInputSchemaMatchesReference: applyInputSchema returns the bytes
// and the error text referenceApplyInputSchema returns, for a table of
// arguments and for thousands of random ones, against a schema with defaults
// and against call_function's own.
func TestApplyInputSchemaMatchesReference(t *testing.T) {
	open := openSchema(t)
	callSchema, err := jsonschema.For[callInput](nil)
	require.NoError(t, err)
	call, err := callSchema.Resolve(&jsonschema.ResolveOptions{ValidateDefaults: true})
	require.NoError(t, err)

	outcomes := map[string]int{}
	check := func(schema string, resolved *jsonschema.Resolved, args string) {
		want, wantInvalid := referenceApplyInputSchema(json.RawMessage(args), resolved)
		got, gotInvalid := applyInputSchema(json.RawMessage(args), resolved)
		if !assert.Equal(t, wantInvalid, gotInvalid, "%s: %.300q", schema, args) {
			return
		}
		assert.Equal(t, string(want), string(got), "%s: %.300q", schema, args)
		switch {
		case wantInvalid == "":
			outcomes["accepted"]++
		case strings.Contains(wantInvalid, "unmarshaling arguments"):
			outcomes["not decoded"]++
		default:
			outcomes["broke the schema"]++
		}
	}

	for _, args := range diffTable() {
		check("open", open, args)
	}
	for _, args := range callTable() {
		check("call", call, args)
	}
	r := rand.New(rand.NewPCG(1, 2))
	for range 3000 {
		check("open", open, openDocument(r))
		check("call", call, callDocument(r))
	}
	t.Log(outcomes)
	for _, outcome := range []string{"accepted", "not decoded", "broke the schema"} {
		assert.Greater(t, outcomes[outcome], 500, outcome)
	}
}
