package validate

import (
	"encoding/json"
	"math/big"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/vanclief/ez"
)

func TestCanonicalNumber(t *testing.T) {
	ones := strings.Repeat("1", 64)
	cases := []struct {
		in, want string
	}{
		// Short and without an exponent: as written.
		{"0", "0"},
		{"-1.50", "-1.50"},
		{"1" + strings.Repeat("0", 63), "1" + strings.Repeat("0", 63)},
		// The exact value, as digits and an exponent.
		{"1e3", "1e3"},
		{"-2.50E+3", "-25e2"},
		{"0.000e7", "0"},
		{"-0.0", "-0.0"},
		{"1.0000000000000000001e0", "10000000000000000001e-19"},
		{"1" + strings.Repeat("0", 64), "1e64"},
		{"0." + strings.Repeat("9", 64), strings.Repeat("9", 64) + "e-64"},
		{"1.7976931348623157e308", "17976931348623157e292"},
		{"9.99e308", "999e306"},
		{"5e-324", "5e-324"},
		{"1e-324", "1e-324"},
		// More significant digits: the first 64 and a 5.
		{ones + "1", ones + "5"},
		{"1" + strings.Repeat("0", 63) + "1", "1" + strings.Repeat("0", 63) + "5"},
		{ones + "1e10", ones + "5e10"},
		{"-1." + strings.Repeat("0", 64) + "1", "-1" + strings.Repeat("0", 63) + "5e-64"},
		{"0." + strings.Repeat("9", 70), strings.Repeat("9", 64) + "5e-65"},
		// Not an integer, though its first 64 digits and a 5 would be one.
		{ones + "11.5", ones + "5" + "0" + "5e-1"},
		{ones + "1.5", ones + "55e-1"},
		// Past the float64 range, or closer to 0 than any float64.
		{"1e309", "1e309"},
		{"-1e999999", "-1e309"},
		{"1e99999999999999999999999999", "1e309"},
		{"1" + strings.Repeat("0", 400) + ".5", "1" + strings.Repeat("0", 309) + ".5"},
		{"1e-325", "1e-325"},
		{"-1e-999999", "-1e-325"},
		{"1e-99999999999999999999999999", "1e-325"},
	}
	for _, c := range cases {
		require.Equal(t, c.want, string(CanonicalNumber(c.in)), c.in)
	}
}

// The text CanonicalNumber gives a number compares with every number of up
// to 64 significant digits as the number does, and is an integer when the
// number is: checked against the numbers next to it, which are the ones a
// rounded value gets wrong.
func TestCanonicalNumberKeepsComparisons(t *testing.T) {
	random := rand.New(rand.NewPCG(1, 2))
	for range 3000 {
		// The number is sign digits × 10^scale.
		var b strings.Builder
		b.WriteByte('1' + byte(random.IntN(9)))
		for range random.IntN(100) {
			b.WriteByte('0' + byte(random.IntN(10)))
		}
		digits := b.String()
		scale := random.IntN(240) - 150
		sign := ""
		if random.IntN(2) == 0 {
			sign = "-"
		}

		// Written with the point after its first point digits, and an
		// exponent unless that is 0 and a coin says to leave it out.
		point := 1 + random.IntN(len(digits))
		in := sign + digits[:point]
		if point < len(digits) {
			in += "." + digits[point:]
		}
		exponent := scale + len(digits) - point
		if exponent != 0 || random.IntN(2) == 0 {
			in += "e" + strconv.Itoa(exponent)
		}

		exact, ok := new(big.Rat).SetString(sign + digits + "e" + strconv.Itoa(scale))
		require.True(t, ok, in)
		got, ok := new(big.Rat).SetString(string(CanonicalNumber(in)))
		require.True(t, ok, in)
		require.Equal(t, exact.IsInt(), got.IsInt(), in)

		// The number cut to k significant digits and the next number of k
		// digits, either side of it; the nearest float64 in its shortest
		// text.
		f, _ := exact.Float64()
		near := []string{strconv.FormatFloat(f, 'g', -1, 64)}
		for k := 1; k <= min(64, len(digits)); k++ {
			cut, ok := new(big.Int).SetString(digits[:k], 10)
			require.True(t, ok)
			at := "e" + strconv.Itoa(scale+len(digits)-k)
			near = append(near, sign+cut.String()+at, sign+cut.Add(cut, big.NewInt(1)).String()+at)
		}
		for _, bound := range near {
			b, ok := new(big.Rat).SetString(bound)
			require.True(t, ok, bound)
			require.Equal(t, exact.Cmp(b), got.Cmp(b), "%s against %s", in, bound)
		}
	}
}

