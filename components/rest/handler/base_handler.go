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
}

func NewHandler(App App) *BaseHandler {
	return &BaseHandler{App: App}
}
