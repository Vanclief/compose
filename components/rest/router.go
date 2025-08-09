package rest

import (
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/rs/zerolog"
	"github.com/ziflex/lecho/v3"
)

const (
	infoLevel = 2
)

func Start(e *echo.Echo, log zerolog.Logger, port string) {
	// Logger
	logger := lecho.From(log,
		lecho.WithLevel(infoLevel),
		lecho.WithTimestamp(),
	)
	e.Logger = logger

	// Middlewares
	e.Use(lecho.Middleware(lecho.Config{
		Logger: logger,
	}))

	e.Use(middleware.Recover())
	e.Use(middleware.CORS())

	e.HideBanner = true
	e.Logger.Fatal(e.Start(":" + port))
}
