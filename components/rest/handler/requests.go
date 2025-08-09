package handler

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/labstack/echo/v4"
	"github.com/vanclief/compose/components/rest/requests"
	"github.com/vanclief/ez"
)

// handleEchoError extracts the clean error message from Echo's error type
func (h *BaseHandler) handleEchoError(c echo.Context, op string, request requests.Request, err error) error {
	var echoErr *echo.HTTPError
	if errors.As(err, &echoErr) {
		msg := ""
		if m, ok := echoErr.Message.(string); ok {
			msg = m
		} else {
			msg = fmt.Sprintf("%v", echoErr.Message)
		}
		return h.ManageError(c, op, request, ez.New(op, ez.EINVALID, msg, err))
	}
	return h.ManageError(c, op, request, ez.New(op, ez.EINVALID, err.Error(), err))
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
	if err := c.Bind(body); err != nil {
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
	if err := c.Bind(body); err != nil {
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
		return h.ManageError(c, op, request, ez.New(op, ez.EINTERNAL, "HandleRequest response is not a byte slice", nil))
	}

	c.Response().Header().Set("Content-Type", contentType)

	return c.Blob(http.StatusOK, "application/pdf", bytes)
}
