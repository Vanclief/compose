package types

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/vanclief/ez"
)

// A JSON value that is not a string is the client's mistake: EINVALID, also
// under Optional, which passes T's ez error up.
func TestLocaleUnmarshalNotString(t *testing.T) {
	l := Locale("es-MX")
	err := json.Unmarshal([]byte(`5`), &l)
	require.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	require.Equal(t, "locale must be a JSON string", ez.ErrorMessage(err))
	require.Equal(t, Locale("es-MX"), l)

	var o Optional[Locale]
	err = json.Unmarshal([]byte(`5`), &o)
	require.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	require.Equal(t, "locale must be a JSON string", ez.ErrorMessage(err))
	require.False(t, o.Set)

	err = json.Unmarshal([]byte(`"es"`), &o)
	require.NoError(t, err)
	require.Equal(t, Some(Locale("es-MX")), o)
}

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
