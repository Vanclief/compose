package rest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/labstack/echo/v5"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"
	"github.com/vanclief/compose/components/rest/handler"
	"github.com/vanclief/ez"
)

// newExposingEcho is an Echo whose error handler writes the wrapped cause of
// an error into the response, so a test can see which error a bind raised.
func newExposingEcho() *echo.Echo {
	e := echo.New()
	e.HTTPErrorHandler = echo.DefaultHTTPErrorHandler(true)
	return e
}

func newNULEcho() *echo.Echo {
	e := newExposingEcho()
	e.Use(RejectNUL())

	e.POST("/json", func(c *echo.Context) error {
		var body struct {
			Name string `json:"name"`
		}
		err := c.Bind(&body)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]string{"name": body.Name})
	})
	e.POST("/raw", func(c *echo.Context) error {
		body, err := io.ReadAll(c.Request().Body)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string][]byte{"body": body})
	})
	e.GET("/items/:id", func(c *echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"id": c.Param("id"), "q": c.QueryParam("q")})
	})
	e.POST("/form", func(c *echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"f": c.FormValue("f")})
	})
	e.POST("/form/bind", bindForm)
	e.POST("/stream", func(c *echo.Context) error {
		reader, err := c.Request().MultipartReader()
		if err != nil {
			return err
		}
		parts := map[string][]byte{}
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			content, err := io.ReadAll(part)
			if err != nil {
				return err
			}
			parts[part.FormName()] = content
		}
		return c.JSON(http.StatusOK, parts)
	})
	e.POST("/upload", func(c *echo.Context) error {
		header, err := c.FormFile("file")
		if err != nil {
			return err
		}
		file, err := header.Open()
		if err != nil {
			return err
		}
		defer file.Close()
		content, err := io.ReadAll(file)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]any{
			"f":        c.FormValue("f"),
			"filename": header.Filename,
			"content":  content,
		})
	})

	return e
}

func bindForm(c *echo.Context) error {
	var body struct {
		F string `form:"f"`
	}
	err := c.Bind(&body)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]string{"f": body.F})
}

// multipartBody builds a form with the field f and one file part with the
// given Content-Disposition, and returns its Content-Type and body.
func multipartBody(t *testing.T, field, disposition string, content []byte) (string, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	require.NoError(t, w.WriteField("f", field))

	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", disposition)
	header.Set("Content-Type", "application/octet-stream")
	part, err := w.CreatePart(header)
	require.NoError(t, err)
	_, err = part.Write(content)
	require.NoError(t, err)
	require.NoError(t, w.Close())

	return w.FormDataContentType(), buf.String()
}

func errorJSON(message string) string {
	body, err := json.Marshal(map[string]any{
		"error": map[string]string{"code": "invalid", "message": message, "request_id": ""},
	})
	if err != nil {
		panic(err)
	}
	return string(body)
}

func resultJSON(values map[string]any) string {
	body, err := json.Marshal(values)
	if err != nil {
		panic(err)
	}
	return string(body)
}

