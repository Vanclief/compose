package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"
	"github.com/vanclief/compose/components/rest/requests"
	"github.com/vanclief/compose/primitives/enums"
	"github.com/vanclief/ez"
)

type ageBody struct {
	Age int `json:"age"`
}

func (b *ageBody) Validate() error { return nil }

func TestBindedJSONResponseHidesBindDetail(t *testing.T) {
	var buf bytes.Buffer
	previous := log.Logger
	log.Logger = zerolog.New(&buf)
	t.Cleanup(func() { log.Logger = previous })

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"age":"forty"}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)

	request := requests.New(http.Header{}, "127.0.0.1")

	err := NewHandler(nil).BindedJSONResponse(c, "test", request, &ageBody{})
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	var response ErrorResponse
	err = json.Unmarshal(rec.Body.Bytes(), &response)
	require.NoError(t, err)
	require.Equal(t, ez.EINVALID, response.Error.Code)
	require.Equal(t, "Invalid request body", response.Error.Message)

	// encoding/json's detail names Go types and fields; it belongs in the log, not the response
	require.NotContains(t, rec.Body.String(), "of type int")
	require.NotContains(t, rec.Body.String(), "ageBody")

	output := buf.String()
	require.Contains(t, output, "Request Bind Error")
	require.Contains(t, output, "field ageBody.age of type int")
	require.Contains(t, output, request.GetID())
}

type color string

var colors = map[color]struct{}{"blue": {}, "red": {}}

func (c *color) UnmarshalJSON(b []byte) error {
	return enums.Unmarshal(b, c, colors)
}

type colorBody struct {
	Color color `json:"color"`
}

func (b *colorBody) Validate() error { return nil }

func TestBindedJSONResponseKeepsAppErrorMessage(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"color":"green"}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)

	request := requests.New(http.Header{}, "127.0.0.1")

	err := NewHandler(nil).BindedJSONResponse(c, "test", request, &colorBody{})
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	var response ErrorResponse
	err = json.Unmarshal(rec.Body.Bytes(), &response)
	require.NoError(t, err)
	require.Equal(t, ez.EINVALID, response.Error.Code)

	// The app wrote this message for the client, without the op prefix ez adds to Error()
	require.Contains(t, response.Error.Message, "should be one of")
	require.Contains(t, response.Error.Message, "green")
	require.NotContains(t, response.Error.Message, "enums.Unmarshal")
	require.NotEqual(t, "Invalid request body", response.Error.Message)
}

func TestBindedJSONResponseLogsWrongTypedEnumField(t *testing.T) {
	var buf bytes.Buffer
	previous := log.Logger
	log.Logger = zerolog.New(&buf)
	t.Cleanup(func() { log.Logger = previous })

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"color":42}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)

	request := requests.New(http.Header{}, "127.0.0.1")

	err := NewHandler(nil).BindedJSONResponse(c, "test", request, &colorBody{})
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	var response ErrorResponse
	err = json.Unmarshal(rec.Body.Bytes(), &response)
	require.NoError(t, err)
	require.Equal(t, ez.EINVALID, response.Error.Code)
	require.Equal(t, "Invalid request body", response.Error.Message)

	// enums.Unmarshal returns the json error bare, so the log holds encoding/json's own words,
	// which name the field on encoding/json's own implementation and not on encoding/json/v2
	output := buf.String()
	require.Contains(t, output, "Request Bind Error")
	err = json.Unmarshal([]byte(`{"color":42}`), &colorBody{})
	require.Error(t, err)
	logged, err := json.Marshal(err.Error())
	require.NoError(t, err)
	require.Contains(t, output, `"bind_error":`+string(logged))
}

// email mirrors emma's types.Email: a translation key with the value as data,
// and a wrapped json error for a wrong-typed value.
type email string

