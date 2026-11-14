package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"reflect"
	"strings"

	"github.com/labstack/echo/v5"
	"github.com/vanclief/ez"

	"github.com/vanclief/compose/components/rest/api/validate"
	"github.com/vanclief/compose/components/rest/handler"
	"github.com/vanclief/compose/components/rest/requests"
)

// acceptKey is the request context key of the negotiated media type.
type acceptKey struct{}

// Accept returns the response media type negotiated for the request whose
// context is ctx, the same value as request.GetAccept(), or "" when ctx does
// not come from a registered operation.
func Accept(ctx context.Context) string {
	mediaType, _ := ctx.Value(acceptKey{}).(string)
	return mediaType
}

// serve is the Echo handler of a typed operation: authenticate the request,
// validate it against the compiled schema, decode it once, run Validate, and
// pass it to the handler of the negotiated media type. The authenticator runs
// before any parameter is coerced or any body byte is read, so an
// unauthenticated caller learns nothing about the request's shape and nothing
// is parsed for it; request.GetBody() is nil inside it. Multipart operations
// validate their form values against the body schema the same way. Every
// error is returned to Echo, for the app's HTTPErrorHandler to answer: the
// pipeline's own errors are ez errors, the handler's are returned as is.
func (op *operation) serve(c *echo.Context) error {
	reqOpts := []requests.Option{requests.WithMethod(c.Request().Method)}
	if op.timeout > 0 {
		reqOpts = append(reqOpts, requests.WithTimeout(op.timeout))
	}
	request := requests.NewWithContext(c.Request().Context(), c.Request().Header, c.RealIP(), reqOpts...)
	defer func() {
		if request.Cancel != nil {
			request.Cancel()
		}
	}()
	handler.SetRequest(c, request)

	mediaType := negotiate(strings.Join(c.Request().Header.Values(echo.HeaderAccept), ","), op.produces)
	if len(op.produces) > 1 {
		c.Response().Header().Add(echo.HeaderVary, echo.HeaderAccept)
	}
	request.SetAccept(mediaType)
	request.SetContext(context.WithValue(request.GetContext(), acceptKey{}, mediaType))

	if !op.reg.ready.Load() {
		return ez.New(ez.EINTERNAL, "api registry not ready", nil)
	}

	// The authenticator's, Validate's and the handler's errors are the app's
	// and reach Echo as returned: ez.Wrap would turn an Echo error among them
	// into a 500.
	if !op.public {
		err := op.authenticate(request)
		if err != nil {
			return err
		}
	}

	instance := map[string]any{}
	values := make([][]string, len(op.params))
	for i, p := range op.params {
		values[i] = paramValues(c, p.In, p.Name)
		if p.In == "header" && op.kinds[i].Type == "array" {
			values[i] = headerListItems(values[i])
		}

		loc, ok := instance[p.In].(map[string]any)
		if !ok {
			loc = map[string]any{}
			instance[p.In] = loc
		}
		coerced := validate.Coerce(values[i], op.kinds[i])
		if coerced != nil {
			loc[p.Name] = coerced
		}
	}

	var raw []byte
	var form *multipart.Form
	var formValues, formDecode map[string]any
	var collisions []validate.FieldError
	if len(op.files) > 0 {
		var err error
		form, err = op.readForm(c)
		if err != nil {
			return ez.Wrap(err)
		}
		// net/http removes spilled files only for its own request, not for
		// one middleware replaced with c.SetRequest. The error is dropped:
		// the request is answered, and nothing can act on a temporary file
		// that would not go.
		defer form.RemoveAll()
		formValues, formDecode, collisions = op.formBody(form)
		if op.hasBody {
			instance["body"] = formValues
		}
	} else if op.hasBody {
		body, bodyRaw, err := op.readBody(c)
		if err != nil {
			return ez.Wrap(err)
		}
		if body != nil {
			instance["body"] = body
			raw = bodyRaw
			collisions = bodyCollisions(body, op.bodySchema, op.reg.result.Components)
		}
	}

	// Report schema violations and case collisions together.
	err := op.schema.Validate(instance, collisions...)
	if err != nil {
		return ez.Wrap(err)
	}

	req := reflect.New(op.reqType)
	for _, path := range op.embedded {
		field := req.Elem().FieldByIndex(path)
		field.Set(reflect.New(field.Type().Elem()))
	}
	if form != nil {
		err = op.decodeForm(req, formDecode, form)
		if err != nil {
			return ez.Wrap(err)
		}
	}
	if raw != nil {
		err = json.Unmarshal(raw, req.Interface())
		if err != nil {
			return ez.New(ez.EINVALID, "body could not be decoded", err)
		}
	}
	err = setParams(req.Elem(), op.params, values)
	if err != nil {
		return ez.Wrap(err)
	}

	body := req.Interface().(requests.Body)
	request.SetBody(body)

	// The app's error, as returned (see Authenticate above).
	err = body.Validate()
	if err != nil {
		return err
	}

	// Read the context after Authenticate, which may have replaced it. The
	// handler's error is the app's, as returned.
	response, err := op.call(request.GetContext(), mediaType, body)
	if err != nil {
		return err
	}

	return writeResponse(c, response, mediaType)
}

