package validate

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/vanclief/ez"
)

func TestCoerce(t *testing.T) {
	intKind := Kind{Type: "integer"}
	cases := []struct {
		values []string
		kind   Kind
		want   any
	}{
		{nil, intKind, nil},
		{[]string{"42"}, intKind, json.Number("42")},
		{[]string{"-7"}, intKind, json.Number("-7")},
		{[]string{"007"}, intKind, json.Number("7")},
		{[]string{"+5"}, intKind, json.Number("5")},
		{[]string{"-0"}, intKind, json.Number("0")},
		{[]string{"1.0"}, intKind, "1.0"},
		{[]string{"abc"}, intKind, "abc"},
		{[]string{"9223372036854775807"}, intKind, json.Number("9223372036854775807")},
		{[]string{"9223372036854775808"}, intKind, json.Number("9223372036854775808")},
		{[]string{"18446744073709551615"}, intKind, json.Number("18446744073709551615")},
		{[]string{"018446744073709551615"}, intKind, json.Number("18446744073709551615")},
		// Past both ranges, for a big.Int, in canonical text up to 64
		// characters.
		{[]string{"18446744073709551616"}, intKind, json.Number("18446744073709551616")},
		{[]string{"-9223372036854775809"}, intKind, json.Number("-9223372036854775809")},
		{[]string{"99999999999999999999"}, intKind, json.Number("99999999999999999999")},
		{[]string{"+00123456789012345678901234567890"}, intKind, json.Number("123456789012345678901234567890")},
		{[]string{"+18446744073709551615"}, intKind, json.Number("18446744073709551615")},
		{[]string{"-" + strings.Repeat("9", 63)}, intKind, json.Number("-" + strings.Repeat("9", 63))},
		{[]string{strings.Repeat("9", 65)}, intKind, strings.Repeat("9", 65)},
		{[]string{"1_000000000000000000000"}, intKind, "1_000000000000000000000"},
		{[]string{"0x1000000000000000000000"}, intKind, "0x1000000000000000000000"},
		{[]string{"--100000000000000000000"}, intKind, "--100000000000000000000"},
		{[]string{"100000000000000000000.0"}, intKind, "100000000000000000000.0"},
		{[]string{"1e30"}, intKind, "1e30"},
		{[]string{"1.0"}, Kind{Type: "number"}, json.Number("1.0")},
		{[]string{"2.5e3"}, Kind{Type: "number"}, json.Number("2500")},
		{[]string{"1.50"}, Kind{Type: "number"}, json.Number("1.50")},
		{[]string{"-0"}, Kind{Type: "number"}, json.Number("-0")},
		{[]string{"0.010000000000000001"}, Kind{Type: "number"}, json.Number("0.010000000000000001")},
		{[]string{"1.0000000000000000001"}, Kind{Type: "number"}, json.Number("1.0000000000000000001")},
		{[]string{"0." + strings.Repeat("0", 61) + "1"}, Kind{Type: "number"}, json.Number("0." + strings.Repeat("0", 61) + "1")},
		{[]string{"0." + strings.Repeat("0", 62) + "1"}, Kind{Type: "number"}, json.Number("1e-63")},
		{[]string{"1E2"}, Kind{Type: "number"}, json.Number("100")},
		{[]string{"007"}, Kind{Type: "number"}, "007"},
		{[]string{" 1.5"}, Kind{Type: "number"}, " 1.5"},
		{[]string{"1e5"}, Kind{Type: "number"}, json.Number("100000")},
		{[]string{"1e21"}, Kind{Type: "number"}, json.Number("1e+21")},
		{[]string{"1e-999999"}, Kind{Type: "number"}, json.Number("0")},
		{[]string{"1e999999"}, Kind{Type: "number"}, "1e999999"},
		{[]string{"+1.5"}, Kind{Type: "number"}, "+1.5"},
		{[]string{"NaN"}, Kind{Type: "number"}, "NaN"},
		{[]string{"0x10"}, Kind{Type: "number"}, "0x10"},
		{[]string{"true"}, Kind{Type: "boolean"}, true},
		{[]string{"false"}, Kind{Type: "boolean"}, false},
		{[]string{"True"}, Kind{Type: "boolean"}, "True"},
		{[]string{"x", "y"}, Kind{Type: "string"}, "x"},
		{[]string{"x"}, Kind{Type: "weird"}, "x"},
		{[]string{"1", "2"}, Kind{Type: "array", Items: &intKind}, []any{json.Number("1"), json.Number("2")}},
		{[]string{"1,x,3"}, Kind{Type: "array", Items: &intKind}, []any{json.Number("1"), "x", json.Number("3")}},
		{[]string{"a,b"}, Kind{Type: "array"}, []any{"a", "b"}},
	}
	for _, c := range cases {
		require.Equal(t, c.want, Coerce(c.values, c.kind), "%v %v", c.values, c.kind.Type)
	}
}

func TestRejectRepeatedKeys(t *testing.T) {
	for _, raw := range []string{
		`{}`, `[]`, `1`, `"a"`, `null`, `{"a":{},"b":[]}`, `[{"a":1},{"a":1}]`,
		`{"a":{"a":{"a":1}}}`, `{"a":[{"b":1}],"b":{"a":[]}}`, `{"a":1e999999}`,
	} {
		require.NoError(t, RejectRepeatedKeys([]byte(raw)), raw)
	}

	for _, raw := range []string{
		`{"a":1,"a":1}`, `{"a":{},"b":2,"a":[]}`, `[{"x":[{"y":1,"y":2}]}]`, `{"a":1,"a":2}`,
	} {
		err := RejectRepeatedKeys([]byte(raw))
		require.Error(t, err, raw)
		require.Equal(t, ez.EINVALID, ez.ErrorCode(err))
		require.Equal(t, "body repeats an object key", ez.ErrorMessage(err))
	}
}

func TestDecodeBytes(t *testing.T) {
	v, err := DecodeBytes([]byte(""))
	require.NoError(t, err)
	require.Nil(t, v)

	v, err = DecodeBytes([]byte("  \n"))
	require.NoError(t, err)
	require.Nil(t, v)

	v, err = DecodeBytes([]byte(`{"id":12345678901234567890,"f":1.5}`))
	require.NoError(t, err)
	require.Equal(t, map[string]any{"id": json.Number("12345678901234567890"), "f": json.Number("1.5")}, v)

	for _, raw := range []string{`{"a":1}{"b":2}`, `{"a":1} garbage`, `{"a":` + sentinel, sentinel} {
		_, err = DecodeBytes([]byte(raw))
		require.Error(t, err, raw)
		require.Equal(t, ez.EINVALID, ez.ErrorCode(err))
		require.Equal(t, "body is not valid JSON", ez.ErrorMessage(err))
	}
}
