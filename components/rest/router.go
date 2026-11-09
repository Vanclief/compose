package rest

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"
	"github.com/rs/zerolog"
	"github.com/vanclief/compose/components/rest/requests"
)

const (
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
	bodyLimit         int64
	readHeaderTimeout time.Duration
	idleTimeout       time.Duration
	readTimeout       time.Duration
	writeTimeout      time.Duration
	errorTranslator   func(c *echo.Context, err error) error
	errorReporter     func(c *echo.Context, err error)
}

// Option configures Configure, Serve, Start and ErrorHandler. The options are
// one set: Configure uses WithAllowedOrigins, WithCredentialedCORS and
// WithBodyLimit, Serve uses the four timeout options, ErrorHandler uses
// WithErrorTranslator and WithErrorReporter, and each ignores the options
// that are not its own, so an app passes the same options to all of them.
type Option func(*config)

// WithAllowedOrigins restricts CORS to the given origins. Without it, with no
// origins, or with "*" among them, every origin is allowed, and the other
// origins are neither used nor checked. An origin is a scheme and host with
// an optional port, like "https://app.example.com", matched whole ignoring
// case and sent back to the browser as written, so write it in lowercase as
// browsers send it. A pattern such as "https://*.example.com" matches
// nothing. An empty or padded origin (as from splitting an unset or spaced
// variable), one without a scheme, or one with a path, trailing slash,
// non-empty query or fragment makes Configure, and so Start, panic.
func WithAllowedOrigins(origins ...string) Option {
	return func(c *config) {
		c.allowedOrigins = origins
	}
}

// WithCredentialedCORS sends Access-Control-Allow-Credentials: true, so a
// browser app on another origin can send and receive cookies. It is needed for
// cookie-based session auth from another origin. It only takes effect together
// with WithAllowedOrigins naming the origins: browsers reject credentials with
// a wildcard origin, so without allowed origins, or with "*" among them, it is
// ignored and Configure logs a warning. The cookie path also needs the origin
// checks of requests.WithCookie (the CSRF rule in docs/auth.md), since CORS
// alone does not stop cross-site writes.
func WithCredentialedCORS() Option {
	return func(c *config) {
		c.credentialedCORS = true
	}
}

