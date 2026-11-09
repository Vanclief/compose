package rest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"github.com/vanclief/compose/components/rest/handler"
	"github.com/vanclief/ez"
)

func errorEnvelope(t *testing.T, rec *httptest.ResponseRecorder) handler.StandardError {
	t.Helper()
	var res handler.ErrorResponse
	err := json.Unmarshal(rec.Body.Bytes(), &res)
	require.NoError(t, err, rec.Body.String())
	return res.Error
}

func TestErrorHandler(t *testing.T) {
	e := echo.New()
	e.HTTPErrorHandler = ErrorHandler()
	e.GET("/message", func(c *echo.Context) error {
		return echo.NewHTTPError(http.StatusTeapot, "no coffee")
	})
	e.GET("/wrapped", func(c *echo.Context) error {
		return fmt.Errorf("save: %w", echo.NewHTTPError(http.StatusConflict, "taken"))
	})
	e.GET("/empty-message", func(c *echo.Context) error {
		return echo.NewHTTPError(http.StatusBadRequest, "").Wrap(errors.New("secret cause"))
	})
	e.POST("/bind", func(c *echo.Context) error {
		var body struct {
			Name string `json:"name"`
		}
		return c.Bind(&body)
	})
	e.GET("/plain", func(c *echo.Context) error {
		return errors.New("secret database detail")
	})
	e.GET("/response-id", func(c *echo.Context) error {
		c.Response().Header().Set(echo.HeaderXRequestID, "response-id")
		return echo.ErrForbidden
	})

	tests := []struct {
		name      string
		method    string
		path      string
		ctype     string
		requestID string
		status    int
		want      handler.StandardError
	}{
		{
			name:   "unknown route",
			method: http.MethodGet, path: "/nope",
			status: http.StatusNotFound,
			want:   handler.StandardError{Code: ez.ENOTFOUND, Message: "Not Found"},
		},
		{
			name:   "method not allowed",
			method: http.MethodDelete, path: "/message",
			status: http.StatusMethodNotAllowed,
			want:   handler.StandardError{Code: ez.EINVALID, Message: "Method Not Allowed"},
		},
		{
			name:   "HTTPError keeps its message",
			method: http.MethodGet, path: "/message",
			status: http.StatusTeapot,
			want:   handler.StandardError{Code: ez.EINVALID, Message: "no coffee"},
		},
		{
			name:   "wrapped HTTPError",
			method: http.MethodGet, path: "/wrapped",
			status: http.StatusConflict,
			want:   handler.StandardError{Code: ez.ECONFLICT, Message: "taken"},
		},
		{
			name:   "HTTPError without a message gets the status text",
			method: http.MethodGet, path: "/empty-message",
			status: http.StatusBadRequest,
			want:   handler.StandardError{Code: ez.EINVALID, Message: "Bad Request"},
		},
		{
			name:   "binder 415",
			method: http.MethodPost, path: "/bind", ctype: "text/plain",
			status: http.StatusUnsupportedMediaType,
			want:   handler.StandardError{Code: ez.EINVALID, Message: "Unsupported Media Type"},
		},
		{
			name:   "plain error",
			method: http.MethodGet, path: "/plain",
			status: http.StatusInternalServerError,
			want:   handler.StandardError{Code: ez.EINTERNAL, Message: "Internal Server Error"},
		},
		{
			name:   "request id from the request",
			method: http.MethodGet, path: "/nope", requestID: "request-id",
			status: http.StatusNotFound,
			want:   handler.StandardError{Code: ez.ENOTFOUND, Message: "Not Found", RequestID: "request-id"},
		},
		{
			name:   "request id from the response",
			method: http.MethodGet, path: "/response-id",
			status: http.StatusForbidden,
			want:   handler.StandardError{Code: ez.ENOTAUTHORIZED, Message: "Forbidden", RequestID: "response-id"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader("x"))
			if tt.ctype != "" {
				req.Header.Set(echo.HeaderContentType, tt.ctype)
			}
			if tt.requestID != "" {
				req.Header.Set(echo.HeaderXRequestID, tt.requestID)
			}
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)

			require.Equal(t, tt.status, rec.Code, rec.Body.String())
			require.Equal(t, tt.want, errorEnvelope(t, rec))
			require.NotContains(t, rec.Body.String(), "secret")
		})
	}
}

