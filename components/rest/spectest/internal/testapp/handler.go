package testapp

import (
	"github.com/labstack/echo/v5"

	"github.com/vanclief/compose/components/rest/handler"
	"github.com/vanclief/compose/components/rest/spectest/internal/testapp/widgets"
)

// Handler holds the app's handler methods.
type Handler struct {
	*handler.BaseHandler
}

func (h *Handler) ListWidgets(c *echo.Context) error {
	body := &widgets.ListRequest{Search: c.QueryParam("search")}
	return h.JSONResponse(c, "ListWidgets", nil, body)
}

func (h *Handler) GetWidget(c *echo.Context) error {
	id, err := h.GetParameterInt64(c, "id")
	if err != nil {
		return err
	}
	return h.JSONResponse(c, "GetWidget", nil, &widgets.GetRequest{ID: id})
}

func (h *Handler) GetWidgetAdmin(c *echo.Context) error {
	id, err := h.GetParameterInt64(c, "id")
	if err != nil {
		return err
	}
	return h.JSONResponse(c, "GetWidgetAdmin", nil, &widgets.AdminRequest{ID: id})
}

func (h *Handler) CreateWidget(c *echo.Context) error {
	return h.BindedJSONResponse(c, "CreateWidget", nil, &widgets.CreateRequest{})
}

func (h *Handler) WidgetHistory(c *echo.Context) error {
	id, err := h.GetParameterInt64(c, "id")
	if err != nil {
		return err
	}
	return h.JSONResponse(c, "WidgetHistory", nil, &widgets.GetRequest{ID: id})
}

func (h *Handler) GetFile(c *echo.Context) error {
	return h.JSONResponse(c, "GetFile", nil, &widgets.FileRequest{Path: c.Param("*")})
}