func TestRejectNUL(t *testing.T) {
	plainFile := `form-data; name="file"; filename="a.txt"`
	formType, formValueNUL := multipartBody(t, "a\x00b", plainFile, []byte("x"))
	nameType, fileNameNUL := multipartBody(t, "ok", `form-data; name="file"; filename*=UTF-8''a%00b.txt`, []byte("x"))
	fileType, fileContentNUL := multipartBody(t, "ok", plainFile, []byte{0, 1, 0})
	fieldNameType, fileFieldNameNUL := multipartBody(t, "ok", `form-data; name*=UTF-8''k%00k; filename="a.txt"`, []byte("x"))
	mixedCaseFormType := "Multipart/Form-Data" + strings.TrimPrefix(formType, echo.MIMEMultipartForm)
	attachmentType, attachmentNUL := multipartBody(t, "ok", `attachment; name="g"`, []byte("a\x00b"))
	_, otherBoundary := multipartBody(t, "ok", plainFile, []byte("x"))
	boundaryType := echo.MIMEMultipartForm + "; boundary=b"
	truncatedValue := "--b\r\nContent-Disposition: form-data; name=\"f\"\r\n\r\nabc"

	tests := []struct {
		name        string
		method      string
		target      string
		contentType string
		body        string
		chunked     bool
		wantStatus  int
		wantBody    string
	}{
		{
			name:        "JSON escape",
			method:      http.MethodPost,
			target:      "/json",
			contentType: echo.MIMEApplicationJSON,
			body:        `{"name":"a\u0000b"}`,
			wantStatus:  http.StatusBadRequest,
			wantBody:    errorJSON("Request body contains a NUL byte at byte 10"),
		},
		{
			name:        "JSON raw NUL byte",
			method:      http.MethodPost,
			target:      "/json",
			contentType: echo.MIMEApplicationJSON,
			body:        "{\"name\":\"a\x00b\"}",
			wantStatus:  http.StatusBadRequest,
			wantBody:    errorJSON("Request body contains a NUL byte at byte 10"),
		},
		{
			name:        "JSON with charset",
			method:      http.MethodPost,
			target:      "/json",
			contentType: "application/json; charset=utf-8",
			body:        `{"name":"\u0000"}`,
			wantStatus:  http.StatusBadRequest,
			wantBody:    errorJSON("Request body contains a NUL byte at byte 9"),
		},
		{
			// Echo trims the media type before matching it, so a space before
			// the parameter still binds as JSON
			name:        "JSON with space before the parameter",
			method:      http.MethodPost,
			target:      "/json",
			contentType: "application/json ; charset=utf-8",
			body:        `{"name":"\u0000"}`,
			wantStatus:  http.StatusBadRequest,
			wantBody:    errorJSON("Request body contains a NUL byte at byte 9"),
		},
		{
			name:        "chunked JSON",
			method:      http.MethodPost,
			target:      "/json",
			contentType: echo.MIMEApplicationJSON,
			body:        `{"name":"a\u0000b"}`,
			chunked:     true,
			wantStatus:  http.StatusBadRequest,
			wantBody:    errorJSON("Request body contains a NUL byte at byte 10"),
		},
		{
			name:       "query value",
			method:     http.MethodGet,
			target:     "/items/1?q=a%00b",
			wantStatus: http.StatusBadRequest,
			wantBody:   errorJSON("Query string contains a NUL byte"),
		},
		{
			name:       "query key",
			method:     http.MethodGet,
			target:     "/items/1?a%00b=1",
			wantStatus: http.StatusBadRequest,
			wantBody:   errorJSON("Query string contains a NUL byte"),
		},
		{
			name:       "path parameter",
			method:     http.MethodGet,
			target:     "/items/a%00b",
			wantStatus: http.StatusBadRequest,
			wantBody:   errorJSON("Path parameter id contains a NUL byte"),
		},
		{
			name:        "urlencoded value",
			method:      http.MethodPost,
			target:      "/form",
			contentType: echo.MIMEApplicationForm,
			body:        "f=a%00b",
			wantStatus:  http.StatusBadRequest,
			wantBody:    errorJSON("Form data contains a NUL byte"),
		},
		{
			name:        "urlencoded key",
			method:      http.MethodPost,
			target:      "/form",
			contentType: echo.MIMEApplicationForm,
			body:        "a%00b=1",
			wantStatus:  http.StatusBadRequest,
			wantBody:    errorJSON("Form data contains a NUL byte"),
		},
		{
			name:        "urlencoded value next to a malformed pair",
			method:      http.MethodPost,
			target:      "/form",
			contentType: echo.MIMEApplicationForm,
			body:        "g=%zz&f=a%00b",
			wantStatus:  http.StatusBadRequest,
			wantBody:    errorJSON("Form data contains a NUL byte"),
		},
		{
			name:        "multipart field value",
			method:      http.MethodPost,
			target:      "/upload",
			contentType: formType,
			body:        formValueNUL,
			wantStatus:  http.StatusBadRequest,
			wantBody:    errorJSON("Form data contains a NUL byte"),
		},
		{
			name:        "multipart file name",
			method:      http.MethodPost,
			target:      "/upload",
			contentType: nameType,
			body:        fileNameNUL,
			wantStatus:  http.StatusBadRequest,
			wantBody:    errorJSON("File name contains a NUL byte"),
		},
		{
			name:        "multipart file part field name",
			method:      http.MethodPost,
			target:      "/upload",
			contentType: fieldNameType,
			body:        fileFieldNameNUL,
			wantStatus:  http.StatusBadRequest,
			wantBody:    errorJSON("Form data contains a NUL byte"),
		},
		{
			name:        "mixed-case multipart field value",
			method:      http.MethodPost,
			target:      "/form",
			contentType: mixedCaseFormType,
			body:        formValueNUL,
			wantStatus:  http.StatusBadRequest,
			wantBody:    errorJSON("Form data contains a NUL byte"),
		},
		{
			name:        "mixed-case urlencoded value",
			method:      http.MethodPost,
			target:      "/form",
			contentType: "Application/X-WWW-Form-Urlencoded",
			body:        "f=a%00b",
			wantStatus:  http.StatusBadRequest,
			wantBody:    errorJSON("Form data contains a NUL byte"),
		},
		{
			name:        "multipart with another boundary",
			method:      http.MethodPost,
			target:      "/form/bind",
			contentType: formType,
			body:        otherBoundary,
			wantStatus:  http.StatusBadRequest,
			wantBody:    errorJSON("Form data could not be read"),
		},
		{
			name:        "multipart value part cut short",
			method:      http.MethodPost,
			target:      "/form/bind",
			contentType: boundaryType,
			body:        truncatedValue,
			wantStatus:  http.StatusBadRequest,
			wantBody:    errorJSON("Form data could not be read"),
		},
		{
			// ReadForm skips a part whose disposition is not form-data.
			name:        "multipart attachment part holding a NUL byte",
			method:      http.MethodPost,
			target:      "/form",
			contentType: attachmentType,
			body:        attachmentNUL,
			wantStatus:  http.StatusOK,
			wantBody:    resultJSON(map[string]any{"f": "ok"}),
		},
		{
			// net/http still parses the body when a parameter is malformed.
			name:        "urlencoded value with a malformed parameter",
			method:      http.MethodPost,
			target:      "/form",
			contentType: echo.MIMEApplicationForm + "; foo",
			body:        "f=a%00b",
			wantStatus:  http.StatusBadRequest,
			wantBody:    errorJSON("Form data contains a NUL byte"),
		},
		{
			// Echo's binder still binds JSON when mime.ParseMediaType fails.
			name:        "JSON with a duplicate parameter",
			method:      http.MethodPost,
			target:      "/json",
			contentType: echo.MIMEApplicationJSON + "; a=1; a=2",
			body:        `{"name":"\u0000"}`,
			wantStatus:  http.StatusBadRequest,
			wantBody:    errorJSON("Request body contains a NUL byte at byte 9"),
		},
		{
			name:        "+json media type",
			method:      http.MethodPost,
			target:      "/raw",
			contentType: "application/vnd.api+json",
			body:        `{"name":"\u0000"}`,
			wantStatus:  http.StatusBadRequest,
			wantBody:    errorJSON("Request body contains a NUL byte at byte 9"),
		},
		{
			name:        "JSON body read raw",
			method:      http.MethodPost,
			target:      "/raw",
			contentType: echo.MIMEApplicationJSON,
			body:        `{"name":"Ana","n":1}`,
			wantStatus:  http.StatusOK,
			wantBody:    resultJSON(map[string]any{"body": []byte(`{"name":"Ana","n":1}`)}),
		},
		{
			name:        "multipart streamed with MultipartReader",
			method:      http.MethodPost,
			target:      "/stream",
			contentType: fileType,
			body:        fileContentNUL,
			wantStatus:  http.StatusOK,
			wantBody:    resultJSON(map[string]any{"f": []byte("ok"), "file": []byte{0, 1, 0}}),
		},
		{
			name:        "JSON text of the escape",
			method:      http.MethodPost,
			target:      "/json",
			contentType: echo.MIMEApplicationJSON,
			body:        `{"name":"a\\u0000b"}`,
			wantStatus:  http.StatusOK,
			wantBody:    resultJSON(map[string]any{"name": `a\u0000b`}),
		},
		{
			name:        "JSON other control escape",
			method:      http.MethodPost,
			target:      "/json",
			contentType: echo.MIMEApplicationJSON,
			body:        `{"name":"\u0001"}`,
			wantStatus:  http.StatusOK,
			wantBody:    resultJSON(map[string]any{"name": "\x01"}),
		},
		{
			name:        "JSON two escaped backslashes before the text",
			method:      http.MethodPost,
			target:      "/json",
			contentType: echo.MIMEApplicationJSON,
			body:        `{"name":"a\\\\u0000"}`,
			wantStatus:  http.StatusOK,
			wantBody:    resultJSON(map[string]any{"name": `a\\u0000`}),
		},
		{
			name:        "JSON body still binds",
			method:      http.MethodPost,
			target:      "/json",
			contentType: echo.MIMEApplicationJSON,
			body:        `{"name":"Ana"}`,
			wantStatus:  http.StatusOK,
			wantBody:    resultJSON(map[string]any{"name": "Ana"}),
		},
		{
			name:        "chunked JSON body still binds",
			method:      http.MethodPost,
			target:      "/json",
			contentType: echo.MIMEApplicationJSON,
			body:        `{"name":"Ana"}`,
			chunked:     true,
			wantStatus:  http.StatusOK,
			wantBody:    resultJSON(map[string]any{"name": "Ana"}),
		},
		{
			name:       "path and query without NUL",
			method:     http.MethodGet,
			target:     "/items/a%20b?q=x",
			wantStatus: http.StatusOK,
			wantBody:   resultJSON(map[string]any{"id": "a b", "q": "x"}),
		},
		{
			name:        "urlencoded form still reads",
			method:      http.MethodPost,
			target:      "/form",
			contentType: echo.MIMEApplicationForm,
			body:        "f=a%20b",
			wantStatus:  http.StatusOK,
			wantBody:    resultJSON(map[string]any{"f": "a b"}),
		},
		{
			name:        "multipart file content holding NUL bytes",
			method:      http.MethodPost,
			target:      "/upload",
			contentType: fileType,
			body:        fileContentNUL,
			wantStatus:  http.StatusOK,
			wantBody:    resultJSON(map[string]any{"f": "ok", "filename": "a.txt", "content": []byte{0, 1, 0}}),
		},
		{
			name:        "XML body",
			method:      http.MethodPost,
			target:      "/raw",
			contentType: echo.MIMEApplicationXML,
			body:        `<name>\u0000</name>`,
			wantStatus:  http.StatusOK,
			wantBody:    resultJSON(map[string]any{"body": []byte(`<name>\u0000</name>`)}),
		},
		{
			name:        "octet-stream body",
			method:      http.MethodPost,
			target:      "/raw",
			contentType: echo.MIMEOctetStream,
			body:        "a\x00b",
			wantStatus:  http.StatusOK,
			wantBody:    resultJSON(map[string]any{"body": []byte("a\x00b")}),
		},
		{
			name:       "GET without a body",
			method:     http.MethodGet,
			target:     "/items/1",
			wantStatus: http.StatusOK,
			wantBody:   resultJSON(map[string]any{"id": "1", "q": ""}),
		},
	}

	e := newNULEcho()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var body io.Reader
			if tt.body != "" {
				body = strings.NewReader(tt.body)
			}
			req := httptest.NewRequest(tt.method, tt.target, body)
			if tt.contentType != "" {
				req.Header.Set(echo.HeaderContentType, tt.contentType)
			}
			if tt.chunked {
				req.ContentLength = -1
			}
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)

			require.Equal(t, tt.wantStatus, rec.Code, rec.Body.String())
			require.JSONEq(t, tt.wantBody, rec.Body.String())
		})
	}
}

