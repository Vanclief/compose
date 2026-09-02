package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
	"github.com/vanclief/compose/components/rest/requests"
	"github.com/vanclief/ez"
)

// recordingTransport keeps every event Sentry would have sent.
type recordingTransport struct{ events []*sentry.Event }

func (t *recordingTransport) Configure(sentry.ClientOptions) {}
func (t *recordingTransport) SendEvent(e *sentry.Event)      { t.events = append(t.events, e) }
func (t *recordingTransport) Flush(time.Duration) bool       { return true }

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
