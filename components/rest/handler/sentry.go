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
// there; ManageError calls it for plain routes. Only errors worth a person's
// attention are reported: an ez EINTERNAL at error level, an ez ETIMEOUT or
// EUNAVAILABLE (usually a vendor outage) at warning level, and an error that
// is neither an ez error nor carries an Echo status (a plain error, a
// recovered panic) at error level. Client errors and Echo's own errors (404,
// 405, 413, 415, *echo.HTTPError) are skipped. The event carries the
// X-Request-Id of the response, the server's id under rest.Configure, and the
// user the request's context holds (see Request).
func ReportToSentry(c *echo.Context, err error) {
	hub := sentryecho.GetHubFromContext(c)
	if hub == nil {
		// Sentry is not configured, nothing to do
		return
	}

	level := sentry.LevelError
	var ezErr *ez.Error
	if errors.As(err, &ezErr) {
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
	} else if echo.StatusCode(err) != 0 {
		return
	}

	hub.WithScope(func(scope *sentry.Scope) {
		scope.SetTag("Request ID", c.Response().Header().Get(echo.HeaderXRequestID))
		scope.SetLevel(level)

		// Add breadcrumbs
		breadcrumbStacktrace(scope, err)

		// Add user context
		user := map[string]interface{}{}
		request := Request(c)
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
