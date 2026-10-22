package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
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

	// Echo's detail names Go types and fields; it belongs in the log, not the response
	require.NotContains(t, rec.Body.String(), "expected=")
	require.NotContains(t, rec.Body.String(), "field=")

	output := buf.String()
	require.Contains(t, output, "Request Bind Error")
	require.Contains(t, output, "expected=int")
	require.Contains(t, output, "field=age")
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