// A form whose urlencoded body or multipart boundary is malformed is left for
// the handler: its bind reports the same error it reports without the
// middleware.
func TestRejectNULLeavesMalformedFormToHandler(t *testing.T) {
	withNUL := newNULEcho()
	without := newExposingEcho()
	without.POST("/form/bind", bindForm)

	_, body := multipartBody(t, "ok", `form-data; name="file"; filename="a.txt"`, []byte("x"))

	tests := []struct {
		name        string
		contentType string
		body        string
		wantError   string
	}{
		{
			name:        "urlencoded escape",
			contentType: echo.MIMEApplicationForm,
			body:        "f=%zz",
			wantError:   "invalid URL escape",
		},
		{
			name:        "multipart without a boundary",
			contentType: echo.MIMEMultipartForm,
			body:        body,
			wantError:   "no multipart boundary",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var recs []*httptest.ResponseRecorder
			for _, e := range []*echo.Echo{withNUL, without} {
				req := httptest.NewRequest(http.MethodPost, "/form/bind", strings.NewReader(tt.body))
				req.Header.Set(echo.HeaderContentType, tt.contentType)
				rec := httptest.NewRecorder()
				e.ServeHTTP(rec, req)
				recs = append(recs, rec)
			}

			require.Equal(t, http.StatusBadRequest, recs[0].Code, recs[0].Body.String())
			require.Contains(t, recs[0].Body.String(), tt.wantError)
			require.Equal(t, recs[1].Code, recs[0].Code)
			require.Equal(t, recs[1].Body.String(), recs[0].Body.String())
		})
	}
}

