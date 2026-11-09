package api_test

import (
	"net/http"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
	"github.com/vanclief/ez"

	"github.com/vanclief/compose/components/rest"
	"github.com/vanclief/compose/components/rest/api"
	"github.com/vanclief/compose/components/rest/handler"
	"github.com/vanclief/compose/components/rest/requests"
)

// publicRegistry registers one public operation, GET /me, on e.
func publicRegistry(e *echo.Echo) *api.Registry {
	reg := api.New(e, api.Public())
	api.GET(reg.Group("/me"), "", "get", who)
	return reg
}

// Ready calls the Echo error handler once and panics unless it answers an ez
// EINVALID error with a 4xx. A plain echo.New() is the guard that fails if
// Echo's default handler ever stops answering it with a 500.
func TestReadyRejectsErrorHandler(t *testing.T) {
	echoDefault := echo.DefaultHTTPErrorHandler(false)
	tests := []struct {
		name    string
		handler func(e *echo.Echo)
		want    string
	}{
		{"echo.New", func(e *echo.Echo) {}, "answered an ez EINVALID error with 500, not a 4xx"},
		{"default exposing errors", func(e *echo.Echo) {
			e.HTTPErrorHandler = echo.DefaultHTTPErrorHandler(true)
		}, "with 500"},
		{"wrapping the default", func(e *echo.Echo) {
			e.HTTPErrorHandler = func(c *echo.Context, err error) {
				echoDefault(c, err)
			}
		}, "with 500"},
		{"nil", func(e *echo.Echo) {
			e.HTTPErrorHandler = nil
		}, "the Echo error handler is nil"},
		{"writes nothing", func(e *echo.Echo) {
			e.HTTPErrorHandler = func(c *echo.Context, err error) {}
		}, "with 200"},
	}
	for _, tt := range tests {
		e := echo.New()
		tt.handler(e)
		reg := publicRegistry(e)

		msg := panicMessage(t, func() {
			reg.Ready()
		})
		require.Contains(t, msg, tt.want, tt.name)
		require.Contains(t, msg, "set e.HTTPErrorHandler = rest.ErrorHandler()", tt.name)
	}

	// A panic in the app's handler reaches the caller of Ready as it is.
	e := echo.New()
	e.HTTPErrorHandler = func(c *echo.Context, err error) {
		panic("handler broke")
	}
	reg := publicRegistry(e)
	require.PanicsWithValue(t, "handler broke", func() {
		reg.Ready()
	})
}

func TestReadyAcceptsErrorHandler(t *testing.T) {
	unprocessable := func(c *echo.Context, err error) {
		_ = c.NoContent(http.StatusUnprocessableEntity)
	}
	tests := []struct {
		name    string
		handler echo.HTTPErrorHandler
	}{
		{"rest.ErrorHandler", rest.ErrorHandler()},
		{"app handler answering 422", unprocessable},
	}
	for _, tt := range tests {
		e := echo.New()
		e.HTTPErrorHandler = tt.handler
		reg := publicRegistry(e)

		require.NotPanics(t, func() {
			reg.Ready()
		}, tt.name)
		whoAmI(t, &server{e: e, reg: reg}, "/me")
	}
}

// Ready's call reaches the reporter of rest.ErrorHandler once, as an ez
// EINVALID error with no request, and a later Ready does not repeat it.
func TestReadyReportsCheckOnce(t *testing.T) {
	var reported []error
	var requestsSeen []requests.Request
	report := func(c *echo.Context, err error) {
		reported = append(reported, err)
		requestsSeen = append(requestsSeen, handler.Request(c))
	}
	e := echo.New()
	e.HTTPErrorHandler = rest.ErrorHandler(rest.WithErrorReporter(report))
	reg := publicRegistry(e)

	reg.Ready()
	require.Len(t, reported, 1)
	require.Equal(t, ez.EINVALID, ez.ErrorCode(reported[0]))
	require.Nil(t, requestsSeen[0])

	reg.Ready()
	_, err := reg.Spec(api.V31)
	require.NoError(t, err)
	require.Len(t, reported, 1)
}

// A failed check leaves the registry not ready: a later Ready checks again,
// and succeeds once the app installs a handler.
func TestReadyAfterFailedCheck(t *testing.T) {
	e := echo.New()
	reg := publicRegistry(e)

	for range 2 {
		msg := panicMessage(t, func() {
			reg.Ready()
		})
		require.Contains(t, msg, "rest.ErrorHandler()")
	}

	e.HTTPErrorHandler = rest.ErrorHandler()
	require.NotPanics(t, func() {
		reg.Ready()
	})
	resp := whoAmI(t, &server{e: e, reg: reg}, "/me")
	require.Equal(t, "", resp.User)
}

// Spec and StrictSpec call Ready, so building only the document needs the
// handler too.
func TestSpecRejectsEchoDefaultErrorHandler(t *testing.T) {
	reg := publicRegistry(echo.New())

	msg := panicMessage(t, func() {
		_, _ = reg.Spec(api.V31)
	})
	require.Contains(t, msg, "with 500")
	require.Contains(t, msg, "rest.ErrorHandler()")

	msg = panicMessage(t, func() {
		reg.StrictSpec()
	})
	require.Contains(t, msg, "rest.ErrorHandler()")
}
