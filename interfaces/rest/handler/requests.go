package handler

import (
	"net/http"

	"github.com/labstack/echo/v4"
	"github.com/vanclief/compose/interfaces/rest/requests"
	"github.com/vanclief/ez"
)

// TODO: Rename this to JSONResponse
func (h *BaseHandler) StandardRequest(c echo.Context, op string, request requests.Request, body requests.Body) error {
	request.SetBody(body)

	response, managedError := h.App.HandleRequest(request)
	if managedError != nil {
		return h.ManageError(c, op, request, managedError)
	}

	return c.JSON(http.StatusOK, response)
}

// TODO: Rename this to BindedJSONResponse
func (h *BaseHandler) BindedRequest(c echo.Context, op string, request requests.Request, body requests.Body) error {
	if managedError := c.Bind(body); managedError != nil {
		return h.ManageError(c, op, request, ez.New(op, ez.EINVALID, managedError.Error(), managedError))
	}

	request.SetBody(body)

	response, managedError := h.App.HandleRequest(request)
	if managedError != nil {
		return h.ManageError(c, op, request, managedError)
	}

	return c.JSON(http.StatusOK, response)
}

// TODO: Rename this to BindedXMLResponse
func (h *BaseHandler) BindedRequestXMLResponse(c echo.Context, op string, request requests.Request, body requests.Body) error {
	if managedError := c.Bind(body); managedError != nil {
		return h.ManageError(c, op, request, ez.New(op, ez.EINVALID, managedError.Error(), managedError))
	}

	request.SetBody(body)

	response, managedError := h.App.HandleRequest(request)
	if managedError != nil {
		return h.ManageError(c, op, request, managedError)
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
