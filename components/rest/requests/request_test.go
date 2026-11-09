package requests

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
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

// New takes the ID SetRequestID stored in the header it is given, as
// rest.Configure does for every request.
func TestNewTakesStoredID(t *testing.T) {
	header := http.Header{}
	SetRequestID(header, "req-1")

	request := New(header, "127.0.0.1")
	defer request.Cancel()

	require.Equal(t, "req-1", request.GetID())
	require.Equal(t, "req-1", RequestID(request.GetContext()))

	// Without the header each request has an ID of its own
	other := New(http.Header{}, "127.0.0.1")
	defer other.Cancel()
	another := New(nil, "127.0.0.1")
	defer another.Cancel()

	require.NotEmpty(t, other.GetID())
	require.NotEmpty(t, another.GetID())
	require.NotEqual(t, other.GetID(), another.GetID())
}

// No header a client sends sets the ID, in any spelling, over HTTP/1 or
// HTTP/2: net/http stores the names it receives in canonical form, never
// under the lower-case key SetRequestID writes.
func TestNewIgnoresClientHeaders(t *testing.T) {
	names := []string{"compose-request-id", "Compose-Request-Id", "COMPOSE-REQUEST-ID", "X-Request-Id", "x-request-id"}
	serve := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := New(r.Header, "127.0.0.1")
		defer request.Cancel()
		_, _ = io.WriteString(w, request.GetID())
	})
	requireOwnID := func(t *testing.T, res *http.Response) {
		t.Helper()
		body, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, res.StatusCode, string(body))
		require.NoError(t, uuid.Validate(string(body)), string(body))
	}

	t.Run("HTTP/1", func(t *testing.T) {
		server := httptest.NewServer(serve)
		defer server.Close()

		// Written by hand, so the names reach the server exactly as spelled
		conn, err := net.Dial("tcp", server.Listener.Addr().String())
		require.NoError(t, err)
		defer conn.Close()
		raw := "GET / HTTP/1.1\r\nHost: example.com\r\nConnection: close\r\n"
		for _, name := range names {
			raw += name + ": forged\r\n"
		}
		_, err = io.WriteString(conn, raw+"\r\n")
		require.NoError(t, err)
		res, err := http.ReadResponse(bufio.NewReader(conn), nil)
		require.NoError(t, err)
		defer res.Body.Close()
		requireOwnID(t, res)
	})

	t.Run("HTTP/2", func(t *testing.T) {
		server := httptest.NewUnstartedServer(serve)
		server.EnableHTTP2 = true
		server.StartTLS()
		defer server.Close()

		req, err := http.NewRequest(http.MethodGet, server.URL, nil)
		require.NoError(t, err)
		for _, name := range names {
			req.Header[name] = []string{"forged"}
		}
		res, err := server.Client().Do(req)
		require.NoError(t, err)
		defer res.Body.Close()
		require.Equal(t, 2, res.ProtoMajor)
		requireOwnID(t, res)
	})
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

func TestAccept(t *testing.T) {
	request := New(http.Header{}, "127.0.0.1")
	require.Equal(t, "", request.GetAccept())

	request.SetAccept("text/csv")
	require.Equal(t, "text/csv", request.GetAccept())
}

type parentKey struct{}

func TestNewWithContextStoresRequestID(t *testing.T) {
	request := NewWithContext(context.Background(), http.Header{}, "127.0.0.1")
	defer request.Cancel()

	require.NotEmpty(t, request.ID)
	require.Equal(t, request.ID, RequestID(request.GetContext()))
}

func TestNewWithContextSeesParentValues(t *testing.T) {
	parent := context.WithValue(context.Background(), parentKey{}, "value")

	request := NewWithContext(parent, http.Header{}, "127.0.0.1", WithTimeout(time.Minute))
	defer request.Cancel()

	require.Equal(t, "value", request.GetContext().Value(parentKey{}))
	require.Equal(t, request.ID, RequestID(request.GetContext()))
}

func TestNewWithContextCancelledWithParent(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())

	request := NewWithContext(parent, http.Header{}, "127.0.0.1", WithTimeout(time.Minute))
	defer request.Cancel()

	require.NoError(t, request.GetContext().Err())

	cancelParent()
	require.ErrorIs(t, request.GetContext().Err(), context.Canceled)
}

func TestNewWithContextDefaultTimeout(t *testing.T) {
	start := time.Now()

	request := NewWithContext(context.Background(), http.Header{}, "127.0.0.1")
	defer request.Cancel()

	deadline, ok := request.GetContext().Deadline()
	require.True(t, ok)
	require.WithinDuration(t, start.Add(DEFAULT_TIMEOUT), deadline, time.Second)
}

func TestNewWithContextWithTimeoutExceedsDefault(t *testing.T) {
	start := time.Now()

	request := NewWithContext(context.Background(), http.Header{}, "127.0.0.1", WithTimeout(60*time.Second))
	defer request.Cancel()

	deadline, ok := request.GetContext().Deadline()
	require.True(t, ok)
	require.WithinDuration(t, start.Add(60*time.Second), deadline, time.Second)
	require.Equal(t, request.ID, RequestID(request.GetContext()))
}

func TestNewWithContextParentDeadlineWins(t *testing.T) {
	start := time.Now()

	parent, cancelParent := context.WithTimeout(context.Background(), time.Second)
	defer cancelParent()

	request := NewWithContext(parent, http.Header{}, "127.0.0.1", WithTimeout(60*time.Second))
	defer request.Cancel()

	deadline, ok := request.GetContext().Deadline()
	require.True(t, ok)
	require.WithinDuration(t, start.Add(time.Second), deadline, 500*time.Millisecond)
}

func TestNewWithContextCancelKeepsParent(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()

	request := NewWithContext(parent, http.Header{}, "127.0.0.1", WithTimeout(time.Minute))

	request.Cancel()
	require.ErrorIs(t, request.GetContext().Err(), context.Canceled)
	require.NoError(t, parent.Err())
}

func TestNewWithContextWithTimeoutReleasesDefaultTimeout(t *testing.T) {
	var defaultContext context.Context

	captureDefault := func(req *StandardRequest) {
		defaultContext = req.Context
	}

	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()

	request := NewWithContext(parent, http.Header{}, "127.0.0.1", captureDefault, WithTimeout(time.Minute))
	defer request.Cancel()

	require.ErrorIs(t, defaultContext.Err(), context.Canceled)
	require.NoError(t, request.GetContext().Err())
	require.NoError(t, parent.Err())
}
