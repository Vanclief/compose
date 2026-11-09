package rest

import (
	"bytes"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/labstack/echo/v5"
	"github.com/rs/zerolog/log"
	"github.com/vanclief/compose/components/rest/handler"
	"github.com/vanclief/ez"
)

// nulEscape is the JSON escape that decodes to a NUL byte. JSON only allows a
// lowercase u.
var nulEscape = []byte(`\u0000`)

// maxMultipartParts mirrors the part limit of mime/multipart's ReadForm, 1000
// by default and set with GODEBUG multipartmaxparts, which this does not read.
const maxMultipartParts = 1000

// maxInspectedBody is the largest body RejectNUL reads to inspect: 32 MB, the
// memory limit net/http's FormValue and Echo's FormFile and MultipartForm give
// mime/multipart's ReadForm, which keeps that much of a form in memory and
// spools the rest of its files to disk. A larger body is passed to the handler
// uninspected.
const maxInspectedBody = 32 << 20

// RejectNUL refuses, with 400 ez.EINVALID and before any handler runs, a
// request that carries a NUL byte (0x00) where a handler would read it.
// Postgres cannot store a NUL byte in text, varchar or jsonb, and bun v1.3.0
// refuses to render a string holding one into SQL, so such a string would fail
// its query with a 500 (ez.EINTERNAL); no legitimate client sends one.
//
// It checks four places, in order:
//   - the path parameters, as c.Param returns them;
//   - the keys and values of the query string;
//   - a JSON body (media type application/json, or one ending in +json): a
//     raw NUL byte, or the escape \u0000 when its backslash is not itself
//     escaped. The six-character text \u0000, written "\\u0000" in JSON,
//     passes;
//   - a form body (application/x-www-form-urlencoded or multipart/form-data):
//     the keys and values of its fields and the names of its files. File
//     contents are not inspected, since files such as images hold NUL bytes.
//
// Media types match in any letter case, as net/http's form parsing does.
// The body is chosen by Content-Type, not by how the handler reads it, so a
// JSON body a handler reads raw, such as a webhook whose signature it checks
// over the bytes, is scanned and refused before the handler runs. An app that
// must accept such a payload on a route exempts the route by wrapping the
// middleware with a check on c.Path(), or by registering it on route groups
// instead of with e.Use. XML bodies are not checked, since encoding/xml
// already rejects a NUL byte, and nor are other content types. Headers need
// no check: net/http refuses a header holding a NUL byte before Echo runs.
//
// It reads a JSON, urlencoded or multipart body of up to 32 MB into memory
// (two to three times that while reading and parsing) and puts it back, so
// handlers can still bind, read or stream it; a larger body passes through
// uninspected and intact, so it holds at most 32 MB per request whatever the
// body limit.
// Registered with e.Use it runs on every route, matched or not. Register it
// after CORS, so a preflight is answered first and its 400 carries the CORS
// headers; after any request-decompression middleware, since compressed bytes
// hold NUL bytes; and after a body limit such as middleware.BodyLimit, so a
// body over the limit gets the limit's 413 without being read first. A
// chunked body over the limit is read up to the limit, and the limit's error
// then goes to Echo's HTTPErrorHandler as Echo's own error, which Echo's
// default handler answers with its status.
//
// A refusal is logged at warn level as "Request Error" with the method, the
// route path and the request id, never the URI, query or body. The middleware
// writes the 400 itself, in the handler.ErrorResponse shape, so neither an app
// ErrorTranslator nor Echo's HTTPErrorHandler sees it. Its request_id, and the
// id of the log line, is the X-Request-Id of the response, as ErrorHandler
// has it: under Configure the request's id, otherwise what the app's own
// middleware set there; the request's X-Request-Id is never read. It is
// empty, and the log line has no id, when nothing set that header, as when
// RejectNUL is used without Configure and without a request ID middleware.
func RejectNUL() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			err := findNUL(c)
			if err == nil {
				return next(c)
			}

			// A body read can fail with Echo's own error, such as BodyLimit's
			// 413 for a chunked body. That error itself goes to the error
			// handler, not the ez wrapper, so an app handler classifying by
			// ez code does not answer it as a 400.
			var statusErr statusError
			if errors.As(err, &statusErr) {
				return statusErr
			}

			// c.Path() is the route template, not the client's URI.
			code := ez.ErrorCode(err)
			message := ez.ErrorMessage(err)
			id := requestID(c)
			evt := log.Warn()
			if id != "" {
				evt = evt.Str("id", id)
			}
			evt.Str("method", c.Request().Method).
				Str("path", c.Path()).
				Str("error_code", code).
				Str("error_message", message).
				Msg("Request Error")

			stdErr := handler.StandardError{Code: code, Message: message, RequestID: id}
			return c.JSON(ez.ErrorToHTTPStatus(err), handler.ErrorResponse{Error: stdErr})
		}
	}
}