func TestErrorHandlerLeavesCommittedResponse(t *testing.T) {
	e := echo.New()
	e.HTTPErrorHandler = ErrorHandler()
	e.GET("/partial", func(c *echo.Context) error {
		err := c.String(http.StatusOK, "partial")
		if err != nil {
			return err
		}
		return errors.New("failed after writing")
	})

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/partial", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "partial", rec.Body.String())
}

func TestErrorHandlerHeadHasNoBody(t *testing.T) {
	e := echo.New()
	e.HTTPErrorHandler = ErrorHandler()

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/nope", nil))
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Empty(t, rec.Body.String())
}

// failingWriter is a ResponseWriter whose body writes fail, as when the client
// has gone.
type failingWriter struct {
	*httptest.ResponseRecorder
}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("client gone")
}

func TestErrorHandlerLogsWriteFailure(t *testing.T) {
	var logs bytes.Buffer
	e := echo.New()
	e.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
	e.HTTPErrorHandler = ErrorHandler()

	e.ServeHTTP(failingWriter{httptest.NewRecorder()}, httptest.NewRequest(http.MethodGet, "/nope", nil))
	require.Contains(t, logs.String(), "rest: error handler failed to send the error to the client")
	require.Contains(t, logs.String(), "client gone")
}

// Through Configure the envelope, the response header and the request log
// line share one id, and the panic reaches the log but not the client.
func TestConfigureErrorHandlerRecoveredPanic(t *testing.T) {
	var logs bytes.Buffer
	e := echo.New()
	Configure(e, zerolog.New(&logs))
	e.HTTPErrorHandler = ErrorHandler()
	e.GET("/panic", func(c *echo.Context) error {
		panic("kaboom")
	})

	tests := []struct {
		name     string
		clientID string
	}{
		{name: "generated id"},
		{name: "client id", clientID: "client-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs.Reset()
			req := httptest.NewRequest(http.MethodGet, "/panic", nil)
			if tt.clientID != "" {
				req.Header.Set(echo.HeaderXRequestID, tt.clientID)
			}
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)

			require.Equal(t, http.StatusInternalServerError, rec.Code)
			id := rec.Header().Get(echo.HeaderXRequestID)
			require.NotEmpty(t, id)
			if tt.clientID != "" {
				require.Equal(t, tt.clientID, id)
			}
			require.Equal(t, handler.StandardError{
				Code:      ez.EINTERNAL,
				Message:   "Internal Server Error",
				RequestID: id,
			}, errorEnvelope(t, rec))
			require.NotContains(t, rec.Body.String(), "kaboom")

			var line map[string]any
			err := json.Unmarshal(logs.Bytes(), &line)
			require.NoError(t, err, logs.String())
			require.Equal(t, "error", line["level"])
			require.Equal(t, id, line["id"])
			require.Contains(t, line["error"], "kaboom")
			require.Contains(t, line["error"], "goroutine")
		})
	}
}

// Middleware registered after Configure runs inside Recover.
func TestConfigureRecoversLaterMiddleware(t *testing.T) {
	var logs bytes.Buffer
	e := echo.New()
	Configure(e, zerolog.New(&logs))
	e.HTTPErrorHandler = ErrorHandler()
	e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			panic("middleware kaboom")
		}
	})
	e.GET("/ok", func(c *echo.Context) error {
		return c.String(http.StatusOK, "ok")
	})

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ok", nil))
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Equal(t, ez.EINTERNAL, errorEnvelope(t, rec).Code)
	require.Contains(t, logs.String(), "middleware kaboom")
}
