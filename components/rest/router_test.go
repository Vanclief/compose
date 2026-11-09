package rest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"github.com/vanclief/compose/components/auth/session"
)

func TestNewConfigDefaults(t *testing.T) {
	cfg := newConfig()

	require.Equal(t, 10*time.Second, cfg.readHeaderTimeout)
	require.Equal(t, 120*time.Second, cfg.idleTimeout)
	require.Empty(t, cfg.allowedOrigins)
	require.Zero(t, cfg.bodyLimit)
	require.Zero(t, cfg.readTimeout)
	require.Zero(t, cfg.writeTimeout)
}

func TestNewConfigOverrides(t *testing.T) {
	cfg := newConfig(
		WithAllowedOrigins("https://app.example.com", "https://admin.example.com"),
		WithBodyLimit(10<<20),
		WithReadHeaderTimeout(5*time.Second),
		WithIdleTimeout(30*time.Second),
		WithReadTimeout(time.Minute),
		WithWriteTimeout(2*time.Minute),
	)

	require.Equal(t, []string{"https://app.example.com", "https://admin.example.com"}, cfg.allowedOrigins)
	require.Equal(t, int64(10<<20), cfg.bodyLimit)
	require.Equal(t, 5*time.Second, cfg.readHeaderTimeout)
	require.Equal(t, 30*time.Second, cfg.idleTimeout)
	require.Equal(t, time.Minute, cfg.readTimeout)
	require.Equal(t, 2*time.Minute, cfg.writeTimeout)
}

func newTestEcho(opts ...Option) *echo.Echo {
	e := echo.New()
	Configure(e, zerolog.Nop(), opts...)
	e.POST("/echo", func(c *echo.Context) error {
		body, err := io.ReadAll(c.Request().Body)
		if err != nil {
			return err
		}
		return c.String(http.StatusOK, string(body))
	})
	return e
}