// statusError is an error Echo answers with the status it carries, as
// *echo.HTTPError and Echo's predefined errors do. echo.HTTPStatusCoder is not
// an error, so it cannot be returned as one.
type statusError interface {
	error
	StatusCode() int
}

// findNUL returns an ez.EINVALID error naming where the request carries a NUL
// byte, or nil when it carries none. A body larger than maxInspectedBody is not
// inspected. A body that cannot be read is an ez.EINVALID error wrapping the
// read error.
func findNUL(c *echo.Context) error {
	for _, param := range c.PathValues() {
		if strings.IndexByte(param.Value, 0) >= 0 {
			return ez.New(ez.EINVALID, "Path parameter "+param.Name+" contains a NUL byte", nil)
		}
	}

	// Query keys are client data of any length, so the message does not echo
	// them.
	if valuesHoldNUL(c.QueryParams()) {
		return ez.New(ez.EINVALID, "Query string contains a NUL byte", nil)
	}

	req := c.Request()
	ctype := req.Header.Get(echo.HeaderContentType)

	// The media type is taken the way Echo's binder and net/http's form
	// parsing take it: the text before the first ';', trimmed, and lowercased
	// as net/http does (Echo's binder matches case-sensitively, so a
	// mixed-case JSON type is checked though Echo would not bind it).
	// mime.ParseMediaType is not used for it, since it fails on a malformed
	// parameter, such as "; foo", while net/http still parses an urlencoded
	// body and Echo still binds a JSON one.
	base, _, _ := strings.Cut(ctype, ";")
	mediaType := strings.ToLower(strings.TrimSpace(base))

	switch {
	case mediaType == echo.MIMEApplicationJSON || strings.HasSuffix(mediaType, "+json"):
		body, inspected, err := readBody(req)
		if err != nil {
			return ez.Wrap(err)
		}
		if !inspected {
			return nil
		}
		offset, found := nulOffset(body)
		if found {
			return ez.New(ez.EINVALID, "Request body contains a NUL byte at byte "+strconv.Itoa(offset), nil)
		}

	case mediaType == echo.MIMEApplicationForm:
		body, inspected, err := readBody(req)
		if err != nil {
			return ez.Wrap(err)
		}
		if !inspected {
			return nil
		}
		// The body is parsed here rather than with c.FormParams: net/http
		// returns a parse error only from the first ParseForm call, so calling
		// it here would hide the error from the handler's bind. For a
		// malformed body ParseQuery still returns the pairs it could parse,
		// which c.FormValue would also return, so they are checked and the
		// error is left for the handler.
		values, _ := url.ParseQuery(string(body))
		if valuesHoldNUL(values) {
			return ez.New(ez.EINVALID, "Form data contains a NUL byte", nil)
		}

	case mediaType == echo.MIMEMultipartForm:
		return multipartNUL(req, ctype)
	}

	return nil
}

// errNUL is what nulWriter returns for a chunk holding a NUL byte.
var errNUL = errors.New("NUL byte")

// nulWriter discards what it is given and fails on a NUL byte, so a value can
// be scanned as io.CopyBuffer reads it instead of being copied whole.
type nulWriter struct{}

func (nulWriter) Write(p []byte) (int, error) {
	if bytes.IndexByte(p, 0) >= 0 {
		return 0, errNUL
	}
	return len(p), nil
}

