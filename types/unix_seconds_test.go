package types

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/vanclief/ez"
)

func TestUnixSecondsScan(t *testing.T) {
	testCases := []struct {
		name     string
		input    any
		expected UnixSeconds
	}{
		{"int64", int64(1700000000), 1700000000},
		{"int32", int32(42), 42},
		{"string", "12", 12},
		{"bytes", []byte("12"), 12},
		{"negative string", "-5", -5},
		{"nil", nil, 0},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			us := UnixSeconds(99)
			err := us.Scan(tc.input)
			require.NoError(t, err)
			require.Equal(t, tc.expected, us)
		})
	}
}

func TestUnixSecondsScanRejectsMalformed(t *testing.T) {
	testCases := []struct {
		name  string
		input any
	}{
		{"trailing junk string", "12abc"},
		{"fractional string", "1.5"},
		{"trailing junk bytes", []byte("12abc")},
		{"fractional bytes", []byte("1.5")},
		{"empty string", ""},
		{"leading space", " 12"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			us := UnixSeconds(99)
			err := us.Scan(tc.input)
			require.Error(t, err)
			require.Equal(t, ez.EINVALID, ez.ErrorCode(err))
			require.Equal(t, UnixSeconds(99), us)
		})
	}
}

func TestUnixSecondsScanUnsupportedType(t *testing.T) {
	us := UnixSeconds(99)
	err := us.Scan(1.5)
	require.Error(t, err)
	require.Equal(t, UnixSeconds(99), us)
}

func TestUnixSecondsUnmarshalJSON(t *testing.T) {
	var us UnixSeconds
	err := json.Unmarshal([]byte(`1700000000`), &us)
	require.NoError(t, err)
	require.Equal(t, UnixSeconds(1700000000), us)

	// null sets 0.
	us = UnixSeconds(99)
	err = us.UnmarshalJSON([]byte(`null`))
	require.NoError(t, err)
	require.Equal(t, UnixSeconds(0), us)

	// Whole numbers with an exponent or a fraction part are refused too.
	testCases := []struct {
		name  string
		input string
	}{
		{"string", `"1700000000"`},
		{"boolean", `true`},
		{"exponent", `1e3`},
		{"zero fraction", `1.0`},
		{"exponent past int64", `1e30`},
		{"digits past int64", `99999999999999999999`},
		{"negative past int64", `-9223372036854775809`},
		{"fraction", `1.5`},
		{"not JSON", `-`},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			us := UnixSeconds(99)
			err := us.UnmarshalJSON([]byte(tc.input))
			require.Equal(t, ez.EINVALID, ez.ErrorCode(err))
			require.Equal(t, "value must be unix seconds written as a plain integer that fits in 64 bits", ez.ErrorMessage(err))
			require.Equal(t, UnixSeconds(99), us)
		})
	}
}