// WithBodyLimit caps request bodies at limit bytes using echo's BodyLimit
// middleware, e.g. 10 << 20 for 10 MiB. Larger bodies get 413. Without it, or
// with a zero or negative limit, bodies are unbounded.
func WithBodyLimit(limit int64) Option {
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

// Start configures e with Configure and serves it on port with Serve,
// passing both the same opts; see both for what they do. An app that needs
// the configured stack without Start's server, or middleware of its own
// inside the stack, calls Configure and then Serve instead of Start, never
// Configure and Start, which would register everything twice.
func Start(ctx context.Context, e *echo.Echo, log zerolog.Logger, port string, opts ...Option) {
	Configure(e, log, opts...)
	Serve(ctx, e, log, port, opts...)
}

// Serve serves e on port with server timeouts. It registers no middleware;
// Configure does that. It uses the timeout options and ignores the others, so
// an app passes it the same options as Configure and ErrorHandler. It always
// sets a 10s read-header timeout and a 120s idle timeout. The read and write
// timeouts are unset (unlimited) unless WithReadTimeout or WithWriteTimeout
// is passed.
//
// Serve blocks until ctx ends, then stops accepting connections, waits up to
// 10s for the requests in flight, closes the connections of any still running
// and returns. The caller owns the shutdown: signal.NotifyContext for SIGINT
// and SIGTERM gives the usual behaviour, and closing the database or flushing
// logs comes after Serve returns. A handler still running at that point has
// lost its connection, so its reads and writes fail and c.Request().Context()
// ends once net/http notices, but it runs on until it returns or its own
// timeout ends (the context requests.New builds does not derive from the
// connection's and ends after 15s by default), so it can hit what the caller
// tears down.
//
// With a ctx that has already ended Serve returns at once. If the server
// cannot listen or fails while serving, Serve logs the error to log at fatal
// level and exits the process.
func Serve(ctx context.Context, e *echo.Echo, log zerolog.Logger, port string, opts ...Option) {
	if ctx.Err() != nil {
		return
	}

	err := startConfig(newConfig(opts...), ":"+port, log).Start(ctx, e)
	if err != nil {
		log.Fatal().Err(err).Msg("rest: server failed")
	}
}

// startConfig is the Echo server Serve runs: no banner, the configured
// timeouts put on the http.Server just before it serves, and the connections
// still open when the graceful timeout runs out closed; the handlers
// themselves are not waited for.
func startConfig(cfg config, address string, log zerolog.Logger) echo.StartConfig {
	var server *http.Server
	return echo.StartConfig{
		Address:    address,
		HideBanner: true,
		BeforeServeFunc: func(s *http.Server) error {
			server = s
			setTimeouts(s, cfg)
			return nil
		},
		OnShutdownError: func(err error) {
			log.Warn().Err(err).Msg("rest: shutdown did not finish cleanly, closing the connections")
			_ = server.Close()
		},
	}
}

// Configure sets e's logger and registers compose's middleware on e, in this
// order:
//   - the request id: every request gets a new UUID, whatever X-Request-Id
//     the client sent, as X-Request-Id on the response and on the request and
//     as the ID of the request requests.New builds, so the response header,
//     the request log line, a handler's request and the errors answered for
//     it all carry the same id (see assignRequestID);
//   - the request logger, one line per request with the request's id, which
//     has e.HTTPErrorHandler answer an error before logging it (see
//     requestLogger);
//   - Echo's Recover, which turns a panic into a 500 error the request logger
//     logs with its stack;
//   - CORS, exposing Session-Token and X-Request-Id to cross-origin clients;
//   - the body limit, when WithBodyLimit is passed;
//   - RejectNUL, unconditionally, after CORS and the body limit; see RejectNUL
//     for what it inspects and its memory use.
//
// It uses WithAllowedOrigins, WithCredentialedCORS and WithBodyLimit and
// ignores the other options, so an app passes it the same options as Serve
// and ErrorHandler.
// Without WithAllowedOrigins every origin is allowed, and without
// WithBodyLimit request bodies are unbounded, so production callers should
// pass both.
//
// The request log and Echo's own lines (the listening address, net/http
// server errors, an app's c.Logger() calls) are written to log at info level
// and above whatever level log has, as lecho did, so a logger set to warn
// still logs requests.
//
// Configure needs no listener, so the configured stack can be served through
// httptest in tests, or by an app that runs its own server. Middleware
// registered with e.Use after Configure runs inside the stack, after
// RejectNUL and so inside Recover (a Sentry middleware that re-panics, for
// one); middleware registered before Configure runs outside it. An app calls
// either Start, or Configure then Serve, never both Configure and Start, which
// would register everything twice. ErrorHandler answers, in compose's error
// shape, the errors this stack logs; Configure does not install it, and the
// request logger calls whatever e.HTTPErrorHandler is when the request runs.
func Configure(e *echo.Echo, log zerolog.Logger, opts ...Option) {
	cfg := newConfig(opts...)

	// Request lines and Echo's own lines are written from info level up
	// whatever level the app's logger has, as lecho forced before.
	echoLog := log.Level(zerolog.InfoLevel)

	// Echo's own lines (the listening address, net/http server errors, an
	// app's c.Logger() calls) go to the app's log.
	e.Logger = slog.New(zerolog.NewSlogHandler(echoLog))

	// Middlewares
	// First, so the request logger and every later handler see the id.
	e.Use(assignRequestID)

	e.Use(middleware.RequestLoggerWithConfig(requestLogger(echoLog)))

	e.Use(middleware.Recover())

	// With no allowed origins, or "*" among them, every origin is allowed,
	// and Echo refuses credentials with "*". Session-Token is exposed so
	// cross-origin clients can read a rotated session token, X-Request-Id so
	// they can quote the request's id.
	cors := middleware.CORSConfig{
		AllowOrigins:  cfg.allowedOrigins,
		ExposeHeaders: []string{sessionTokenHeader, echo.HeaderXRequestID},
	}
	wildcard := len(cfg.allowedOrigins) == 0 || slices.Contains(cfg.allowedOrigins, "*")
	if wildcard {
		cors.AllowOrigins = []string{"*"}
	}
	if cfg.credentialedCORS {
		if wildcard {
			log.Warn().Msg("rest: WithCredentialedCORS ignored without WithAllowedOrigins naming the origins")
		} else {
			cors.AllowCredentials = true
		}
	}
	e.Use(middleware.CORSWithConfig(cors))

	if cfg.bodyLimit > 0 {
		e.Use(middleware.BodyLimit(cfg.bodyLimit))
	}

	// After CORS, so preflights are answered first and its 400 carries the
	// CORS headers, and after the body limit, so a body over the limit gets
	// the limit's 413, also when RejectNUL is the one reading it.
	e.Use(RejectNUL())
}

// setTimeouts puts the configured timeouts on the server Echo is about to
// serve with. Echo sets a 30s read timeout of its own first; the configured
// value replaces it, so with no WithReadTimeout the read timeout is unlimited,
// as documented.
func setTimeouts(s *http.Server, cfg config) {
	s.ReadHeaderTimeout = cfg.readHeaderTimeout
	s.IdleTimeout = cfg.idleTimeout
	s.ReadTimeout = cfg.readTimeout
	s.WriteTimeout = cfg.writeTimeout
}

// assignRequestID gives every request an id of the server's own, a new UUID.
// An X-Request-Id the client sent is never used: any caller can send one, so
// it could put text of its choice in the logs and in error bodies, or give
// several requests one id. The id is set as X-Request-Id on the response, for
// the client to quote; as X-Request-Id on the request, replacing the
// client's, which is where Echo's request logger and a handler read it; and
// with requests.SetRequestID, which is where requests.New takes it from.
func assignRequestID(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		id := uuid.New().String()
		header := c.Request().Header
		header.Set(echo.HeaderXRequestID, id)
		requests.SetRequestID(header, id)
		c.Response().Header().Set(echo.HeaderXRequestID, id)
		return next(c)
	}
}