// fieldsThenNUL builds a form of n fields p and then a field f holding a NUL
// byte, and returns its Content-Type and body.
func fieldsThenNUL(t *testing.T, n int) (string, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for range n {
		require.NoError(t, w.WriteField("p", "x"))
	}
	require.NoError(t, w.WriteField("f", "a\x00b"))
	require.NoError(t, w.Close())
	return w.FormDataContentType(), buf.String()
}

// The walk stops after ReadForm's 1000 parts: a NUL byte in part 1001 is left
// for the handler, whose form parsing refuses the body as it would without
// the middleware, while one in part 1000 is refused.
func TestRejectNULStopsAtMultipartPartLimit(t *testing.T) {
	t.Run("NUL byte in part 1001", func(t *testing.T) {
		without := newExposingEcho()
		without.POST("/form/bind", bindForm)

		contentType, body := fieldsThenNUL(t, 1000)
		var recs []*httptest.ResponseRecorder
		for _, e := range []*echo.Echo{newNULEcho(), without} {
			req := httptest.NewRequest(http.MethodPost, "/form/bind", strings.NewReader(body))
			req.Header.Set(echo.HeaderContentType, contentType)
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)
			recs = append(recs, rec)
		}

		require.Equal(t, http.StatusBadRequest, recs[0].Code, recs[0].Body.String())
		require.Contains(t, recs[0].Body.String(), multipart.ErrMessageTooLarge.Error())
		require.Equal(t, recs[1].Code, recs[0].Code)
		require.Equal(t, recs[1].Body.String(), recs[0].Body.String())
	})

	t.Run("NUL byte in part 1000", func(t *testing.T) {
		contentType, body := fieldsThenNUL(t, 999)
		req := httptest.NewRequest(http.MethodPost, "/form/bind", strings.NewReader(body))
		req.Header.Set(echo.HeaderContentType, contentType)
		rec := httptest.NewRecorder()
		newNULEcho().ServeHTTP(rec, req)

		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		require.JSONEq(t, errorJSON("Form data contains a NUL byte"), rec.Body.String())
	})
}

