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

	// sessionTokenHeader mirrors session.TokenHeader, the response header
	// session.SetRotated writes a rotated token to. It is copied rather than
	// imported so rest does not depend on the session package and its bun
	// store; router_test.go checks the two stay equal.
	sessionTokenHeader = "Session-Token"
)

type config struct {
	allowedOrigins    []string
	credentialedCORS  bool
	bodyLimit         string
	readHeaderTimeout time.Duration
	idleTimeout       time.Duration
	readTimeout       time.Duration
	writeTimeout      time.Duration
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

// WithCredentialedCORS sends Access-Control-Allow-Credentials: true, so a
// browser app on another origin can send and receive cookies. It is needed for
// cookie-based session auth from another origin. It only takes effect together
// with WithAllowedOrigins: browsers reject credentials with a wildcard origin,
// so without allowed origins it is ignored and Start logs a warning. The
// cookie path also needs the origin checks of requests.WithCookie (the CSRF
// rule in docs/auth.md), since CORS alone does not stop cross-site writes.
func WithCredentialedCORS() Option {
	return func(c *config) {
		c.credentialedCORS = true
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

// WithReadTimeout caps the time to read a whole request, headers and body,
// so a client trickling body bytes cannot hold a connection open. It is off by
// default (zero means unlimited). Uploads that take longer than d are cut off,
// so size it for the slowest upload the service accepts.
func WithReadTimeout(d time.Duration) Option {
	return func(c *config) {
		c.readTimeout = d
	}
}

// WithWriteTimeout caps the time from the end of reading the request headers
// to the end of writing the response. It is off by default (zero means
// unlimited). Long-running handlers, large downloads and streaming or SSE
// responses that take longer than d are cut off, so only set it on services
// that do none of those.
func WithWriteTimeout(d time.Duration) Option {
	return func(c *config) {
		c.writeTimeout = d
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
// The read and write timeouts are unset (unlimited) unless WithReadTimeout or
// WithWriteTimeout is passed.
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

	// With no allowed origins echo allows every origin, as middleware.CORS
	// does. Session-Token is exposed so cross-origin clients can read a
	// rotated session token.
	cors := middleware.CORSConfig{
		AllowOrigins:  cfg.allowedOrigins,
		ExposeHeaders: []string{sessionTokenHeader},
	}
	if cfg.credentialedCORS {
		if len(cfg.allowedOrigins) > 0 {
			cors.AllowCredentials = true
		} else {
			log.Warn().Msg("rest: WithCredentialedCORS ignored without WithAllowedOrigins")
		}
	}
	e.Use(middleware.CORSWithConfig(cors))

	if cfg.bodyLimit != "" {
		e.Use(middleware.BodyLimit(cfg.bodyLimit))
	}

	// Server timeouts. e.Start serves e.Server and only sets Addr, Handler and
	// ErrorLog on it, so these values take effect.
	e.Server.ReadHeaderTimeout = cfg.readHeaderTimeout
	e.Server.IdleTimeout = cfg.idleTimeout
	e.Server.ReadTimeout = cfg.readTimeout
	e.Server.WriteTimeout = cfg.writeTimeout

	e.HideBanner = true
}
