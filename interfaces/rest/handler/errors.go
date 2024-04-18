package handler

import (
	"time"

	"github.com/labstack/echo/v4"
	"github.com/rs/zerolog/log"
	"github.com/vanclief/compose/interfaces/rest/requests"
	"github.com/vanclief/ez"
)

// ManageError translates an error into the appropriate HTTP error code
func (h *BaseHandler) ManageError(c echo.Context, op string, request requests.Request, err error) error {
	code := ez.ErrorCode(err)
	msg := ez.ErrorMessage(err)

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

	if code == ez.EINTERNAL {
		LogErrorStacktrace(err)
		h.reportErrorToSentry(c, request, err)
	}

	stdErr := StandardError{Code: code, Message: msg, RequestID: request.GetID()}
	return c.JSON(ez.ErrorToHTTPStatus(err), ErrorResponse{Error: stdErr})
}

func LogErrorStacktrace(err error) {
	if err == nil {
		return
	} else if e, ok := err.(*ez.Error); ok {
		log.Debug().Msg(e.String())
		LogErrorStacktrace(e.Err)
	} else if ok && e.Err != nil {
		log.Debug().Msg(e.String())
	} else {
		log.Debug().Msg(err.Error())
	}
}