// multipart.Reader.NextPart caps a part header at 10 MB, while the handler's
// ReadForm allows 32 MB plus 10 MB, so without the middleware the handler
// reads the field after an 11 MB header. The walk refuses the body it cannot
// read rather than let that field through.
func TestRejectNULRefusesPartHeaderNextPartCannotRead(t *testing.T) {
	body := "--b\r\n" +
		"X-Pad: " + strings.Repeat("a", 11<<20) + "\r\n" +
		"Content-Disposition: form-data; name=\"f\"\r\n" +
		"\r\n" +
		"a\x00b\r\n" +
		"--b--\r\n"

	without := newExposingEcho()
	without.POST("/form", func(c *echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"f": c.FormValue("f")})
	})

	var recs []*httptest.ResponseRecorder
	for _, e := range []*echo.Echo{newNULEcho(), without} {
		req := httptest.NewRequest(http.MethodPost, "/form", strings.NewReader(body))
		req.Header.Set(echo.HeaderContentType, echo.MIMEMultipartForm+"; boundary=b")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		recs = append(recs, rec)
	}

	require.Equal(t, http.StatusOK, recs[1].Code, recs[1].Body.String())
	require.JSONEq(t, resultJSON(map[string]any{"f": "a\x00b"}), recs[1].Body.String())
	require.Equal(t, http.StatusBadRequest, recs[0].Code, recs[0].Body.String())
	require.JSONEq(t, errorJSON("Form data could not be read"), recs[0].Body.String())
}

