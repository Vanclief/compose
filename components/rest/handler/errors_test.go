package handler

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/labstack/echo/v5"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"
	"github.com/vanclief/compose/components/rest/requests"
	"github.com/vanclief/ez"
)

// recordingTransport keeps every event Sentry would have sent.
type recordingTransport struct{ events []*sentry.Event }

func (t *recordingTransport) Configure(sentry.ClientOptions)        {}
func (t *recordingTransport) SendEvent(e *sentry.Event)             { t.events = append(t.events, e) }
func (t *recordingTransport) Flush(time.Duration) bool              { return true }
func (t *recordingTransport) FlushWithContext(context.Context) bool { return true }
func (t *recordingTransport) Close()                                {}

func TestManageErrorSentryGate(t *testing.T) {
	tests := []struct {
		code     string
		reported bool
		level    sentry.Level
	}{
		{ez.EINTERNAL, true, sentry.LevelError},
		{ez.ETIMEOUT, true, sentry.LevelWarning},
		{ez.EUNAVAILABLE, true, sentry.LevelWarning},
		{ez.EINVALID, false, ""},
		{ez.ENOTFOUND, false, ""},
	}

	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			transport := &recordingTransport{}
			client, err := sentry.NewClient(sentry.ClientOptions{Transport: transport})
			require.NoError(t, err)
			hub := sentry.NewHub(client, sentry.NewScope())

			c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), httptest.NewRecorder())
			c.Set("sentry", hub) // same key sentryecho middleware uses

			request := requests.New(http.Header{}, "127.0.0.1")
			err = NewHandler(nil).ManageError(c, "test", request, ez.New(tt.code, "boom", nil))
			require.NoError(t, err)

			if !tt.reported {
				require.Empty(t, transport.events)
				return
			}
			require.Len(t, transport.events, 1)
			require.Equal(t, tt.level, transport.events[0].Level)
		})
	}
}

// ManageError reports every error it answers as a 500, 503 or 504, even an
// Echo error that ReportToSentry skips for its Echo status.
func TestManageErrorSentryRawErrors(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status int
		level  sentry.Level
	}{
		{"echo sentinel", echo.ErrNotFound, http.StatusInternalServerError, sentry.LevelError},
		{"echo wrapping internal", echo.ErrBadRequest.Wrap(ez.New(ez.EINTERNAL, "boom", nil)), http.StatusInternalServerError, sentry.LevelError},
		{"raw deadline", context.DeadlineExceeded, http.StatusGatewayTimeout, sentry.LevelWarning},
		{"invalid", ez.New(ez.EINVALID, "bad", nil), http.StatusBadRequest, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transport := &recordingTransport{}
			client, err := sentry.NewClient(sentry.ClientOptions{Transport: transport})
			require.NoError(t, err)
			hub := sentry.NewHub(client, sentry.NewScope())

			rec := httptest.NewRecorder()
			c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
			c.Set("sentry", hub) // same key sentryecho middleware uses

			request := requests.New(http.Header{}, "127.0.0.1")
			err = NewHandler(nil).ManageError(c, "test", request, tt.err)
			require.NoError(t, err)
			require.Equal(t, tt.status, rec.Code)

			if tt.level == "" {
				require.Empty(t, transport.events)
				return
			}
			require.Len(t, transport.events, 1)
			require.Equal(t, tt.level, transport.events[0].Level)
			require.Equal(t, request.GetID(), transport.events[0].Tags["Request ID"])
		})
	}
}

type loginBody struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (b *loginBody) Validate() error { return nil }

func TestManageErrorDoesNotLogBody(t *testing.T) {
	var buf bytes.Buffer
	previous := log.Logger
	log.Logger = zerolog.New(&buf)
	t.Cleanup(func() { log.Logger = previous })

	c := echo.New().NewContext(httptest.NewRequest(http.MethodPost, "/", nil), httptest.NewRecorder())

	request := requests.New(http.Header{}, "127.0.0.1")
	request.SetBody(&loginBody{Email: "user@example.com", Password: "s3cr3t-password"})

	err := NewHandler(nil).ManageError(c, "test", request, ez.New(ez.EINVALID, "bad credentials", nil))
	require.NoError(t, err)

	output := buf.String()
	require.Contains(t, output, "Request Error")
	require.Contains(t, output, "*handler.loginBody")
	require.NotContains(t, output, "s3cr3t-password")
	require.NotContains(t, output, "user@example.com")
	require.NotContains(t, output, "request_json")
}

func TestRequestRoundTrip(t *testing.T) {
	c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), httptest.NewRecorder())
	require.Nil(t, Request(c))

	request := requests.New(http.Header{}, "127.0.0.1")
	SetRequest(c, request)
	require.Same(t, request, Request(c))
}

