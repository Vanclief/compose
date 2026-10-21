package rest

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

func TestNewConfigDefaults(t *testing.T) {
	cfg := newConfig()

	require.Equal(t, 10*time.Second, cfg.readHeaderTimeout)
	require.Equal(t, 120*time.Second, cfg.idleTimeout)
	require.Empty(t, cfg.allowedOrigins)
	require.Empty(t, cfg.bodyLimit)
	require.Zero(t, cfg.readTimeout)
	require.Zero(t, cfg.writeTimeout)
}

func TestNewConfigOverrides(t *testing.T) {
	cfg := newConfig(
		WithAllowedOrigins("https://app.example.com", "https://admin.example.com"),
		WithBodyLimit("10M"),
		WithReadHeaderTimeout(5*time.Second),
		WithIdleTimeout(30*time.Second),
		WithReadTimeout(time.Minute),
		WithWriteTimeout(2*time.Minute),
	)

	require.Equal(t, []string{"https://app.example.com", "https://admin.example.com"}, cfg.allowedOrigins)
	require.Equal(t, "10M", cfg.bodyLimit)
	require.Equal(t, 5*time.Second, cfg.readHeaderTimeout)
	require.Equal(t, 30*time.Second, cfg.idleTimeout)
	require.Equal(t, time.Minute, cfg.readTimeout)
	require.Equal(t, 2*time.Minute, cfg.writeTimeout)
}

func newTestEcho(opts ...Option) *echo.Echo {
	e := echo.New()
	configure(e, zerolog.Nop(), opts...)
	e.POST("/echo", func(c echo.Context) error {
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

func TestConfigureServerTimeouts(t *testing.T) {
	e := newTestEcho()
	require.Equal(t, 10*time.Second, e.Server.ReadHeaderTimeout)
	require.Equal(t, 120*time.Second, e.Server.IdleTimeout)
	require.Zero(t, e.Server.ReadTimeout)
	require.Zero(t, e.Server.WriteTimeout)

	e = newTestEcho(WithReadHeaderTimeout(3*time.Second), WithIdleTimeout(time.Minute))
	require.Equal(t, 3*time.Second, e.Server.ReadHeaderTimeout)
	require.Equal(t, time.Minute, e.Server.IdleTimeout)
	require.Zero(t, e.Server.ReadTimeout)
	require.Zero(t, e.Server.WriteTimeout)

	e = newTestEcho(WithReadTimeout(30*time.Second), WithWriteTimeout(45*time.Second))
	require.Equal(t, 30*time.Second, e.Server.ReadTimeout)
	require.Equal(t, 45*time.Second, e.Server.WriteTimeout)
	require.Equal(t, 10*time.Second, e.Server.ReadHeaderTimeout)
	require.Equal(t, 120*time.Second, e.Server.IdleTimeout)
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

func TestConfigureBodyLimit(t *testing.T) {
	e := newTestEcho(WithBodyLimit("1K"))

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
	e := newTestEcho()

	req := httptest.NewRequest(http.MethodPost, "/echo", strings.NewReader(strings.Repeat("a", 2048)))
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, rec.Body.String(), 2048)
}
