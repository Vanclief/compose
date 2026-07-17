package handler

import (
	"github.com/vanclief/compose/components/rest/requests"
)

type App interface {
	HandleRequest(requests requests.Request) (interface{}, error)
}

// BaseHandler is a struct with basic methods that should be extended to properly handle a HTTP Service.
type BaseHandler struct {
	App App

	// ErrorTranslator, when set, is applied to every error right before it is
	// written to the HTTP response, so applications can localize error messages.
	// Only the translated error's message is used; the response code and HTTP
	// status always come from the original error. Returning nil keeps the
	// original error untouched.
	ErrorTranslator func(err error, request requests.Request) error
}

func NewHandler(App App) *BaseHandler {
	return &BaseHandler{App: App}
}
