package handler

import (
	"net/http"

	"github.com/labstack/echo/v4"
	"github.com/rs/zerolog/log"
	"github.com/vanclief/compose/interfaces/rest/requests"
	"github.com/vanclief/ez"
)

type App interface {
	HandleRequest(requests *requests.Request) (interface{}, error)
}

// BaseHandler is a struct with basic methods that should be extended to properly handle a HTTP Service.
type BaseHandler struct {
	App App
}

func NewHandler(App App) *BaseHandler {
	return &BaseHandler{App: App}
}

func (h *BaseHandler) StandardRequest(c echo.Context, op string, request *requests.Request, body requests.Body) error {
	request.SetBody(body)

	response, managedError := h.App.HandleRequest(request)
	if managedError != nil {
		return h.ManageError(c, op, request, managedError)
	}

	return c.JSON(http.StatusOK, response)
}

func (h *BaseHandler) BindedRequest(c echo.Context, op string, request *requests.Request, body requests.Body) error {
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

func (h *BaseHandler) BindedRequestXMLResponse(c echo.Context, op string, request *requests.Request, body requests.Body) error {
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

// ManageError translates an error into the appropriate HTTP error code
func (h *BaseHandler) ManageError(c echo.Context, op string, request *requests.Request, err error) error {
	code := ez.ErrorCode(err)
	msg := ez.ErrorMessage(err)

	log.Error().
		Str("op", op).
		Str("code", code).
		Str("managedError", ez.ErrorMessage(err)).
		Str("request_id", request.ID).
		Str("client", request.Client).
		Msg("Handler.ManageError")

	if code == ez.EINTERNAL {
		log.Debug().Str("ID", request.ID).Interface("Body", request.Body).Msg("Internal Error")
		LogErrorStacktrace(err)
	}

	stdErr := StandardError{Code: code, Message: msg, RequestID: request.ID}
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
