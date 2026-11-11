package handler

import (
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/labstack/echo/v5"
	"github.com/rs/zerolog/log"
	"github.com/vanclief/compose/components/rest/requests"
	"github.com/vanclief/ez"
)

// requestKey is the Echo context key SetRequest stores the request under. It
// is namespaced so applications do not collide with it by accident.
const requestKey = "compose/rest/handler:request"

// Request returns the request of this HTTP request, as the api registry and
// ManageError store it with SetRequest, or nil when nothing stored one (a
// route that never reached either, an unknown route, a panic before the
// handler). Echo middleware reads it after next(c), and an error translator
// or reporter of rest.ErrorHandler reads it to localize or describe an error.
func Request(c *echo.Context) requests.Request {
	request, _ := c.Get(requestKey).(requests.Request)
	return request
}

// SetRequest stores request as the request of this HTTP request, for Request.
// The api registry calls it as soon as it builds the request, and ManageError
// does for plain routes.
func SetRequest(c *echo.Context, request requests.Request) {
	c.Set(requestKey, request)
}

// ManageError translates an error into the appropriate HTTP error code
func (h *BaseHandler) ManageError(c *echo.Context, op string, request requests.Request, err error) error {
	SetRequest(c, request)

	code := ez.ErrorCode(err)

	log.Error().
		Str("id", request.GetID()).
		Type("body_type", request.GetBody()).
		Str("latency", time.Since(request.GetCreatedAt()).String()).
		Str("error_code", code).
		Str("error_message", ez.ErrorMessage(err)).
		Str("request_client", request.GetClient()).
		Str("request_ip", request.GetIP()).
		Msg("Request Error")

	// Internal errors are bugs. Timeouts and unavailability are usually a vendor
	// outage, but still worth a Sentry event so someone notices, at warning
	// level. Every error here is answered with its ez status, so an Echo
	// error, which ReportToSentry skips for its Echo status, is a 500 and
	// reported too.
	if code == ez.EINTERNAL || code == ez.ETIMEOUT || code == ez.EUNAVAILABLE {
		LogErrorStacktrace(err)
		level := sentry.LevelWarning
		if code == ez.EINTERNAL {
			level = sentry.LevelError
		}
		captureToSentry(c, err, level)
	}

	status := ez.ErrorToHTTPStatus(err)

	if h.ErrorTranslator != nil {
		translated := h.ErrorTranslator(err, request)
		if translated != nil {
			err = translated
		}
	}

	stdErr := StandardError{Code: code, Message: ez.ErrorMessage(err), RequestID: request.GetID()}
	return c.JSON(status, ErrorResponse{Error: stdErr})
}

func LogErrorStacktrace(err error) {
	if err == nil {
		return
	}

	e, ok := err.(*ez.Error)
	if ok {
		log.Debug().Msg(e.String())
		LogErrorStacktrace(e.Err)
		return
	}

	log.Debug().Msg(err.Error())
}
