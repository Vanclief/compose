package requests

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewNegotiatesLocale(t *testing.T) {
	header := http.Header{}
	header.Set("Accept-Language", "es-MX,es;q=0.9,en;q=0.8")

	request := New(header, "127.0.0.1")
	require.Equal(t, "es-MX", request.GetLocale())

	request = New(http.Header{}, "127.0.0.1")
	require.Equal(t, "en-US", request.GetLocale())

	request.SetLocale("es-MX")
	require.Equal(t, "es-MX", request.GetLocale())
}