// An empty multipart body passes to the handler, which answers as it would
// without the middleware: Echo's Bind skips it and FormValue reads no values.
func TestRejectNULPassesEmptyMultipartBody(t *testing.T) {
	without := newExposingEcho()
	without.POST("/form", func(c *echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"f": c.FormValue("f")})
	})
	without.POST("/form/bind", bindForm)

	for _, target := range []string{"/form", "/form/bind"} {
		t.Run(target, func(t *testing.T) {
			var recs []*httptest.ResponseRecorder
			for _, e := range []*echo.Echo{newNULEcho(), without} {
				req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(""))
				req.Header.Set(echo.HeaderContentType, echo.MIMEMultipartForm+"; boundary=b")
				rec := httptest.NewRecorder()
				e.ServeHTTP(rec, req)
				recs = append(recs, rec)
			}

			require.Equal(t, http.StatusOK, recs[0].Code, recs[0].Body.String())
			require.JSONEq(t, resultJSON(map[string]any{"f": ""}), recs[0].Body.String())
			require.Equal(t, recs[1].Code, recs[0].Code)
			require.Equal(t, recs[1].Body.String(), recs[0].Body.String())
		})
	}
}

// countingReader counts the bytes read from it.
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// A body larger than maxInspectedBody reaches the handler uninspected and
// whole, NUL escapes and all; one whose Content-Length says so is not read by
// the middleware at all.
func TestRejectNULPassesBodyOverInspectionCap(t *testing.T) {
	body := []byte(`{"name":"\u0000` + strings.Repeat("a", maxInspectedBody) + `\u0000"}`)
	sum := sha256.Sum256(body)
	wantHash := hex.EncodeToString(sum[:])

	tests := []struct {
		name          string
		contentLength int64
	}{
		{name: "announced length", contentLength: int64(len(body))},
		{name: "chunked", contentLength: -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			counter := &countingReader{r: bytes.NewReader(body)}
			readBefore := -1

			e := newExposingEcho()
			e.Use(RejectNUL())
			e.POST("/hash", func(c *echo.Context) error {
				readBefore = counter.n
				received, err := io.ReadAll(c.Request().Body)
				if err != nil {
					return err
				}
				sum := sha256.Sum256(received)
				return c.String(http.StatusOK, hex.EncodeToString(sum[:]))
			})

			req := httptest.NewRequest(http.MethodPost, "/hash", counter)
			req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			req.ContentLength = tt.contentLength
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)

			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.Equal(t, wantHash, rec.Body.String())
			if tt.contentLength > 0 {
				require.Zero(t, readBefore)
			}
		})
	}
}

// A body of exactly maxInspectedBody is still inspected, whether its
// Content-Length announces it or it is chunked.
func TestRejectNULInspectsBodyAtCap(t *testing.T) {
	prefix := `{"name":"\u0000`
	body := prefix + strings.Repeat("a", maxInspectedBody-len(prefix))

	tests := []struct {
		name          string
		contentLength int64
	}{
		{name: "announced length", contentLength: maxInspectedBody},
		{name: "chunked", contentLength: -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/json", strings.NewReader(body))
			req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			req.ContentLength = tt.contentLength
			rec := httptest.NewRecorder()
			newNULEcho().ServeHTTP(rec, req)

			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			require.JSONEq(t, errorJSON("Request body contains a NUL byte at byte 9"), rec.Body.String())
		})
	}
}

func TestRejectNULBodyReadError(t *testing.T) {
	tests := []struct {
		name        string
		target      string
		contentType string
	}{
		{name: "JSON", target: "/json", contentType: echo.MIMEApplicationJSON},
		{name: "multipart", target: "/form/bind", contentType: echo.MIMEMultipartForm + "; boundary=b"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tt.target, iotest.ErrReader(errors.New("connection reset")))
			req.Header.Set(echo.HeaderContentType, tt.contentType)
			req.ContentLength = -1
			rec := httptest.NewRecorder()
			newNULEcho().ServeHTTP(rec, req)

			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			require.JSONEq(t, errorJSON("Request body could not be read"), rec.Body.String())
		})
	}
}

// A refusal is logged with the route template, never the client's URI.
func TestRejectNULLogsRefusal(t *testing.T) {
	var buf bytes.Buffer
	previous := log.Logger
	log.Logger = zerolog.New(&buf)
	t.Cleanup(func() { log.Logger = previous })

	req := httptest.NewRequest(http.MethodGet, "/items/1?q=secret%00", nil)
	rec := httptest.NewRecorder()
	newNULEcho().ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	var entry map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &entry), buf.String())
	require.Equal(t, map[string]any{
		"level":         "warn",
		"method":        http.MethodGet,
		"path":          "/items/:id",
		"error_code":    "invalid",
		"error_message": "Query string contains a NUL byte",
		"message":       "Request Error",
	}, entry)
}

