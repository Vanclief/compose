package handler

import (
	"errors"

	"github.com/getsentry/sentry-go"
	sentryecho "github.com/getsentry/sentry-go/echo"
	"github.com/labstack/echo/v5"
	"github.com/vanclief/ez"
)

// ReportToSentry reports err to the Sentry hub the sentryecho middleware put
// on c, and does nothing without one. It has the signature
// rest.WithErrorReporter takes, so an app that reports to Sentry passes it
// there. Only errors worth a person's attention are reported, classified
// with ez.ErrorCode as ManageError does:
// EINTERNAL (also a plain error or a recovered panic) at error level,
// ETIMEOUT or EUNAVAILABLE (usually a vendor outage; also a raw
// context.DeadlineExceeded) at warning level. Everything else is skipped:
// client errors, ECANCELED (also a raw context.Canceled, the client gave
// up), and an error that is not an ez error but carries an Echo status (404,
// 405, 413, 415, *echo.HTTPError). The error is an ez error when it is an
// *ez.Error itself, or when an *ez.Error is in its chain and the chain
// carries no Echo status, as rest.ErrorHandler has it; so an Echo error
// wrapping an ez error, as c.Bind or middleware.KeyAuth make, is skipped. The
// event carries the id of the request Request returns, which the server made
// and ManageError writes in its answer, or without a request the
// X-Request-Id of the response, the same id under rest.Configure; and the
// user the request's context holds (see Request).
func ReportToSentry(c *echo.Context, err error) {
	var ezErr *ez.Error
	_, isEZ := err.(*ez.Error)
	if !isEZ {
		isEZ = errors.As(err, &ezErr) && echo.StatusCode(err) == 0
	}
	if !isEZ && echo.StatusCode(err) != 0 {
		return
	}

	level := sentry.LevelError
	switch ez.ErrorCode(err) {
	case ez.EINTERNAL:
		// A bug, at error level.
	case ez.ETIMEOUT, ez.EUNAVAILABLE:
		// A vendor outage rather than a bug: a warning lets alert rules
		// tell the two apart.
		level = sentry.LevelWarning
	default:
		return
	}

	captureToSentry(c, err, level)
}

// captureToSentry sends err at level to the Sentry hub the sentryecho
// middleware put on c, with the request id and user ReportToSentry
// describes, and does nothing without a hub.
func captureToSentry(c *echo.Context, err error, level sentry.Level) {
	hub := sentryecho.GetHubFromContext(c)
	if hub == nil {
		// Sentry is not configured, nothing to do
		return
	}

	hub.WithScope(func(scope *sentry.Scope) {
		// The request's id is the server's; the header may be the client's,
		// copied there by Echo's middleware.RequestID.
		request := Request(c)
		requestID := c.Response().Header().Get(echo.HeaderXRequestID)
		if request != nil {
			requestID = request.GetID()
		}
		scope.SetTag("Request ID", requestID)
		scope.SetLevel(level)

		// Add breadcrumbs
		breadcrumbStacktrace(scope, err)

		// Add user context
		user := map[string]interface{}{}
		if request != nil {
			value, ok := request.GetContext().Value("user").(map[string]interface{})
			if ok {
				user = value
			}
		}

		sentryUser := sentry.User{IPAddress: c.RealIP()}
		id, exists := user["id"].(string)
		if exists {
			sentryUser.ID = id
		}
		name, exists := user["name"].(string)
		if exists {
			sentryUser.Name = name
		}
		email, exists := user["email"].(string)
		if exists {
			sentryUser.Email = email
		}

		scope.SetUser(sentryUser)

		// Finally, capturing the error.
		hub.CaptureException(err)
	})
}

func breadcrumbStacktrace(scope *sentry.Scope, managedError error) {
	if managedError == nil {
		return
	}

	e, ok := managedError.(*ez.Error)
	if ok {
		scope.AddBreadcrumb(&sentry.Breadcrumb{
			Category: e.Code,
			Message:  e.String(),
			Level:    sentry.LevelError,
		}, 10)
		breadcrumbStacktrace(scope, e.Err)
		return
	}

	scope.AddBreadcrumb(&sentry.Breadcrumb{
		Message: managedError.Error(),
		Level:   sentry.LevelError,
	}, 10)
}