// call runs the handler of the negotiated media type: the producer declared
// for it, else the primary handler.
func (op *operation) call(ctx context.Context, mediaType string, body requests.Body) (any, error) {
	for _, p := range op.producers {
		if p.mediaType == mediaType {
			return p.fn(ctx, body)
		}
	}
	return op.invoke(ctx, body)
}

// paramValues returns the non-empty raw values of a parameter.
func paramValues(c *echo.Context, in, name string) []string {
	var values []string
	switch in {
	case "path":
		values = []string{c.Param(name)}
	case "query":
		values = c.QueryParams()[name]
	case "header":
		values = c.Request().Header.Values(name)
	}

	var out []string
	for _, v := range values {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// headerListItems splits the lines of an array header into its items: each
// line is a comma-separated list whose items may carry spaces around them
// (a, b), and empty items are ignored (RFC 9110 section 5.6.1).
func headerListItems(lines []string) []string {
	var items []string
	for _, line := range lines {
		for _, item := range strings.Split(line, ",") {
			item = strings.Trim(item, " \t")
			if item != "" {
				items = append(items, item)
			}
		}
	}
	return items
}

// readBody reads (up to the operation's body limit) and decodes the JSON
// body. It returns nil for an empty body, whatever the Content-Type, and for
// a JSON null. It rejects a non-empty body whose Content-Type is missing or
// not JSON (rest.RejectNUL scans only bodies that declare JSON or a form),
// bodies nested deeper than maxBodyDepth and bodies in which an object
// repeats a key. The decoded body carries every number in
// the text validate.CanonicalNumber gives it. It also returns the raw bytes,
// unchanged, for decoding.
func (op *operation) readBody(c *echo.Context) (any, []byte, error) {
	raw, err := io.ReadAll(http.MaxBytesReader(c.Response(), c.Request().Body, op.maxBody))
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return nil, nil, ez.New(ez.EINVALID, fmt.Sprintf("body exceeds %d bytes", op.maxBody), err)
	}
	if err != nil {
		return nil, nil, ez.New(ez.EINVALID, "body could not be read", err)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil, nil
	}

	contentType := c.Request().Header.Get(echo.HeaderContentType)
	if !isJSON(contentType) {
		return nil, nil, ez.New(ez.EINVALID, "content type must be application/json", nil)
	}

	body, err := validate.DecodeBytes(raw)
	if err != nil {
		return nil, nil, ez.Wrap(err)
	}
	if body == nil {
		return nil, nil, nil
	}
	body, err = canonicalBody(body, 0)
	if err != nil {
		return nil, nil, ez.Wrap(err)
	}
	// After canonicalBody has bounded the depth the scan holds.
	err = validate.RejectRepeatedKeys(raw)
	if err != nil {
		return nil, nil, ez.Wrap(err)
	}

	return body, raw, nil
}

// maxBodyDepth bounds how deep the arrays and objects of a JSON body nest.
// Every validation error and key-case collision carries its full path, so on
// a recursive type a small, deeply nested body would make answers and
// allocations grow with its depth (with its square in the collision walk).
const maxBodyDepth = 64

// canonicalBody walks a decoded JSON value whose arrays and objects are
// depth levels deep, and returns it with every json.Number replaced by the
// text validate.CanonicalNumber gives it, in maps and slices in place. It refuses
// arrays and objects nested deeper than maxBodyDepth, each array and object
// counting as one level, at the first one past the limit without looking
// inside it.
func canonicalBody(value any, depth int) (any, error) {
	switch v := value.(type) {
	case json.Number:
		return validate.CanonicalNumber(string(v)), nil
	case map[string]any:
		if depth == maxBodyDepth {
			return nil, ez.New(ez.EINVALID, fmt.Sprintf("body is nested deeper than %d levels", maxBodyDepth), nil)
		}
		for key, item := range v {
			canonical, err := canonicalBody(item, depth+1)
			if err != nil {
				return nil, ez.Wrap(err)
			}
			v[key] = canonical
		}
	case []any:
		if depth == maxBodyDepth {
			return nil, ez.New(ez.EINVALID, fmt.Sprintf("body is nested deeper than %d levels", maxBodyDepth), nil)
		}
		for i, item := range v {
			canonical, err := canonicalBody(item, depth+1)
			if err != nil {
				return nil, ez.Wrap(err)
			}
			v[i] = canonical
		}
	}
	return value, nil
}

func isJSON(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	return mediaType == echo.MIMEApplicationJSON || strings.HasSuffix(mediaType, "+json")
}
