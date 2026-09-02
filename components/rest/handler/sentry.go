package handler

import (
	"github.com/getsentry/sentry-go"
	sentryecho "github.com/getsentry/sentry-go/echo"
	"github.com/labstack/echo/v4"
	"github.com/vanclief/compose/components/rest/requests"
	"github.com/vanclief/ez"
)

// reportErrorToSentry reports an error to Sentry along with relevant request and user information.
func (h *BaseHandler) reportErrorToSentry(c echo.Context, request requests.Request, managedError error) {
	hub := sentryecho.GetHubFromContext(c)
	if hub == nil {
		// Sentry is not configured, nothing to do
		return
	}

	hub.WithScope(func(scope *sentry.Scope) {
		// Set the request ID
		scope.SetTag("Request ID", request.GetID())

		// Only internal errors are bugs. Everything else that reaches here is a
		// vendor outage, so downgrade it to a warning to let alert rules tell
		// the two apart.
		if ez.ErrorCode(managedError) != ez.EINTERNAL {
			scope.SetLevel(sentry.LevelWarning)
		}

		// Add breadcrumbs
		breadcrumbStacktrace(scope, managedError)

		// Add user context
		user, ok := request.GetContext().Value("user").(map[string]interface{})
		if !ok {
			// Handle case where user information is not available/not a map
			user = make(map[string]interface{})
		}

		sentryUser := sentry.User{IPAddress: request.GetIP()}
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
		hub.CaptureException(managedError)
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
