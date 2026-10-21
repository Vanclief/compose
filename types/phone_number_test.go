package types

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/vanclief/ez"
)

func TestValidatePhoneNumber(t *testing.T) {
	testCases := []struct {
		name  string
		input string
		valid bool
	}{
		{"valid us number", "+15551234567", true},
		{"empty", "", false},
		{"missing plus", "15551234567", false},
		{"leading junk", "junk+15551234567", false},
		{"trailing junk", "+15551234567junk", false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePhoneNumber(tc.input)
			if tc.valid {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Equal(t, ez.EINVALID, ez.ErrorCode(err))
		})
	}
}