func (e *email) UnmarshalJSON(b []byte) error {
	var value string
	err := json.Unmarshal(b, &value)
	if err != nil {
		return ez.Wrap(err)
	}

	if !strings.Contains(value, "@") {
		return ez.New(ez.EINVALID, "errors.shared.invalid_email", nil).AddData("email", value)
	}

	*e = email(value)
	return nil
}

type emailBody struct {
	Email email `json:"email"`
}

func (b *emailBody) Validate() error { return nil }

func TestBindedJSONResponseKeepsAppErrorData(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"email":"not-an-email"}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)

	request := requests.New(http.Header{}, "127.0.0.1")

	handler := NewHandler(nil)
	handler.ErrorTranslator = func(err error, request requests.Request) error {
		require.Equal(t, ez.EINVALID, ez.ErrorCode(err))
		require.Equal(t, "errors.shared.invalid_email", ez.ErrorMessage(err))
		require.Equal(t, "not-an-email", ez.ErrorData(err)["email"])
		return ez.New(ez.EINVALID, "not-an-email is not a valid email", err)
	}

	err := handler.BindedJSONResponse(c, "test", request, &emailBody{})
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	var response ErrorResponse
	err = json.Unmarshal(rec.Body.Bytes(), &response)
	require.NoError(t, err)
	require.Equal(t, ez.EINVALID, response.Error.Code)
	require.Equal(t, "not-an-email is not a valid email", response.Error.Message)
}

// Echo v5 decodes with json.Unmarshal, so data after the value is refused
// where v4's streaming decoder bound the first value.
func TestBindedJSONResponseRefusesTrailingData(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"age":1} {"age":2}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)

	err := NewHandler(nil).BindedJSONResponse(c, "test", requests.New(http.Header{}, "127.0.0.1"), &ageBody{})
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	var response ErrorResponse
	err = json.Unmarshal(rec.Body.Bytes(), &response)
	require.NoError(t, err)
	require.Equal(t, ez.EINVALID, response.Error.Code)
	require.Equal(t, "Invalid request body", response.Error.Message)
}

func TestBindedJSONResponseHidesWrappedJSONError(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"email":5}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)

	request := requests.New(http.Header{}, "127.0.0.1")

	err := NewHandler(nil).BindedJSONResponse(c, "test", request, &emailBody{})
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	var response ErrorResponse
	err = json.Unmarshal(rec.Body.Bytes(), &response)
	require.NoError(t, err)
	require.Equal(t, ez.EINVALID, response.Error.Code)

	// ez.Wrap of a json error carries EINTERNAL and no message; neither belongs in a 400
	require.Equal(t, "Invalid request body", response.Error.Message)
	require.NotContains(t, rec.Body.String(), "internal error")
}

// wrappingBinder wraps the default binder's errors, as an app's own Binder may.
type wrappingBinder struct {
	next echo.Binder
}

func (b wrappingBinder) Bind(c *echo.Context, target any) error {
	err := b.next.Bind(c, target)
	if err != nil {
		return fmt.Errorf("app binder: %w", err)
	}
	return nil
}

// errBinder fails every bind with err, as an app's own Binder may.
type errBinder struct {
	err error
}

func (b errBinder) Bind(c *echo.Context, target any) error {
	return b.err
}

func TestBindedJSONResponseFindsWrappedEchoCause(t *testing.T) {
	var buf bytes.Buffer
	previous := log.Logger
	log.Logger = zerolog.New(&buf)
	t.Cleanup(func() { log.Logger = previous })

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"age":"forty"}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	e := echo.New()
	e.Binder = wrappingBinder{next: e.Binder}
	c := e.NewContext(req, rec)

	err := NewHandler(nil).BindedJSONResponse(c, "test", requests.New(http.Header{}, "127.0.0.1"), &ageBody{})
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	var response ErrorResponse
	err = json.Unmarshal(rec.Body.Bytes(), &response)
	require.NoError(t, err)
	require.Equal(t, "Invalid request body", response.Error.Message)

	// The cause Echo wrapped is what gets logged, without the app binder's prefix
	require.Contains(t, buf.String(), `"bind_error":"json: cannot unmarshal`)
}

