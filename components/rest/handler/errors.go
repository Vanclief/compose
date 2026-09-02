package handler

import (
	"time"

	"github.com/labstack/echo/v4"
	"github.com/rs/zerolog/log"
	"github.com/vanclief/compose/components/rest/requests"
	"github.com/vanclief/ez"
)

// ManageError translates an error into the appropriate HTTP error code
func (h *BaseHandler) ManageError(c echo.Context, op string, request requests.Request, err error) error {
	code := ez.ErrorCode(err)

	log.Error().
		Str("id", request.GetID()).
		Type("body_type", request.GetBody()).
		Str("latency", time.Since(request.GetCreatedAt()).String()).
		Str("error_code", code).
		Str("error_message", ez.ErrorMessage(err)).
		Str("request_client", request.GetClient()).
		Str("request_ip", request.GetIP()).
		Interface("request_json", request.GetBody()).
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
