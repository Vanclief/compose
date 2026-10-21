package types

import (
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