func preflight(e *echo.Echo, origin string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodOptions, "/echo", nil)
	req.Header.Set(echo.HeaderOrigin, origin)
	req.Header.Set(echo.HeaderAccessControlRequestMethod, http.MethodPost)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func crossOriginGet(e *echo.Echo, origin string) *httptest.ResponseRecorder {
	e.GET("/get", func(c *echo.Context) error {
		return c.NoContent(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/get", nil)
	req.Header.Set(echo.HeaderOrigin, origin)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

// waitStopped waits for a started server to return, giving up rather than
// hanging the run when it does not.
func waitStopped(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("the server did not stop")
	}
}

// echoServer is an http.Server as echo.StartConfig hands it to BeforeServeFunc,
// with Echo's own 30s read timeout already on it.
func echoServer() *http.Server {
	return &http.Server{ReadTimeout: 30 * time.Second}
}

func TestSetTimeouts(t *testing.T) {
	s := echoServer()
	setTimeouts(s, newConfig())
	require.Equal(t, 10*time.Second, s.ReadHeaderTimeout)
	require.Equal(t, 120*time.Second, s.IdleTimeout)
	require.Zero(t, s.ReadTimeout)
	require.Zero(t, s.WriteTimeout)

	s = echoServer()
	setTimeouts(s, newConfig(WithReadHeaderTimeout(3*time.Second), WithIdleTimeout(time.Minute)))
	require.Equal(t, 3*time.Second, s.ReadHeaderTimeout)
	require.Equal(t, time.Minute, s.IdleTimeout)
	require.Zero(t, s.ReadTimeout)
	require.Zero(t, s.WriteTimeout)

	s = echoServer()
	setTimeouts(s, newConfig(WithReadTimeout(30*time.Second), WithWriteTimeout(45*time.Second)))
	require.Equal(t, 30*time.Second, s.ReadTimeout)
	require.Equal(t, 45*time.Second, s.WriteTimeout)
	require.Equal(t, 10*time.Second, s.ReadHeaderTimeout)
	require.Equal(t, 120*time.Second, s.IdleTimeout)
}

// serveOK answers GET /ok on e through ServeHTTP, with no listener.
func serveOK(e *echo.Echo) *httptest.ResponseRecorder {
	e.GET("/ok", func(c *echo.Context) error {
		return c.String(http.StatusOK, "ok")
	})
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ok", nil))
	return rec
}

func TestStartReturnsOnEndedContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var logs bytes.Buffer
	e := echo.New()
	done := make(chan struct{})
	go func() {
		Start(ctx, e, zerolog.New(&logs), "0")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return on an ended context")
	}
	// Nothing was listened on, so Echo logged no start line
	require.Empty(t, logs.String())

	// Start still configured e, as it did before Serve was split out
	rec := serveOK(e)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotEmpty(t, rec.Header().Get(echo.HeaderXRequestID))
	require.Contains(t, logs.String(), `"status":200`)
}

// Serve registers no middleware, so a route on an echo only served has no
// request id and is not logged.
func TestServeRegistersNoMiddleware(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var logs bytes.Buffer
	e := echo.New()
	Serve(ctx, e, zerolog.New(&logs), "0", WithBodyLimit(1), WithAllowedOrigins("https://app.example.com"))

	rec := serveOK(e)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Empty(t, rec.Header().Get(echo.HeaderXRequestID))
	require.Empty(t, logs.String())
}

func TestStartConfigServesAndStops(t *testing.T) {
	var logs bytes.Buffer
	e := echo.New()
	Configure(e, zerolog.New(&logs))
	e.GET("/ok", func(c *echo.Context) error {
		return c.String(http.StatusOK, "ok")
	})

	server := startConfig(newConfig(WithReadTimeout(time.Minute)), "127.0.0.1:0", zerolog.Nop())
	var served *http.Server
	before := server.BeforeServeFunc
	server.BeforeServeFunc = func(s *http.Server) error {
		served = s
		return before(s)
	}
	addr := make(chan string, 1)
	server.ListenerAddrFunc = func(a net.Addr) {
		addr <- a.String()
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- server.Start(ctx, e)
	}()
	t.Cleanup(func() {
		cancel()
		waitStopped(t, done)
	})

	var address string
	select {
	case address = <-addr:
	case err := <-done:
		done <- err
		t.Fatalf("server stopped before listening: %v", err)
	}
	client := http.Client{Timeout: 5 * time.Second}
	res, err := client.Get("http://" + address + "/ok")
	require.NoError(t, err)
	body, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	require.NoError(t, res.Body.Close())
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Equal(t, "ok", string(body))

	cancel()
	select {
	case err = <-done:
		done <- err
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after its context ended")
	}

	// Echo's own 30s read timeout was replaced by the configured one
	require.Equal(t, time.Minute, served.ReadTimeout)
	require.Equal(t, 10*time.Second, served.ReadHeaderTimeout)
	require.Equal(t, 120*time.Second, served.IdleTimeout)
	require.Zero(t, served.WriteTimeout)
	require.Contains(t, logs.String(), "http(s) server started")
	require.Contains(t, logs.String(), `"status":200`)
}

func TestStartConfigClosesRequestsPastGracefulTimeout(t *testing.T) {
	var logs bytes.Buffer
	e := echo.New()
	Configure(e, zerolog.New(&logs))
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	e.GET("/slow", func(c *echo.Context) error {
		started <- struct{}{}
		<-release
		return c.NoContent(http.StatusOK)
	})

	server := startConfig(newConfig(), "127.0.0.1:0", zerolog.New(&logs))
	server.GracefulTimeout = 50 * time.Millisecond
	addr := make(chan string, 1)
	server.ListenerAddrFunc = func(a net.Addr) {
		addr <- a.String()
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- server.Start(ctx, e)
	}()
	t.Cleanup(func() {
		cancel()
		close(release)
		waitStopped(t, done)
	})

	var address string
	select {
	case address = <-addr:
	case err := <-done:
		done <- err
		t.Fatalf("server stopped before listening: %v", err)
	}
	client := http.Client{Timeout: 5 * time.Second}
	got := make(chan error, 1)
	go func() {
		res, err := client.Get("http://" + address + "/slow")
		if err == nil {
			err = res.Body.Close()
		}
		got <- err
	}()

	// Stop while the request is in flight, past the graceful timeout
	select {
	case <-started:
	case err := <-got:
		t.Fatalf("request failed before reaching the handler: %v", err)
	}
	cancel()
	select {
	case err := <-done:
		done <- err
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after the graceful timeout")
	}
	// Well inside the client's own timeout, which would fail the request too
	select {
	case err := <-got:
		require.Error(t, err, "the slow request was answered")
	case <-time.After(2 * time.Second):
		t.Fatal("the slow request's connection was not closed")
	}
	require.Contains(t, logs.String(), "closing the connections")
}

func TestConfigureRequestLog(t *testing.T) {
	var logs bytes.Buffer
	e := echo.New()
	Configure(e, zerolog.New(&logs))
	e.GET("/items/:id", func(c *echo.Context) error {
		return c.String(http.StatusOK, "ok")
	})
	e.GET("/fail", func(c *echo.Context) error {
		return echo.NewHTTPError(http.StatusTeapot, "no coffee")
	})
	e.GET("/panic", func(c *echo.Context) error {
		panic("kaboom")
	})

	req := httptest.NewRequest(http.MethodGet, "/items/7?x=1", strings.NewReader("abc"))
	req.Header.Set(echo.HeaderXRequestID, "req-1")
	req.Header.Set(echo.HeaderContentLength, "3")
	req.Header.Set("User-Agent", "test-agent")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	var line map[string]any
	err := json.Unmarshal(logs.Bytes(), &line)
	require.NoError(t, err)
	require.Equal(t, "info", line["level"])
	require.Equal(t, "req-1", line["id"])
	require.Equal(t, "192.0.2.1", line["remote_ip"])
	require.Equal(t, "example.com", line["host"])
	require.Equal(t, http.MethodGet, line["method"])
	require.Equal(t, "/items/7?x=1", line["uri"])
	require.Equal(t, "test-agent", line["user_agent"])
	require.Equal(t, float64(http.StatusOK), line["status"])
	require.Equal(t, "3", line["bytes_in"])
	require.Equal(t, "2", line["bytes_out"])
	require.Contains(t, line, "latency")
	require.Contains(t, line, "latency_human")
	require.NotContains(t, line, "message")

	logs.Reset()
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/fail", nil))
	require.Equal(t, http.StatusTeapot, rec.Code)

	line = map[string]any{}
	err = json.Unmarshal(logs.Bytes(), &line)
	require.NoError(t, err)
	require.Equal(t, "error", line["level"])
	require.Equal(t, float64(http.StatusTeapot), line["status"])
	require.Contains(t, line["error"], "no coffee")
	// Without a client id the line has the generated one the response carries
	require.NotEmpty(t, rec.Header().Get(echo.HeaderXRequestID))
	require.Equal(t, rec.Header().Get(echo.HeaderXRequestID), line["id"])
	require.Equal(t, "0", line["bytes_in"])

	// A recovered panic is a 500 with the stack in the error field, where
	// Echo v4's Recover logged a separate line through lecho
	logs.Reset()
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panic", nil))
	require.Equal(t, http.StatusInternalServerError, rec.Code)

	line = map[string]any{}
	err = json.Unmarshal(logs.Bytes(), &line)
	require.NoError(t, err)
	require.Equal(t, "error", line["level"])
	require.Equal(t, float64(http.StatusInternalServerError), line["status"])
	require.Contains(t, line["error"], "kaboom")
	require.Contains(t, line["error"], "goroutine")
}

func TestConfigureCORSDefaultAllowsEveryOrigin(t *testing.T) {
	rec := preflight(newTestEcho(), "https://anywhere.example.com")

	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Equal(t, "*", rec.Header().Get(echo.HeaderAccessControlAllowOrigin))
}

func TestConfigureCORSAllowedOrigins(t *testing.T) {
	e := newTestEcho(WithAllowedOrigins("https://app.example.com"))

	rec := preflight(e, "https://app.example.com")
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Equal(t, "https://app.example.com", rec.Header().Get(echo.HeaderAccessControlAllowOrigin))

	rec = preflight(e, "https://evil.example.com")
	require.Empty(t, rec.Header().Get(echo.HeaderAccessControlAllowOrigin))
}

func TestSessionTokenHeaderMirrorsSession(t *testing.T) {
	require.Equal(t, session.TokenHeader, sessionTokenHeader)
}

func TestConfigureCORSExposesSessionToken(t *testing.T) {
	rec := crossOriginGet(newTestEcho(), "https://anywhere.example.com")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "*", rec.Header().Get(echo.HeaderAccessControlAllowOrigin))
	require.Contains(t, rec.Header().Get(echo.HeaderAccessControlExposeHeaders), session.TokenHeader)
	require.Contains(t, rec.Header().Get(echo.HeaderAccessControlExposeHeaders), echo.HeaderXRequestID)
	require.Empty(t, rec.Header().Get(echo.HeaderAccessControlAllowCredentials))

	rec = crossOriginGet(newTestEcho(WithAllowedOrigins("https://app.example.com")), "https://app.example.com")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "https://app.example.com", rec.Header().Get(echo.HeaderAccessControlAllowOrigin))
	require.Contains(t, rec.Header().Get(echo.HeaderAccessControlExposeHeaders), session.TokenHeader)
	require.Empty(t, rec.Header().Get(echo.HeaderAccessControlAllowCredentials))
}

