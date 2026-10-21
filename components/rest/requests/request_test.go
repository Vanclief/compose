package requests

import (
	"context"
	"net/http"
	"testing"
	"time"

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

func TestRequestID(t *testing.T) {
	request := New(http.Header{}, "127.0.0.1")
	defer request.Cancel()

	require.NotEmpty(t, request.GetID())
	require.Equal(t, request.GetID(), RequestID(request.GetContext()))

	require.Empty(t, RequestID(context.Background()))

	// A plain string key with the old name must not be read as the request ID
	//lint:ignore SA1029 the test checks that a string key does not collide
	ctx := context.WithValue(context.Background(), "request-id", "other")
	require.Empty(t, RequestID(ctx))
}

func TestWithTimeoutReleasesDefaultTimeout(t *testing.T) {
	var defaultContext context.Context

	captureDefault := func(req *StandardRequest) {
		defaultContext = req.Context
	}

	request := New(http.Header{}, "127.0.0.1", captureDefault, WithTimeout(time.Minute))
	defer request.Cancel()

	require.ErrorIs(t, defaultContext.Err(), context.Canceled)

	require.NoError(t, request.GetContext().Err())
	require.Equal(t, request.GetID(), RequestID(request.GetContext()))

	deadline, ok := request.GetContext().Deadline()
	require.True(t, ok)
	require.Greater(t, time.Until(deadline), DEFAULT_TIMEOUT)

	request.Cancel()
	require.ErrorIs(t, request.GetContext().Err(), context.Canceled)
}
