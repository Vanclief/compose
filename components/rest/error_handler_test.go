package rest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"
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
	e.GET("/canceled", func(c *echo.Context) error {
		return context.Canceled
	})
	e.GET("/canceled-wrapped", func(c *echo.Context) error {
		return fmt.Errorf("secret query: %w", context.Canceled)
	})
	e.GET("/deadline", func(c *echo.Context) error {
		return context.DeadlineExceeded
	})
	e.GET("/ez", func(c *echo.Context) error {
		return ez.New(ez.ECONFLICT, "already running", errors.New("secret cause"))
	})
	e.GET("/ez-wrapped", func(c *echo.Context) error {
		return ez.Wrap(ez.New(ez.ENOTFOUND, "no such run", nil))
	})
	e.GET("/ez-in-other", func(c *echo.Context) error {
		return fmt.Errorf("save: %w", ez.New(ez.EINVALID, "secret detail", nil))
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
			name:   "canceled context",
			method: http.MethodGet, path: "/canceled",
			status: ez.StatusClientClosedRequest,
			want:   handler.StandardError{Code: ez.ECANCELED, Message: "The operation was canceled."},
		},
		{
			name:   "wrapped canceled context",
			method: http.MethodGet, path: "/canceled-wrapped",
			status: ez.StatusClientClosedRequest,
			want:   handler.StandardError{Code: ez.ECANCELED, Message: "The operation was canceled."},
		},
		{
			name:   "deadline exceeded",
			method: http.MethodGet, path: "/deadline",
			status: http.StatusGatewayTimeout,
			want:   handler.StandardError{Code: ez.ETIMEOUT, Message: "The operation timed out."},
		},
		{
			name:   "ez error",
			method: http.MethodGet, path: "/ez",
			status: http.StatusConflict,
			want:   handler.StandardError{Code: ez.ECONFLICT, Message: "already running"},
		},
		{
			name:   "wrapped ez error",
			method: http.MethodGet, path: "/ez-wrapped",
			status: http.StatusNotFound,
			want:   handler.StandardError{Code: ez.ENOTFOUND, Message: "no such run"},
		},
		{
			name:   "ez error inside another error",
			method: http.MethodGet, path: "/ez-in-other",
			status: http.StatusInternalServerError,
			want:   handler.StandardError{Code: ez.EINTERNAL, Message: "An internal error has occurred. Please contact technical support."},
		},
		{
			name:   "request id of the client is not used",
			method: http.MethodGet, path: "/nope", requestID: "request-id",
			status: http.StatusNotFound,
			want:   handler.StandardError{Code: ez.ENOTFOUND, Message: "Not Found"},
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

// reported records the errors a WithErrorReporter hook was called with.
type reported struct {
	errs []error
}

func (r *reported) report(c *echo.Context, err error) {
	r.errs = append(r.errs, err)
}

func TestErrorHandlerReporter(t *testing.T) {
	invalid := ez.New(ez.EINVALID, "bad name", nil)
	internal := ez.New(ez.EINTERNAL, "database down", nil)
	plain := errors.New("secret database detail")

	tests := []struct {
		name   string
		err    error
		status int
	}{
		{"client ez error", invalid, http.StatusBadRequest},
		{"server ez error", internal, http.StatusInternalServerError},
		{"plain error", plain, http.StatusInternalServerError},
		{"echo error", echo.ErrForbidden, http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var r reported
			e := echo.New()
			e.HTTPErrorHandler = ErrorHandler(WithErrorReporter(r.report))
			e.GET("/fail", func(c *echo.Context) error {
				return tt.err
			})

			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/fail", nil))
			require.Equal(t, tt.status, rec.Code)
			require.Len(t, r.errs, 1)
			require.Same(t, tt.err, r.errs[0])
		})
	}
}

// Under the request logger, which answers the error and returns it, Echo
// calls the error handler a second time: the second call does nothing.
func TestErrorHandlerRunsOnceUnderRequestLogger(t *testing.T) {
	var r reported
	translations := 0
	translate := func(c *echo.Context, err error) error {
		translations++
		return nil
	}
	var logs bytes.Buffer
	e := echo.New()
	e.HTTPErrorHandler = ErrorHandler(WithErrorReporter(r.report), WithErrorTranslator(translate))
	e.Use(middleware.RequestLoggerWithConfig(requestLogger(zerolog.New(&logs))))
	failure := ez.New(ez.EINVALID, "bad name", nil)
	e.GET("/fail", func(c *echo.Context) error {
		return failure
	})

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/fail", nil))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, handler.StandardError{Code: ez.EINVALID, Message: "bad name"}, errorEnvelope(t, rec))
	require.Len(t, r.errs, 1)
	require.Same(t, failure, r.errs[0])
	require.Equal(t, 1, translations)
	require.Contains(t, logs.String(), `"status":400`)
}

func TestErrorHandlerTranslator(t *testing.T) {
	var seen []error
	translate := func(c *echo.Context, err error) error {
		seen = append(seen, err)
		if ez.ErrorMessage(err) == "keep" {
			return nil
		}
		return ez.New(ez.EINTERNAL, "traducido: "+ez.ErrorMessage(err), err)
	}
	e := echo.New()
	e.HTTPErrorHandler = ErrorHandler(WithErrorTranslator(translate))
	conflict := ez.New(ez.ECONFLICT, "busy", nil)
	e.GET("/translated", func(c *echo.Context) error {
		return conflict
	})
	e.GET("/kept", func(c *echo.Context) error {
		return ez.New(ez.ENOTFOUND, "keep", nil)
	})
	e.GET("/echo", func(c *echo.Context) error {
		return echo.NewHTTPError(http.StatusTeapot, "no coffee")
	})

	// The message is translated; the code and status stay the original's.
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/translated", nil))
	require.Equal(t, http.StatusConflict, rec.Code)
	require.Equal(t, handler.StandardError{Code: ez.ECONFLICT, Message: "traducido: busy"}, errorEnvelope(t, rec))
	require.Len(t, seen, 1)
	require.Same(t, conflict, seen[0])

	// A nil translation keeps the original message.
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/kept", nil))
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Equal(t, handler.StandardError{Code: ez.ENOTFOUND, Message: "keep"}, errorEnvelope(t, rec))
	require.Len(t, seen, 2)

	// Errors that are not ez errors are not translated.
	for _, path := range []string{"/echo", "/nope"} {
		rec = httptest.NewRecorder()
		e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		require.NotContains(t, rec.Body.String(), "traducido", path)
	}
	require.Len(t, seen, 2)
}

// requirePanicsInvalid asserts fn panics with an ez EINVALID error with
// message want.
func requirePanicsInvalid(t *testing.T, want string, fn func()) {
	t.Helper()
	defer func() {
		err, ok := recover().(error)
		require.True(t, ok, "no error panic")
		require.Equal(t, ez.EINVALID, ez.ErrorCode(err))
		require.Equal(t, want, ez.ErrorMessage(err))
	}()
	fn()
}

func TestErrorHandlerOptionsNeedFunctions(t *testing.T) {
	requirePanicsInvalid(t, "rest: WithErrorReporter needs a function", func() {
		WithErrorReporter(nil)
	})
	requirePanicsInvalid(t, "rest: WithErrorTranslator needs a function", func() {
		WithErrorTranslator(nil)
	})
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
			require.NoError(t, uuid.Validate(id))
			require.NotContains(t, rec.Body.String(), "client-1")
			require.NotContains(t, logs.String(), "client-1")
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