func TestManageErrorSetsRequest(t *testing.T) {
	c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), httptest.NewRecorder())

	request := requests.New(http.Header{}, "127.0.0.1")
	err := NewHandler(nil).ManageError(c, "test", request, ez.New(ez.ENOTFOUND, "x", nil))
	require.NoError(t, err)
	require.Same(t, request, Request(c))
}

// reportErrors are the errors ReportToSentry is tested with, and whether
// each is reported and at which level.
var reportErrors = []struct {
	name     string
	err      error
	reported bool
	level    sentry.Level
}{
	{"internal", ez.New(ez.EINTERNAL, "boom", nil), true, sentry.LevelError},
	{"wrapped internal", ez.Wrap(ez.New(ez.EINTERNAL, "boom", nil)), true, sentry.LevelError},
	{"timeout", ez.New(ez.ETIMEOUT, "slow", nil), true, sentry.LevelWarning},
	{"unavailable", ez.New(ez.EUNAVAILABLE, "down", nil), true, sentry.LevelWarning},
	{"invalid", ez.New(ez.EINVALID, "bad", nil), false, ""},
	{"not found", ez.New(ez.ENOTFOUND, "gone", nil), false, ""},
	{"plain", errors.New("boom"), true, sentry.LevelError},
	{"raw canceled", context.Canceled, false, ""},
	{"raw deadline", context.DeadlineExceeded, true, sentry.LevelWarning},
	{"echo sentinel", echo.ErrNotFound, false, ""},
	{"echo HTTPError", echo.NewHTTPError(http.StatusTeapot, "no coffee"), false, ""},
	{"echo wrapping internal", echo.ErrBadRequest.Wrap(ez.New(ez.EINTERNAL, "boom", nil)), false, ""},
	{"internal wrapping echo", ez.New(ez.EINTERNAL, "boom", echo.ErrBadRequest), true, sentry.LevelError},
}

func TestReportToSentry(t *testing.T) {
	for _, tt := range reportErrors {
		t.Run(tt.name, func(t *testing.T) {
			transport := &recordingTransport{}
			client, err := sentry.NewClient(sentry.ClientOptions{Transport: transport})
			require.NoError(t, err)
			hub := sentry.NewHub(client, sentry.NewScope())

			rec := httptest.NewRecorder()
			c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
			c.Set("sentry", hub) // same key sentryecho middleware uses
			c.Response().Header().Set(echo.HeaderXRequestID, "request-1")
			request := requests.New(http.Header{}, "127.0.0.1")
			request.SetContext(context.WithValue(request.GetContext(), "user", map[string]interface{}{"id": "user-1"}))
			SetRequest(c, request)

			ReportToSentry(c, tt.err)

			if !tt.reported {
				require.Empty(t, transport.events)
				return
			}
			require.Len(t, transport.events, 1)
			require.Equal(t, tt.level, transport.events[0].Level)
			require.Equal(t, "request-1", transport.events[0].Tags["Request ID"])
			require.Equal(t, "user-1", transport.events[0].User.ID)
		})
	}
}

// The Sentry event of an error ManageError answers carries the response's
// X-Request-Id, or without it the id of the request ManageError stored.
func TestManageErrorSentryRequestID(t *testing.T) {
	tests := []struct {
		name   string
		header string
	}{
		{name: "from the request"},
		{name: "from the header", header: "request-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transport := &recordingTransport{}
			client, err := sentry.NewClient(sentry.ClientOptions{Transport: transport})
			require.NoError(t, err)
			hub := sentry.NewHub(client, sentry.NewScope())

			c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), httptest.NewRecorder())
			c.Set("sentry", hub) // same key sentryecho middleware uses
			if tt.header != "" {
				c.Response().Header().Set(echo.HeaderXRequestID, tt.header)
			}
			request := requests.New(http.Header{}, "127.0.0.1")
			err = NewHandler(nil).ManageError(c, "test", request, ez.New(ez.EINTERNAL, "boom", nil))
			require.NoError(t, err)

			want := tt.header
			if want == "" {
				want = request.GetID()
			}
			require.NotEmpty(t, want)
			require.Len(t, transport.events, 1)
			require.Equal(t, want, transport.events[0].Tags["Request ID"])
		})
	}
}

func TestReportToSentryWithoutHub(t *testing.T) {
	for _, tt := range reportErrors {
		c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), httptest.NewRecorder())
		require.NotPanics(t, func() {
			ReportToSentry(c, tt.err)
		}, tt.name)
	}
}
