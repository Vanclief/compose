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
		{"spain", "+34612345678", true},
		{"france", "+33612345678", true},
		{"netherlands", "+31612345678", true},
		{"australia", "+61412345678", true},
		{"singapore", "+6581234567", true},
		{"switzerland", "+41781234567", true},
		{"belgium", "+3225123456", true},
		{"united arab emirates", "+971501234567", true},
		{"mexico", "+525512345678", true},
		{"mexico with mobile prefix", "+5215512345678", true},
		{"united states", "+12025550123", true},
		{"united kingdom", "+447911123456", true},
		{"four national digits", "+341234", true},
		{"three national digits", "+34123", false},
		{"fifteen digits", "+123456789012345", true},
		{"sixteen digits", "+1234567890123456", false},
		{"three digit country code and fourteen digits", "+97112345678901234", false},
		{"country code starting with zero", "+0123456789", false},
		{"inner space", "+34 612345678", false},
		{"inner dash", "+34-612345678", false},
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
