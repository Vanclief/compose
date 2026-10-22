package handler

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/labstack/echo/v4"
	"github.com/rs/zerolog/log"
	"github.com/vanclief/compose/components/rest/requests"
	"github.com/vanclief/ez"
)

// handleEchoError answers a bind failure. Errors the app's own UnmarshalJSON
// raised as ez errors keep their code, message and data, since the app wrote
// them for the client. Echo's and encoding/json's messages name Go types and
// fields, and an ez.Wrap of a json error carries only EINTERNAL, so those
// become a generic EINVALID with the detail kept in the log.
func (h *BaseHandler) handleEchoError(c echo.Context, op string, request requests.Request, err error) error {
	var appErr *ez.Error
	isAppErr := errors.As(err, &appErr)
	if isAppErr && ez.ErrorCode(appErr) != ez.EINTERNAL {
		return h.ManageError(c, op, request, ez.Wrap(appErr))
	}

	detail := err.Error()

	var echoErr *echo.HTTPError
	if errors.As(err, &echoErr) {
		detail = fmt.Sprintf("%v", echoErr.Message)
	}

	log.Warn().
		Str("id", request.GetID()).
		Str("op", op).
		Str("bind_error", detail).
		Msg("Request Bind Error")

	return h.ManageError(c, op, request, ez.New(ez.EINVALID, "Invalid request body", err))
}

func (h *BaseHandler) JSONResponse(c echo.Context, op string, request requests.Request, body requests.Body) error {
	request.SetBody(body)

	response, err := h.App.HandleRequest(request)
	if err != nil {
		return h.ManageError(c, op, request, err)
	}

	return c.JSON(http.StatusOK, response)
}

func (h *BaseHandler) BindedJSONResponse(c echo.Context, op string, request requests.Request, body requests.Body) error {
	err := c.Bind(body)
	if err != nil {
		return h.handleEchoError(c, op, request, err)
	}
	request.SetBody(body)

	response, err := h.App.HandleRequest(request)
	if err != nil {
		return h.ManageError(c, op, request, err)
	}

	return c.JSON(http.StatusOK, response)
}

func (h *BaseHandler) BindedXMLResponse(c echo.Context, op string, request requests.Request, body requests.Body) error {
	err := c.Bind(body)
	if err != nil {
		return h.handleEchoError(c, op, request, err)
	}
	request.SetBody(body)

	response, err := h.App.HandleRequest(request)
	if err != nil {
		return h.ManageError(c, op, request, err)
	}

	return c.XMLPretty(http.StatusOK, response, "  ")
}

func (h *BaseHandler) BlobResponse(c echo.Context, op string, request requests.Request, contentType string, body requests.Body) error {
	request.SetBody(body)

	response, managedError := h.App.HandleRequest(request)
	if managedError != nil {
		return h.ManageError(c, op, request, managedError)
	}

	bytes, ok := response.([]byte)
	if !ok {
		return h.ManageError(c, op, request, ez.New(ez.EINTERNAL, "HandleRequest response is not a byte slice", nil))
	}

	return c.Blob(http.StatusOK, contentType, bytes)
}
