package handler

import (
	"net/http"

	"github.com/labstack/echo/v4"
	"github.com/rs/zerolog/log"
	"github.com/vanclief/compose/interfaces/rest/requests"
	"github.com/vanclief/ez"
)

type RESTServer interface {
	HandleRequest(requests *requests.Request) (interface{}, error)
}

// BaseHandler is a struct with basic methods that should be extended to properly handle a HTTP Service.
type BaseHandler struct {
	server RESTServer
}

func NewHandler(server RESTServer) *BaseHandler {
	return &BaseHandler{server: server}
}

func (h *BaseHandler) StandardRequest(c echo.Context, op string, requests *requests.Request, body requests.Body) error {
	requests.SetBody(body)

	response, managedError := h.server.HandleRequest(requests)
	if managedError != nil {
		return h.ManageError(c, op, requests, managedError)
	}

	return c.JSON(http.StatusOK, response)
}

func (h *BaseHandler) BindedRequest(c echo.Context, op string, requests *requests.Request, body requests.Body) error {
	if managedError := c.Bind(body); managedError != nil {
		return h.ManageError(c, op, requests, ez.New(op, ez.EINVALID, managedError.Error(), managedError))
	}

	requests.SetBody(body)

	response, managedError := h.server.HandleRequest(requests)
	if managedError != nil {
		return h.ManageError(c, op, requests, managedError)
	}

	return c.JSON(http.StatusOK, response)
}

func (h *BaseHandler) BindedRequestXMLResponse(c echo.Context, op string, requests *requests.Request, body requests.Body) error {
	if managedError := c.Bind(body); managedError != nil {
		return h.ManageError(c, op, requests, ez.New(op, ez.EINVALID, managedError.Error(), managedError))
	}

	requests.SetBody(body)

	response, managedError := h.server.HandleRequest(requests)
	if managedError != nil {
		return h.ManageError(c, op, requests, managedError)
	}

	return c.XMLPretty(http.StatusOK, response, "  ")
}

// ManageError translates an error into the appropriate HTTP error code
func (h *BaseHandler) ManageError(c echo.Context, op string, requests *requests.Request, managedError error) error {
	code := ez.ErrorCode(managedError)
	msg := ez.ErrorMessage(managedError)

	log.Error().
		Str("op", op).
		Str("code", code).
		Str("managedError", ez.ErrorMessage(managedError)).
		Str("request_id", requests.ID).
		Str("client", requests.Client).
		Msg("Handler.ManageError")

	if code == ez.EINTERNAL {
		log.Debug().Str("ID", requests.ID).Interface("Body", requests.Body).Msg("Internal Error")
		errorStacktrace(managedError)
	}

	stdErr := StandardError{Code: code, Message: msg, RequestID: requests.ID}
	return c.JSON(ez.ErrorToHTTPStatus(managedError), ErrorResponse{Error: stdErr})
}

func errorStacktrace(managedError error) {
	if managedError == nil {
		return
	} else if e, ok := managedError.(*ez.Error); ok {
		log.Debug().Msg(e.String())
		errorStacktrace(e.Err)
	} else if ok && e.Err != nil {
		log.Debug().Msg(e.String())
	} else {
		log.Debug().Msg(managedError.Error())
	}
}