func TestConfigureCredentialedCORS(t *testing.T) {
	e := newTestEcho(WithAllowedOrigins("https://app.example.com"), WithCredentialedCORS())

	rec := crossOriginGet(e, "https://app.example.com")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "https://app.example.com", rec.Header().Get(echo.HeaderAccessControlAllowOrigin))
	require.Equal(t, "true", rec.Header().Get(echo.HeaderAccessControlAllowCredentials))

	rec = preflight(e, "https://app.example.com")
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Equal(t, "https://app.example.com", rec.Header().Get(echo.HeaderAccessControlAllowOrigin))
	require.Equal(t, "true", rec.Header().Get(echo.HeaderAccessControlAllowCredentials))

	rec = preflight(e, "https://evil.example.com")
	require.Empty(t, rec.Header().Get(echo.HeaderAccessControlAllowOrigin))
	require.Empty(t, rec.Header().Get(echo.HeaderAccessControlAllowCredentials))
}

func TestConfigureCredentialedCORSNeedsAllowedOrigins(t *testing.T) {
	var logs bytes.Buffer
	e := echo.New()
	Configure(e, zerolog.New(&logs), WithCredentialedCORS())

	rec := crossOriginGet(e, "https://anywhere.example.com")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "*", rec.Header().Get(echo.HeaderAccessControlAllowOrigin))
	require.Empty(t, rec.Header().Get(echo.HeaderAccessControlAllowCredentials))
	require.Contains(t, logs.String(), "WithCredentialedCORS ignored without WithAllowedOrigins")

	logs.Reset()
	e = echo.New()
	Configure(e, zerolog.New(&logs), WithAllowedOrigins("*"), WithCredentialedCORS())

	rec = crossOriginGet(e, "https://anywhere.example.com")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "*", rec.Header().Get(echo.HeaderAccessControlAllowOrigin))
	require.Empty(t, rec.Header().Get(echo.HeaderAccessControlAllowCredentials))
	require.Contains(t, logs.String(), "WithCredentialedCORS ignored without WithAllowedOrigins")
}