// A body of one long number or one long exponent costs time and memory in
// proportion to its length.
func TestCanonicalNumberCost(t *testing.T) {
	long := "1" + strings.Repeat("7", 1<<20)
	exponent := "1e" + strings.Repeat("9", 1<<20)
	for _, in := range []string{long, exponent, "-" + long + ".5", "0." + long} {
		allocs := testing.AllocsPerRun(5, func() {
			CanonicalNumber(in)
		})
		require.Less(t, allocs, 10.0)
	}
	require.Equal(t, "1e309", string(CanonicalNumber(long)))
	require.Equal(t, "1"+strings.Repeat("7", 63)+"5e-65", string(CanonicalNumber("0."+long)))
}

// multipleOf gives on CanonicalNumber's text the answer it gives on the
// number when the number has at most 64 significant digits, trailing zeros
// not counted, and is under 10^309 in magnitude, or is under 10^-324.
func TestMultipleOfChecks(t *testing.T) {
	ones := strings.Repeat("1", 64)
	cases := []struct {
		in    string
		check bool
	}{
		{"0", true},
		{"-1.50", true},
		{ones, true},
		{ones + strings.Repeat("0", 9), true},
		{ones + "e200", true},
		{"0." + strings.Repeat("0", 300) + ones, true},
		{"9.99e308", true},
		{"1e-400", true},
		{"-0." + strings.Repeat("0", 400) + ones + "1", true},
		{ones + "1", false},
		{ones + "1" + strings.Repeat("0", 9), false},
		{"1." + strings.Repeat("0", 63) + "1", false},
		{ones + "11.5", false},
		{"1e309", false},
		{"-1e400", false},
		{"1" + strings.Repeat("0", 400) + ".5", false},
	}
	for _, c := range cases {
		require.Equal(t, c.check, multipleOfChecks(CanonicalNumber(c.in)), c.in)
	}
}

// requestDoc holds numbers with multipleOf, one without, and an anyOf of
// numbers with multipleOf.
const requestDoc = `{
  "openapi": "3.1.0",
  "info": {"title": "numbers", "version": "1.0.0"},
  "paths": {},
  "components": {
    "schemas": {
      "Amounts": {
        "type": "object",
        "properties": {
          "step": {"type": "number", "multipleOf": 1000000000},
          "free": {"type": "number"},
          "one": {"type": "number", "multipleOf": 1},
          "half": {"type": "number", "multipleOf": 0.5},
          "either": {"anyOf": [
            {"type": "array", "items": {"type": "number", "multipleOf": 1000000000}},
            {"type": "null"}
          ]}
        }
      }
    }
  }
}`

// A request compiler refuses, where a schema has multipleOf, a number whose
// canonical text does not hold all its digits, with one message, also when
// santhosh's multipleOf failed on that text. A number it can check, and a
// number under a schema without multipleOf, are validated as before, and
// NewCompiler, which a response contract uses on the numbers as written,
// checks multipleOf on them exactly.
func TestRequestCompilerRefusesUncheckableMultiples(t *testing.T) {
	compileWith := func(newCompiler func(json.RawMessage) (*Compiler, error)) *Schema {
		compiler, err := newCompiler(json.RawMessage(requestDoc))
		require.NoError(t, err)
		schema, err := compiler.Compile("op:amounts", map[string]any{
			"type":       "object",
			"properties": map[string]any{"body": map[string]any{"$ref": "spec.json#/components/schemas/Amounts"}},
		})
		require.NoError(t, err)
		return schema
	}
	request := compileWith(NewRequestCompiler)
	validated := func(schema *Schema, name, number string) error {
		return schema.Validate(map[string]any{"body": map[string]any{name: CanonicalNumber(number)}})
	}

	digits64 := "1234567890123456789012345678901234567890123456789012345678901234"
	multiple73 := digits64 + "000000000"
	notMultiple73 := "1234567890123456789012345678901234567890123456789012345678901234567890123"
	multiple83 := "1" + notMultiple73 + "000000000"
	require.Len(t, multiple73, 73)
	require.Len(t, notMultiple73, 73)
	require.Len(t, multiple83, 83)

	require.NoError(t, validated(request, "step", multiple73))
	require.NoError(t, validated(request, "step", "1e300"))
	err := validated(request, "step", digits64)
	require.Equal(t, "step must be a multiple of 1000000000", ez.ErrorMessage(err))
	err = validated(request, "step", "1.5")
	require.Equal(t, "step must be a multiple of 1000000000", ez.ErrorMessage(err))

	// The canonical text of notMultiple73 ends in 5e8 and that of 5 followed
	// by 64 ones in 5, no multiples, which santhosh's multipleOf refuses; that
	// of multiple83 ends in 5e18, a multiple, which it passes: one message
	// either way.
	for _, number := range []string{notMultiple73, multiple83, "5" + strings.Repeat("1", 64), "1e309", "-1e400"} {
		err = validated(request, "step", number)
		require.Equal(t, ez.EINVALID, ez.ErrorCode(err), number)
		require.Equal(t, "step has too many digits to be checked as a multiple of 1000000000", ez.ErrorMessage(err), number)
		require.Equal(t, []FieldError{{In: "body", Name: "step", Message: "has too many digits to be checked as a multiple of 1000000000"}}, Fields(err))

		require.NoError(t, validated(request, "free", number), number)
	}

	plain := compileWith(NewCompiler)
	require.NoError(t, plain.Validate(map[string]any{"body": map[string]any{"step": json.Number(multiple83)}}))
	err = plain.Validate(map[string]any{"body": map[string]any{"step": json.Number(notMultiple73)}})
	require.Equal(t, "step must be a multiple of 1000000000", ez.ErrorMessage(err))
}

