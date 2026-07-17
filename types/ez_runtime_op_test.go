package types

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/vanclief/ez"
)

func TestEzNewDerivesOperationFromFunction(t *testing.T) {
	_, err := UnixSecondsFromRFC3339("invalid")
	require.Error(t, err)

	var ezErr *ez.Error
	require.ErrorAs(t, err, &ezErr)
	require.Equal(t, "types.UnixSecondsFromRFC3339", ezErr.Op)
}

func TestEzWrapDerivesOperationFromMethod(t *testing.T) {
	var locale Locale
	err := locale.UnmarshalJSON([]byte(`{}`))
	require.Error(t, err)

	var ezErr *ez.Error
	require.ErrorAs(t, err, &ezErr)
	require.Equal(t, "types.Locale.UnmarshalJSON", ezErr.Op)
}