func TestBindedJSONResponseKeepsWrappedEchoMessage(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"age":40}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	e := echo.New()
	e.Binder = errBinder{err: fmt.Errorf("app binder: %w", echo.NewHTTPError(http.StatusBadRequest, "age is out of range"))}
	c := e.NewContext(req, rec)

	err := NewHandler(nil).BindedJSONResponse(c, "test", requests.New(http.Header{}, "127.0.0.1"), &ageBody{})
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	var response ErrorResponse
	err = json.Unmarshal(rec.Body.Bytes(), &response)
	require.NoError(t, err)
	require.Equal(t, ez.EINVALID, response.Error.Code)
	require.Equal(t, "age is out of range", response.Error.Message)
}

func TestBindedJSONResponseLogsBindingError(t *testing.T) {
	var buf bytes.Buffer
	previous := log.Logger
	log.Logger = zerolog.New(&buf)
	t.Cleanup(func() { log.Logger = previous })

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"age":40}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	e := echo.New()
	e.Binder = errBinder{err: echo.NewBindingError("age", []string{"forty"}, "failed to bind", errors.New("not a number"))}
	c := e.NewContext(req, rec)

	err := NewHandler(nil).BindedJSONResponse(c, "test", requests.New(http.Header{}, "127.0.0.1"), &ageBody{})
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	var response ErrorResponse
	err = json.Unmarshal(rec.Body.Bytes(), &response)
	require.NoError(t, err)
	require.Equal(t, "Invalid request body", response.Error.Message)

	// Echo's value binders name the parameter; that detail stays in the log
	require.Contains(t, buf.String(), "Request Bind Error")
	require.Contains(t, buf.String(), "field=age")
}

func TestBindedJSONResponseKeepsEchoStatusText(t *testing.T) {
	tests := []struct {
		name       string
		req        func() *http.Request
		wrapBinder bool
		message    string
	}{
		{
			name: "unsupported media type",
			req: func() *http.Request {
				req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"age":40}`))
				req.Header.Set(echo.HeaderContentType, echo.MIMETextPlain)
				return req
			},
			message: http.StatusText(http.StatusUnsupportedMediaType),
		},
		{
			name: "body over the limit",
			req: func() *http.Request {
				body := `{"age":` + strings.Repeat(" ", 2048) + `40}`
				req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
				req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
				// An unknown length, as with a chunked body, gets past the
				// middleware's ContentLength check and trips the limit during bind
				req.ContentLength = -1
				return req
			},
			message: http.StatusText(http.StatusRequestEntityTooLarge),
		},
		{
			name: "unsupported media type wrapped by the app's binder",
			req: func() *http.Request {
				req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"age":40}`))
				req.Header.Set(echo.HeaderContentType, echo.MIMETextPlain)
				return req
			},
			wrapBinder: true,
			message:    http.StatusText(http.StatusUnsupportedMediaType),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := echo.New()
			if tt.wrapBinder {
				e.Binder = wrappingBinder{next: e.Binder}
			}
			e.Use(middleware.BodyLimit(1024))
			e.POST("/", func(c *echo.Context) error {
				request := requests.New(http.Header{}, "127.0.0.1")
				return NewHandler(nil).BindedJSONResponse(c, "test", request, &ageBody{})
			})

			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, tt.req())
			require.Equal(t, http.StatusBadRequest, rec.Code)

			var response ErrorResponse
			err := json.Unmarshal(rec.Body.Bytes(), &response)
			require.NoError(t, err)
			require.Equal(t, ez.EINVALID, response.Error.Code)

			// Echo's fixed status text names no Go types or fields, and tells the client what to fix
			require.Equal(t, tt.message, response.Error.Message)
		})
	}
}