// requestAmounts compiles the Amounts schema of requestDoc with
// NewRequestCompiler and returns a function that validates a body.
func requestAmounts(t *testing.T) func(body map[string]any) error {
	t.Helper()
	compiler, err := NewRequestCompiler(json.RawMessage(requestDoc))
	require.NoError(t, err)
	schema, err := compiler.Compile("op:amounts", map[string]any{
		"type":       "object",
		"properties": map[string]any{"body": map[string]any{"$ref": "spec.json#/components/schemas/Amounts"}},
	})
	require.NoError(t, err)
	return func(body map[string]any) error {
		return schema.Validate(map[string]any{"body": body})
	}
}

// Every integer is a multiple of 1 and of 1/k, and CanonicalNumber's text is
// an integer exactly when the number is: under multipleOf 1 a number of any
// length is checked on that text, and under 0.5 an integer is, so neither is
// refused as too long. A long number that is no integer is still refused
// under 0.5, and every long number under 10^9.
func TestRequestCompilerChecksUnitFractions(t *testing.T) {
	validated := requestAmounts(t)
	integer70 := strings.Repeat("7", 70)
	fraction71 := strings.Repeat("7", 69) + "2.5"

	for _, number := range []string{integer70, "1e400", "-1e400"} {
		require.NoError(t, validated(map[string]any{"one": CanonicalNumber(number)}), number)
		require.NoError(t, validated(map[string]any{"half": CanonicalNumber(number)}), number)
	}
	err := validated(map[string]any{"one": CanonicalNumber(fraction71)})
	require.Equal(t, "one must be a multiple of 1", ez.ErrorMessage(err))
	err = validated(map[string]any{"half": CanonicalNumber(fraction71)})
	require.Equal(t, "half has too many digits to be checked as a multiple of 0.5", ez.ErrorMessage(err))
	err = validated(map[string]any{"step": CanonicalNumber(integer70)})
	require.Equal(t, "step has too many digits to be checked as a multiple of 1000000000", ez.ErrorMessage(err))
}

// A multipleOf failure is dropped only beside a too-many-digits failure at
// its own location: the failure of another item stays, inside an anyOf too.
func TestTooManyDigitsKeepsOtherMultipleOfFailures(t *testing.T) {
	validated := requestAmounts(t)
	long := "1" + strings.Repeat("2", 64) + "000000001"

	err := validated(map[string]any{"either": []any{CanonicalNumber(long), json.Number("1")}})
	require.Equal(t, []FieldError{
		{In: "body", Name: "either.0", Message: "has too many digits to be checked as a multiple of 1000000000"},
		{In: "body", Name: "either.1", Message: "must be a multiple of 1000000000"},
	}, Fields(err))
}

// An array of many numbers too long to be checked and many short ones that
// are no multiple costs time in proportion to its length: it takes no more
// than ten times as long as an array of as many short numbers, where the
// drop of the multipleOf failures beside a too-many-digits one once took
// time in proportion to the product of the two counts, more than thirty
// times as long at these counts, with the race detector too. Both are timed
// in the same run, so the race detector and a busy machine slow them alike.
func TestTooManyDigitsCostIsLinear(t *testing.T) {
	validated := requestAmounts(t)
	long := CanonicalNumber("1" + strings.Repeat("2", 64) + "000000001")
	short := json.Number("1")
	const longs = 10000
	const shorts = 50000

	// One long number, then five short ones, and so on.
	mixed := make([]any, 0, longs+shorts)
	for i := range longs + shorts {
		if i%(shorts/longs+1) == 0 {
			mixed = append(mixed, long)
			continue
		}
		mixed = append(mixed, short)
	}
	plain := make([]any, 0, longs+shorts)
	for range longs + shorts {
		plain = append(plain, short)
	}

	start := time.Now()
	err := validated(map[string]any{"either": plain})
	plainTime := time.Since(start)
	require.Len(t, Fields(err), MaxFields)

	start = time.Now()
	err = validated(map[string]any{"either": mixed})
	mixedTime := time.Since(start)
	fields := Fields(err)
	require.Len(t, fields, MaxFields)
	require.Equal(t, FieldError{In: "body", Name: "either.0", Message: "has too many digits to be checked as a multiple of 1000000000"}, fields[0])
	require.Equal(t, FieldError{In: "body", Name: "either.1", Message: "must be a multiple of 1000000000"}, fields[1])

	require.Less(t, mixedTime, 10*plainTime, "mixed %v, plain %v", mixedTime, plainTime)
}
