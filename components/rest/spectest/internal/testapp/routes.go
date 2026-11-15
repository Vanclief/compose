// Package testapp is the application the spectest end-to-end test runs the
// OpenAPI generator on and serves with Echo, so both helpers are checked
// against the generator's real output.
package testapp

import (
	"net/http"

	"github.com/labstack/echo/v5"
)

// Routes registers the app's routes on e.
func Routes(e *echo.Echo, h *Handler) {
	widgets := e.Group("/widgets")
	widgets.GET("", h.ListWidgets)
	widgets.GET("/:id", h.GetWidget)
	widgets.POST("", h.CreateWidget)
	widgets.GET("/:id/admin", h.GetWidgetAdmin)

	e.GET("/files/*", h.GetFile)
	e.GET("/health", health)

	// Add is not a call the generator documents: the test's generator
	// config leaves this route out, so the document lacks it.
	e.Add(http.MethodGet, "/widgets/:id/history", h.WidgetHistory)
}

func health(c *echo.Context) error {
	return c.NoContent(http.StatusOK)
}