// multipartNUL reads a multipart body of up to maxInspectedBody into memory,
// puts it back and walks it as mime/multipart's ReadForm (the parser behind
// FormValue, FormFile, MultipartForm and Bind) reads the fields it stores: the
// name of every named part, the value of a field and the name of a file, whose
// contents are not read. The walk runs on a second reader over the same bytes
// and nothing is parsed into the request, so the handler still reads the body
// itself, with any of those or with MultipartReader.
//
// A Content-Type without a usable boundary is left for the handler, which
// reports it as it would without the middleware, and so is an empty body. A
// body whose parts cannot be read is refused with "Form data could not be
// read". ReadForm uses the same part parser, so every non-empty body the walk
// cannot read the handler would refuse too, except in one window: NextPart
// caps a part header at 10 MB, while ReadForm allows its memory limit plus
// 10 MB, so a field after a header from 10 MB up to the inspection cap would
// reach the handler unchecked. Refusing here closes that window; a larger
// header comes only in a body over maxInspectedBody, which is not inspected.
// For the rest only the message changes (a handler reading FormValue, which
// ignores the parse error, gets the 400 instead of empty values).
//
// The walk stops after maxMultipartParts parts, since ReadForm refuses a body
// with more; a NUL byte past them reaches only a handler that streams the
// body with MultipartReader.
func multipartNUL(req *http.Request, ctype string) error {
	// net/http reads the boundary with mime.ParseMediaType and refuses the
	// body when that fails or the boundary is missing or empty.
	_, params, err := mime.ParseMediaType(ctype)
	if err != nil {
		return nil
	}
	boundary := params["boundary"]
	if boundary == "" {
		return nil
	}

	body, inspected, err := readBody(req)
	if err != nil {
		return ez.Wrap(err)
	}
	if !inspected {
		return nil
	}
	// Echo's Bind skips an empty body and FormValue reads no values from it,
	// while NextPart fails on it.
	if len(body) == 0 {
		return nil
	}

	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	buf := make([]byte, 32<<10)
	for range maxMultipartParts {
		part, err := reader.NextPart()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return ez.New(ez.EINVALID, "Form data could not be read", err)
		}

		// net/http skips a part without a form name.
		name := part.FormName()
		if name == "" {
			continue
		}
		if strings.IndexByte(name, 0) >= 0 {
			return ez.New(ez.EINVALID, "Form data contains a NUL byte", nil)
		}

		filename := part.FileName()
		if filename != "" {
			if strings.IndexByte(filename, 0) >= 0 {
				return ez.New(ez.EINVALID, "File name contains a NUL byte", nil)
			}
			continue
		}

		// A read error is a malformed part, such as one cut short.
		_, err = io.CopyBuffer(nulWriter{}, part, buf)
		if errors.Is(err, errNUL) {
			return ez.New(ez.EINVALID, "Form data contains a NUL byte", nil)
		}
		if err != nil {
			return ez.New(ez.EINVALID, "Form data could not be read", err)
		}
	}

	// ReadForm refuses a body with more parts, so its callers read no field
	// past them.
	return nil
}

// readBody reads a request body of up to maxInspectedBody and puts it back for
// the handler. inspected is false for a larger body, which is not returned: a
// body whose Content-Length is larger is not read, and a chunked one found
// larger is put back as the bytes read followed by the unread rest.
func readBody(req *http.Request) (body []byte, inspected bool, err error) {
	if req.Body == nil || req.Body == http.NoBody || req.ContentLength == 0 {
		return nil, true, nil
	}
	if req.ContentLength > maxInspectedBody {
		return nil, false, nil
	}

	// A chunked body has a ContentLength of -1. The buffer grows only with
	// the bytes received, never from the announced length, so a client
	// announcing a body it does not send cannot make it allocate.
	body, err = io.ReadAll(io.LimitReader(req.Body, maxInspectedBody+1))
	if err != nil {
		return nil, false, ez.New(ez.EINVALID, "Request body could not be read", err)
	}

	if len(body) > maxInspectedBody {
		req.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), req.Body))
		return nil, false, nil
	}

	req.Body = io.NopCloser(bytes.NewReader(body))
	return body, true, nil
}

func valuesHoldNUL(values url.Values) bool {
	for key, list := range values {
		if strings.IndexByte(key, 0) >= 0 {
			return true
		}
		for _, value := range list {
			if strings.IndexByte(value, 0) >= 0 {
				return true
			}
		}
	}
	return false
}

// nulOffset returns the offset of the first NUL in a JSON text: a raw NUL
// byte, or the backslash that starts a \u0000 escape. The escape counts only
// when an even number of backslashes precede it, since an odd run means its
// backslash is itself escaped and the text decodes to a backslash and u0000.
func nulOffset(body []byte) (int, bool) {
	raw := bytes.IndexByte(body, 0)

	i := 0
	for {
		j := bytes.Index(body[i:], nulEscape)
		if j < 0 {
			break
		}
		start := i + j
		if raw >= 0 && raw < start {
			break
		}

		backslashes := 0
		for k := start - 1; k >= 0 && body[k] == '\\'; k-- {
			backslashes++
		}
		if backslashes%2 == 0 {
			return start, true
		}
		i = start + 1
	}

	return raw, raw >= 0
}
