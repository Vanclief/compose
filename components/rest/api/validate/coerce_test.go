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
		{[]string{"1.0"}, intKind, "1.0"},
		{[]string{"abc"}, intKind, "abc"},
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

func TestDecodeBody(t *testing.T) {
	v, err := DecodeBody(strings.NewReader(""))
	require.NoError(t, err)
	require.Nil(t, v)

	v, err = DecodeBody(strings.NewReader("  \n"))
	require.NoError(t, err)
	require.Nil(t, v)

	v, err = DecodeBody(strings.NewReader(`{"id":12345678901234567890,"f":1.5}`))
	require.NoError(t, err)
	require.Equal(t, map[string]any{"id": json.Number("12345678901234567890"), "f": json.Number("1.5")}, v)

	for _, raw := range []string{`{"a":1}{"b":2}`, `{"a":1} garbage`, `{"a":` + sentinel, sentinel} {
		_, err = DecodeBody(strings.NewReader(raw))
		require.Error(t, err, raw)
		require.Equal(t, ez.EINVALID, ez.ErrorCode(err))
		require.Equal(t, "body is not valid JSON", ez.ErrorMessage(err))
	}
}
