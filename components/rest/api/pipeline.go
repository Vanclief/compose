package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"reflect"
	"strings"

	"github.com/labstack/echo/v4"
	"github.com/vanclief/ez"

	"github.com/vanclief/compose/components/rest/api/validate"
	"github.com/vanclief/compose/components/rest/requests"
)

// serve is the Echo handler of a typed operation: validate the request
// against the compiled schema, decode it once, and pass it to the app.
// Multipart operations validate their form values against the body schema
// the same way.
func (op *operation) serve(c echo.Context) error {
	h := op.reg.handler

	var reqOpts []requests.Option
	if op.timeout > 0 {
		reqOpts = append(reqOpts, requests.WithTimeout(op.timeout))
	}
	request := requests.New(c.Request().Header, c.RealIP(), reqOpts...)
	mediaType := negotiate(strings.Join(c.Request().Header.Values(echo.HeaderAccept), ","), op.produces)
	request.SetAccept(mediaType)

	if !op.reg.ready.Load() {
		return h.ManageError(c, op.id, request, ez.New(ez.EINTERNAL, "api registry not ready", nil))
	}

	instance := map[string]any{}
	values := make([][]string, len(op.params))
	for i, p := range op.params {
		values[i] = paramValues(c, p.In, p.Name)

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
	var formValues map[string]any
	var collisions []validate.FieldError
	if len(op.files) > 0 {
		var err error
		form, err = op.readForm(c)
		if err != nil {
			return h.ManageError(c, op.id, request, err)
		}
		formValues, collisions = op.formBody(form)
		if op.hasBody {
			instance["body"] = formValues
		}
	} else if op.hasBody {
		body, bodyRaw, err := op.readBody(c)
		if err != nil {
			return h.ManageError(c, op.id, request, err)
		}
		if body != nil {
			instance["body"] = body
			raw = bodyRaw
			collisions = bodyCollisions(body, op.bodySchema, op.reg.result.Components)
		}
	}

	// Report schema violations and case collisions together.
	err := op.schema.Validate(instance)
	fields := validate.Fields(err)
	if err != nil && fields == nil {
		return h.ManageError(c, op.id, request, err)
	}
	err = validate.NewFieldsError(append(fields, collisions...))
	if err != nil {
		return h.ManageError(c, op.id, request, err)
	}

	req := reflect.New(op.reqType)
	if form != nil {
		err = op.decodeForm(req, formValues, form)
		if err != nil {
			return h.ManageError(c, op.id, request, err)
		}
	}
	if raw != nil {
		err = json.Unmarshal(raw, req.Interface())
		if err != nil {
			return h.ManageError(c, op.id, request, ez.New(ez.EINVALID, "body could not be decoded", err))
		}
	}
	err = setParams(req.Elem(), op.params, values)
	if err != nil {
		return h.ManageError(c, op.id, request, err)
	}

	request.SetBody(req.Interface().(requests.Body))

	response, err := h.App.HandleRequest(request)
	if err != nil {
		return h.ManageError(c, op.id, request, err)
	}

	_, isBlob := asBlob(response)
	if op.blob && !isBlob {
		return h.ManageError(c, op.id, request, ez.New(ez.EINTERNAL, "HandleRequest response is not an api.Blob", nil))
	}

	return writeResponse(c, response, mediaType)
}

// paramValues returns the non-empty raw values of a parameter.
func paramValues(c echo.Context, in, name string) []string {
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

// readBody reads (up to the operation's body limit) and decodes the JSON
// body. It returns nil for an empty (or null) body and rejects non-JSON
// content types; a missing Content-Type is treated as JSON. It also returns
// the raw bytes for decoding.
func (op *operation) readBody(c echo.Context) (any, []byte, error) {
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
	if contentType != "" && !isJSON(contentType) {
		return nil, nil, ez.New(ez.EINVALID, "content type must be application/json", nil)
	}

	body, err := validate.DecodeBytes(raw)
	if err != nil {
		return nil, nil, ez.Wrap(err)
	}
	if body == nil {
		return nil, nil, nil
	}

	return body, raw, nil
}

func isJSON(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	return mediaType == echo.MIMEApplicationJSON || strings.HasSuffix(mediaType, "+json")
}
