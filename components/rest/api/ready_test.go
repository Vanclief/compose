package api_test

import (
	"context"
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

type runRequest struct {
	requests.NoRules
	ID string `json:"-" path:"id"`
}

// runsRegistry registers the public operation runs.get, GET /runs/:id, on e,
// answering {"user":"registry"}.
func runsRegistry(e *echo.Echo) *api.Registry {
	reg := api.New(e, api.Public())
	api.GET(reg.Group("/runs"), "/:id", "get", func(ctx context.Context, req *runRequest) (whoResponse, error) {
		return whoResponse{User: "registry"}, nil
	})
	return reg
}

// plain is an Echo handler outside the registry, answering {"user":"plain"}.
func plain(c *echo.Context) error {
	return c.JSON(http.StatusOK, whoResponse{User: "plain"})
}

// Ready panics, naming the operation, when a plain Echo route registered
// after it on its method and path, with any placeholder name, replaced it;
// the registry stays not ready, so a second Ready panics too.
func TestReadyRejectsReplacedRoute(t *testing.T) {
	for _, path := range []string{"/runs/:id", "/runs/:other"} {
		e := newEcho()
		reg := runsRegistry(e)
		e.GET(path, plain)

		// The plain route is what Echo serves.
		resp := whoAmI(t, &server{e: e, reg: reg}, "/runs/1")
		require.Equal(t, "plain", resp.User, path)

		for range 2 {
			msg := panicMessage(t, func() {
				reg.Ready()
			})
			require.Contains(t, msg, "api: GET /runs/:id (runs.get): another route was registered on its method and path after the registry's", path)
		}
	}
}

// A plain route on another method or path, or one the registry's replaced
// because it was registered first, leaves the operation its route.
func TestReadyAcceptsOtherRoutes(t *testing.T) {
	tests := []struct {
		name  string
		setup func(e *echo.Echo) *api.Registry
	}{
		{"other method", func(e *echo.Echo) *api.Registry {
			reg := runsRegistry(e)
			e.POST("/runs/:id", plain)
			return reg
		}},
		{"other path", func(e *echo.Echo) *api.Registry {
			reg := runsRegistry(e)
			e.GET("/runs/:id/logs", plain)
			e.GET("/runs", plain)
			return reg
		}},
		{"same path before", func(e *echo.Echo) *api.Registry {
			e.GET("/runs/:id", plain)
			return runsRegistry(e)
		}},
		{"other placeholder before", func(e *echo.Echo) *api.Registry {
			e.GET("/runs/:other", plain)
			return runsRegistry(e)
		}},
		// Echo's route list reads as after the "/runs/:other" replacement
		// (the registry's route, then GET /runs/:other), yet the registry's
		// route was registered last and serves.
		{"same path and other placeholder before", func(e *echo.Echo) *api.Registry {
			e.GET("/runs/:id", plain)
			e.GET("/runs/:other", plain)
			return runsRegistry(e)
		}},
	}
	for _, tt := range tests {
		e := newEcho()
		reg := tt.setup(e)

		require.NotPanics(t, func() {
			reg.Ready()
		}, tt.name)
		resp := whoAmI(t, &server{e: e, reg: reg}, "/runs/1")
		require.Equal(t, "registry", resp.User, tt.name)
	}
}

// silentRouter routes every request to plain without reporting which route
// it took.
type silentRouter struct {
	echo.Router
}

func (silentRouter) Route(c *echo.Context) echo.HandlerFunc {
	return plain
}

// A router that reports no route for a request cannot tell whether an
// operation still owns its route, so Ready does not fail on it.
func TestReadyAcceptsSilentRouter(t *testing.T) {
	e := echo.NewWithConfig(echo.Config{
		Router:           silentRouter{Router: echo.NewRouter(echo.RouterConfig{AllowOverwritingRoute: true})},
		HTTPErrorHandler: rest.ErrorHandler(),
	})
	reg := runsRegistry(e)
	e.GET("/runs/:id", plain)

	require.NotPanics(t, func() {
		reg.Ready()
	})
}
