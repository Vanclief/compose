package rest

import (
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/rs/zerolog"
	"github.com/ziflex/lecho/v3"
)

const (
	infoLevel = 2

	defaultReadHeaderTimeout = 10 * time.Second
	defaultIdleTimeout       = 120 * time.Second
)

type config struct {
	allowedOrigins    []string
	bodyLimit         string
	readHeaderTimeout time.Duration
	idleTimeout       time.Duration
}

// Option configures Start.
type Option func(*config)

// WithAllowedOrigins restricts CORS to the given origins. Without it, or with
// no origins, every origin is allowed.
func WithAllowedOrigins(origins ...string) Option {
	return func(c *config) {
		c.allowedOrigins = origins
	}
}

// WithBodyLimit caps request bodies using echo's BodyLimit middleware, e.g.
// "10M". Larger bodies get 413. An invalid limit panics when the server is
// configured. Without it bodies are unbounded.
func WithBodyLimit(limit string) Option {
	return func(c *config) {
		c.bodyLimit = limit
	}
}

// WithReadHeaderTimeout overrides the default 10s read-header timeout.
func WithReadHeaderTimeout(d time.Duration) Option {
	return func(c *config) {
		c.readHeaderTimeout = d
	}
}

// WithIdleTimeout overrides the default 120s keep-alive idle timeout.
func WithIdleTimeout(d time.Duration) Option {
	return func(c *config) {
		c.idleTimeout = d
	}
}

func newConfig(opts ...Option) config {
	cfg := config{
		readHeaderTimeout: defaultReadHeaderTimeout,
		idleTimeout:       defaultIdleTimeout,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	return cfg
}

// Start configures logging, recovery, CORS and server timeouts on e and serves
// on port. It always sets a 10s read-header timeout and a 120s idle timeout.
//
// Without WithAllowedOrigins every origin is allowed, and without WithBodyLimit
// request bodies are unbounded, so production callers should pass both.
func Start(e *echo.Echo, log zerolog.Logger, port string, opts ...Option) {
	configure(e, log, opts...)
	e.Logger.Fatal(e.Start(":" + port))
}

func configure(e *echo.Echo, log zerolog.Logger, opts ...Option) {
	cfg := newConfig(opts...)

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

	if len(cfg.allowedOrigins) > 0 {
		e.Use(middleware.CORSWithConfig(middleware.CORSConfig{AllowOrigins: cfg.allowedOrigins}))
	} else {
		e.Use(middleware.CORS())
	}

	if cfg.bodyLimit != "" {
		e.Use(middleware.BodyLimit(cfg.bodyLimit))
	}

	// Server timeouts. e.Start serves e.Server and only sets Addr, Handler and
	// ErrorLog on it, so these values take effect.
	e.Server.ReadHeaderTimeout = cfg.readHeaderTimeout
	e.Server.IdleTimeout = cfg.idleTimeout

	e.HideBanner = true
}