// requestLogger logs one line per request with the fields lecho used to
// write, so existing log queries keep working: id (the X-Request-Id header
// of the request, which under Configure is the id assignRequestID gave it, so
// every line has one), remote_ip, host, method, uri, user_agent, status,
// referer, latency, latency_human, bytes_in and bytes_out, with no message. A
// request whose handler returned an error is first answered by Echo's
// HTTPErrorHandler (HandleError), so its status is the one actually sent, an
// ez error's own status under ErrorHandler, and then logged with the error at
// warn level for a 4xx and error level for a 5xx. The logger returns the error
// afterwards, so Echo calls the HTTPErrorHandler a second time on the
// committed response, which ErrorHandler leaves alone.
func requestLogger(log zerolog.Logger) middleware.RequestLoggerConfig {
	return middleware.RequestLoggerConfig{
		LogLatency:       true,
		LogRemoteIP:      true,
		LogHost:          true,
		LogMethod:        true,
		LogURI:           true,
		LogRequestID:     true,
		LogReferer:       true,
		LogUserAgent:     true,
		LogStatus:        true,
		LogContentLength: true,
		LogResponseSize:  true,
		HandleError:      true,
		LogValuesFunc: func(c *echo.Context, v middleware.RequestLoggerValues) error {
			evt := log.Info()
			if v.Status >= http.StatusInternalServerError {
				evt = log.Error()
			} else if v.Status >= http.StatusBadRequest {
				evt = log.Warn()
			}
			if v.Error != nil {
				evt = evt.Err(v.Error)
			}
			if v.RequestID != "" {
				evt = evt.Str("id", v.RequestID)
			}

			bytesIn := v.ContentLength
			if bytesIn == "" {
				bytesIn = "0"
			}

			evt.Str("remote_ip", v.RemoteIP).
				Str("host", v.Host).
				Str("method", v.Method).
				Str("uri", v.URI).
				Str("user_agent", v.UserAgent).
				Int("status", v.Status).
				Str("referer", v.Referer).
				Dur("latency", v.Latency).
				Str("latency_human", v.Latency.String()).
				Str("bytes_in", bytesIn).
				Str("bytes_out", strconv.FormatInt(v.ResponseSize, 10)).
				Send()
			return nil
		},
	}
}