func TestConfigureWildcardAmongOriginsSkipsTheOthers(t *testing.T) {
	// An origin Echo would refuse next to "*" is neither used nor checked
	e := newTestEcho(WithAllowedOrigins("https://bad.example.com/path", "*"))

	rec := crossOriginGet(e, "https://anywhere.example.com")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "*", rec.Header().Get(echo.HeaderAccessControlAllowOrigin))
}

func TestConfigureLogsRequestsUnderWarnLevel(t *testing.T) {
	var logs bytes.Buffer
	e := echo.New()
	Configure(e, zerolog.New(&logs).Level(zerolog.WarnLevel))
	e.GET("/ok", func(c *echo.Context) error {
		return c.NoContent(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ok", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var line map[string]any
	err := json.Unmarshal(logs.Bytes(), &line)
	require.NoError(t, err)
	require.Equal(t, "info", line["level"])
	require.Equal(t, float64(http.StatusOK), line["status"])
	require.True(t, e.Logger.Enabled(t.Context(), slog.LevelInfo))
}

func TestConfigureEchoLoggerWritesZerolog(t *testing.T) {
	var logs bytes.Buffer
	e := echo.New()
	Configure(e, zerolog.New(&logs).With().Timestamp().Logger())

	e.Logger.Info("started", "address", ":8080")
	var line map[string]any
	err := json.Unmarshal(logs.Bytes(), &line)
	require.NoError(t, err)
	require.Equal(t, "info", line["level"])
	require.Equal(t, "started", line["message"])
	require.Equal(t, ":8080", line["address"])
	// The logger's own timestamp is the only one
	require.Equal(t, 1, strings.Count(logs.String(), `"time"`))

	logs.Reset()
	e.Logger.Error("failed", "error", errors.New("listen failed"))
	line = map[string]any{}
	err = json.Unmarshal(logs.Bytes(), &line)
	require.NoError(t, err)
	require.Equal(t, "error", line["level"])
	require.Equal(t, "listen failed", line["error"])
}

func TestConfigureBodyLimit(t *testing.T) {
	e := newTestEcho(WithBodyLimit(1024))

	req := httptest.NewRequest(http.MethodPost, "/echo", strings.NewReader(strings.Repeat("a", 2048)))
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)

	req = httptest.NewRequest(http.MethodPost, "/echo", strings.NewReader("small"))
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "small", rec.Body.String())
}

func TestConfigureNoBodyLimitByDefault(t *testing.T) {
	for _, e := range []*echo.Echo{newTestEcho(), newTestEcho(WithBodyLimit(0)), newTestEcho(WithBodyLimit(-1))} {
		req := httptest.NewRequest(http.MethodPost, "/echo", strings.NewReader(strings.Repeat("a", 2048)))
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code)
		require.Len(t, rec.Body.String(), 2048)
	}
}