// Under Configure the refusal carries the request id the response and the
// request log line carry.
func TestConfigureRejectsNUL(t *testing.T) {
	var buf bytes.Buffer
	previous := log.Logger
	log.Logger = zerolog.New(&buf)
	t.Cleanup(func() { log.Logger = previous })

	e := newTestEcho()
	e.GET("/get", func(c *echo.Context) error {
		return c.NoContent(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/get?q=%00", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	id := rec.Header().Get(echo.HeaderXRequestID)
	require.NotEmpty(t, id)
	want, err := json.Marshal(handler.ErrorResponse{Error: handler.StandardError{
		Code:      ez.EINVALID,
		Message:   "Query string contains a NUL byte",
		RequestID: id,
	}})
	require.NoError(t, err)
	require.JSONEq(t, string(want), rec.Body.String())

	var entry map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &entry), buf.String())
	require.Equal(t, "warn", entry["level"])
	require.Equal(t, id, entry["id"])

	// A client's id is reused
	req = httptest.NewRequest(http.MethodGet, "/get?q=%00", nil)
	req.Header.Set(echo.HeaderXRequestID, "client-id")
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), `"request_id":"client-id"`)
}

// Configure registers the body limit before RejectNUL, so RejectNUL's own
// read of a chunked body over the limit fails with Echo's 413, even when the
// body holds a NUL byte before the limit. Were the order swapped, RejectNUL
// would read the whole body and refuse that NUL byte with 400. The handler
// does not read the body, so the 413 can only come from RejectNUL's read.
func TestConfigureRejectNULKeepsBodyLimit(t *testing.T) {
	e := newTestEcho(WithBodyLimit(1024))
	e.POST("/ignore", func(c *echo.Context) error {
		return c.NoContent(http.StatusNoContent)
	})
	// An app handler classifying by ez code must get Echo's own error, not
	// the ez wrapper RejectNUL read the body with
	e.HTTPErrorHandler = func(c *echo.Context, err error) {
		var appErr *ez.Error
		if errors.As(err, &appErr) {
			_ = c.NoContent(http.StatusBadRequest)
			return
		}
		_, status := echo.ResolveResponseStatus(c.Response(), err)
		_ = c.NoContent(status)
	}

	formType, formBody := multipartBody(t, strings.Repeat("a", 2048), `form-data; name="file"; filename="a.txt"`, []byte("x"))

	tests := []struct {
		name        string
		contentType string
		body        string
	}{
		{name: "JSON without NUL", contentType: echo.MIMEApplicationJSON, body: `{"name":"` + strings.Repeat("a", 2048) + `"}`},
		{name: "JSON NUL before the limit", contentType: echo.MIMEApplicationJSON, body: `{"name":"\u0000` + strings.Repeat("a", 2048) + `"}`},
		{name: "multipart", contentType: formType, body: formBody},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/ignore", strings.NewReader(tt.body))
			req.Header.Set(echo.HeaderContentType, tt.contentType)
			req.ContentLength = -1
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)
			require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
		})
	}
}

func TestNulOffset(t *testing.T) {
	tests := []struct {
		body   string
		offset int
		found  bool
	}{
		{body: ``, found: false},
		{body: `{"a":"b"}`, found: false},
		{body: `\u0000`, offset: 0, found: true},
		{body: `\\u0000`, found: false},
		{body: `\\\u0000`, offset: 2, found: true},
		{body: `\\\\u0000`, found: false},
		{body: `\U0000`, found: false},
		{body: `\u00000`, offset: 0, found: true},
		{body: `\u0001`, found: false},
		{body: `\u000`, found: false},
		{body: `\\u0000 \u0000`, offset: 8, found: true},
		{body: "a\x00 \\u0000", offset: 1, found: true},
		{body: "\\u0000 \x00", offset: 0, found: true},
		{body: "\\\\u0000 \x00", offset: 8, found: true},
	}

	for _, tt := range tests {
		offset, found := nulOffset([]byte(tt.body))
		require.Equal(t, tt.found, found, tt.body)
		if tt.found {
			require.Equal(t, tt.offset, offset, tt.body)
		}
	}
}
