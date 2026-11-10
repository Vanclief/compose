package validate

import (
	"encoding/json"
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
		{[]string{"18446744073709551616"}, intKind, "18446744073709551616"},
		{[]string{"-9223372036854775809"}, intKind, "-9223372036854775809"},
		{[]string{"99999999999999999999"}, intKind, "99999999999999999999"},
		{[]string{"1.0"}, Kind{Type: "number"}, json.Number("1.0")},
		{[]string{"2.5e3"}, Kind{Type: "number"}, json.Number("2.5e3")},
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
