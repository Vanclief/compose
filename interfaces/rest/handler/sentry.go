package handler

import (
	"github.com/getsentry/sentry-go"
	sentryecho "github.com/getsentry/sentry-go/echo"
	"github.com/labstack/echo/v4"
	"github.com/vanclief/compose/interfaces/rest/requests"
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

		// Add breadcrumbs
		breadcrumbStacktrace(scope, managedError)

		// Add user context
		user, ok := request.GetContext().Value("user").(map[string]interface{})
		if !ok {
			// Handle case where user information is not available/not a map
			user = make(map[string]interface{})
		}

		sentryUser := sentry.User{IPAddress: request.GetIP()}
		if id, exists := user["id"].(string); exists {
			sentryUser.ID = id
		}
		if name, exists := user["name"].(string); exists {
			sentryUser.Name = name
		}
		if email, exists := user["email"].(string); exists {
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
	} else if e, ok := managedError.(*ez.Error); ok {
		scope.AddBreadcrumb(&sentry.Breadcrumb{
			Category: e.Code,
			Message:  e.String(),
			Level:    sentry.LevelError,
		}, 10)
		breadcrumbStacktrace(scope, e.Err)
	} else if ok && e.Err != nil {
		scope.AddBreadcrumb(&sentry.Breadcrumb{
			Category: e.Code,
			Message:  e.String(),
			Level:    sentry.LevelError,
		}, 10)
	} else {
		scope.AddBreadcrumb(&sentry.Breadcrumb{
			Message: managedError.Error(),
			Level:   sentry.LevelError,
		}, 10)
	}
}
