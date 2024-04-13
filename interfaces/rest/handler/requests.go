package handler

import (
	"net/http"

	"github.com/labstack/echo/v4"
	"github.com/vanclief/compose/interfaces/rest/requests"
	"github.com/vanclief/ez"
)

func (h *BaseHandler) StandardRequest(c echo.Context, op string, request requests.Request, body requests.Body) error {
	request.SetBody(body)

	response, managedError := h.App.HandleRequest(request)
	if managedError != nil {
		return h.ManageError(c, op, request, managedError)
	}

	return c.JSON(http.StatusOK, response)
}

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
