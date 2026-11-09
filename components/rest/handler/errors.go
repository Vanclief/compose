package handler

import (
	"time"

	"github.com/labstack/echo/v5"
	"github.com/rs/zerolog/log"
	"github.com/vanclief/compose/components/rest/requests"
	"github.com/vanclief/ez"
)

// handledErrorKey is the Echo context key ManageError stores the handled error
// under. It is namespaced so applications do not collide with it by accident.
const handledErrorKey = "compose/rest/handler:error"

// HandledError returns the error ManageError wrote for this request, before
// any ErrorTranslator ran, or nil when no error was handled. Middleware
// reads it after next(c) to log the outcome of a request.
func HandledError(c *echo.Context) error {
	err, ok := c.Get(handledErrorKey).(error)
	if !ok {
		return nil
	}
	return err
}

// ManageError translates an error into the appropriate HTTP error code
func (h *BaseHandler) ManageError(c *echo.Context, op string, request requests.Request, err error) error {
	c.Set(handledErrorKey, err)

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
	// outage, but still worth a Sentry event so someone notices.
	if code == ez.EINTERNAL || code == ez.ETIMEOUT || code == ez.EUNAVAILABLE {
		LogErrorStacktrace(err)
		h.reportErrorToSentry(c, request, err)
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
