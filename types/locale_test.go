package types

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewLocaleString(t *testing.T) {
	testCases := []struct {
		name     string
		input    string
		expected Locale
	}{
		{"empty defaults", "", "en-US"},
		{"bare english", "en", "en-US"},
		{"bare spanish", "es", "es-MX"},
		{"uppercase tag", "EN", "en-US"},
		{"explicit region kept", "es-AR", "es-AR"},
		{"accept language list", "es-MX,es;q=0.9,en;q=0.8", "es-MX"},
		{"unsupported first supported later", "fr-FR,es;q=0.5", "es-MX"},
		{"unsupported only", "fr-FR", "en-US"},
		{"garbage", ";;;", "en-US"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			locale, err := NewLocaleString(tc.input)
			require.NoError(t, err)
			require.Equal(t, tc.expected, locale)
		})
	}
}
