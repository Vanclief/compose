package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"mime/multipart"
	"net/http"
	"reflect"
	"slices"
	"strings"

	"github.com/labstack/echo/v5"
	"github.com/vanclief/ez"

	"github.com/vanclief/compose/components/rest/api/internal/huma"
	"github.com/vanclief/compose/components/rest/api/openapi"
	"github.com/vanclief/compose/components/rest/api/validate"
)

// multipartMemory is how much of a multipart body is held in memory; larger
// files spill to temporary files (Echo's default).
const multipartMemory = 32 << 20

var (
	fileHeaderType  = reflect.TypeFor[*multipart.FileHeader]()
	fileHeadersType = reflect.TypeFor[[]*multipart.FileHeader]()
)

// fileField is a request field tagged form:"name" that receives uploaded
// files. Any file field makes the operation multipart/form-data: the other
// body fields are read from form values, coerced and validated like query
// parameters. An uploaded file is readable only during the handler call:
// the files spilled to disk are removed after it returns.
type fileField struct {
	name     string
	index    []int
	multiple bool // []*multipart.FileHeader
	required bool
	doc      string
}

// fileFields returns the form:"name" fields of a request type. They must be
// *multipart.FileHeader or []*multipart.FileHeader, carry json:"-" and not
// be reached through an unexported embedded pointer, which decodeForm cannot
// allocate; a file typed field without a form tag is an error too.
func fileFields(t reflect.Type) ([]fileField, error) {
	var files []fileField
	var problems []string
	for _, f := range reflect.VisibleFields(derefType(t)) {
		if !f.IsExported() || f.Anonymous {
			continue
		}

		isFile := f.Type == fileHeaderType || f.Type == fileHeadersType
		name, tagged := f.Tag.Lookup("form")
		if !tagged {
			if isFile {
				problems = append(problems, fmt.Sprintf("file field %s needs a form:\"...\" tag and json:\"-\"", f.Name))
			}
			continue
		}

		embedded := unexportedEmbeddedPointer(t, f.Index)
		switch {
		case name == "":
			problems = append(problems, fmt.Sprintf("field %s has an empty form tag", f.Name))
		case !isFile:
			problems = append(problems, fmt.Sprintf("form:%q field %s must be *multipart.FileHeader or []*multipart.FileHeader", name, f.Name))
		case jsonTagName(f) != "-":
			problems = append(problems, fmt.Sprintf("form:%q field %s must carry json:\"-\"", name, f.Name))
		case embedded != nil:
			problems = append(problems, fmt.Sprintf(
				"form:%q field %s: reached through the unexported embedded pointer %s, which cannot be allocated (export the type or embed it by value)",
				name, f.Name, embedded))
		case slices.ContainsFunc(files, func(other fileField) bool { return other.name == name }):
			problems = append(problems, fmt.Sprintf("duplicate form:%q field %s", name, f.Name))
		default:
			files = append(files, fileField{
				name:     name,
				index:    f.Index,
				multiple: f.Type == fileHeadersType,
				required: f.Tag.Get("required") == "true",
				doc:      f.Tag.Get("doc"),
			})
		}
	}
	if len(problems) > 0 {
		return nil, ez.New(ez.EINVALID, strings.Join(problems, "; "), nil)
	}
	return files, nil
}

// checkFormFields rejects body fields a form value cannot carry: nested
// objects, maps, byte slices and types.Optional; and a body field named like
// one of files,
// whose form part would be read as both the value and the file. The fields
// of an embedded struct tagged json:"-" are no body fields.
func checkFormFields(t reflect.Type, files []fileField) error {
	var problems []string
	for _, f := range reflect.VisibleFields(derefType(t)) {
		// An embedded struct whose json tag has no name has its fields
		// promoted, as encoding/json does; they are checked one by one.
		tagName, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if !f.IsExported() || tagName == "-" || (f.Anonymous && tagName == "") || openapi.UnderIgnoredEmbed(t, f.Index) {
			continue
		}
		name := jsonTagName(f)
		i := slices.IndexFunc(files, func(file fileField) bool { return file.name == name })
		if i >= 0 {
			problems = append(problems, fmt.Sprintf(
				"multipart body field %s (%s): the file field %s has the same form name, so one form part would be read as both; rename one of them",
				f.Name, name, derefType(t).FieldByIndex(files[i].index).Name))
		}
		if openapi.IsOptional(f.Type) {
			problems = append(problems, fmt.Sprintf(
				"multipart body field %s (%s): types.Optional is a JSON body field; use a pointer for an optional form value",
				f.Name, name))
			continue
		}
		if !paramTypeSupported(f.Type) {
			problems = append(problems, fmt.Sprintf(
				"multipart body field %s (%s): type %s cannot be sent as a form value (use a string, bool or number kind, an encoding.TextUnmarshaler, or a slice of those)",
				f.Name, name, f.Type))
		}
	}
	if len(problems) > 0 {
		return ez.New(ez.EINVALID, strings.Join(problems, "; "), nil)
	}
	return nil
}

func jsonTagName(f reflect.StructField) string {
	name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
	if name == "" {
		return f.Name
	}
	return name
}

// readForm parses a multipart/form-data body under the operation's body
// limit.
func (op *operation) readForm(c *echo.Context) (*multipart.Form, error) {
	mediaType, _, err := mime.ParseMediaType(c.Request().Header.Get(echo.HeaderContentType))
	if err != nil || mediaType != echo.MIMEMultipartForm {
		return nil, ez.New(ez.EINVALID, "content type must be multipart/form-data", err)
	}

	c.Request().Body = http.MaxBytesReader(c.Response(), c.Request().Body, op.maxBody)
	err = c.Request().ParseMultipartForm(multipartMemory)
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return nil, ez.New(ez.EINVALID, fmt.Sprintf("body exceeds %d bytes", op.maxBody), err)
	}
	if err != nil {
		return nil, ez.New(ez.EINVALID, "body is not a valid multipart form", err)
	}
	return c.Request().MultipartForm, nil
}

// formBody builds the body instance from the declared properties' form
// values (empty values are absent, unknown keys are ignored), the values
// decodeForm decodes (see decodeValue), and reports missing required files
// and repeated single files.
func (op *operation) formBody(form *multipart.Form) (body, decode map[string]any, fields []validate.FieldError) {
	body = map[string]any{}
	decode = map[string]any{}
	for name, kind := range op.formKinds {
		var values []string
		for _, v := range form.Value[name] {
			if v != "" {
				values = append(values, v)
			}
		}
		coerced := validate.Coerce(values, kind)
		if coerced != nil {
			body[name] = coerced
			decode[name] = decodeValue(coerced, values)
		}
	}

	for _, f := range op.files {
		n := len(form.File[f.name])
		if n == 0 && f.required {
			fields = append(fields, validate.FieldError{In: "body", Name: f.name, Message: "is required"})
		}
		if n > 1 && !f.multiple {
			fields = append(fields, validate.FieldError{In: "body", Name: f.name, Message: "must be a single file"})
		}
	}
	return body, decode, fields
}

// decodeValue returns what decodeForm decodes for form values that
// validate.Coerce turned into coerced: a number takes the client's own
// literal when it is a JSON number, as a query parameter and a JSON body
// field do, where the validator may have seen float64 text (65 digits, 1e2);
// any other value, and a number that is no JSON number (007, +5), takes
// coerced, which Coerce gave exact canonical text.
func decodeValue(coerced any, values []string) any {
	switch v := coerced.(type) {
	case json.Number:
		if json.Valid([]byte(values[0])) {
			return json.Number(values[0])
		}
	case []any:
		// The items Coerce split values into.
		items := splitValues(values)
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = decodeValue(item, items[i:i+1])
		}
		return out
	}
	return coerced
}

// decodeForm decodes the validated form values, as decodeValue gives them,
// into req (through encoding/json, so named types decode as in JSON bodies)
// and sets the file fields.
func (op *operation) decodeForm(req reflect.Value, body map[string]any, form *multipart.Form) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return ez.New(ez.EINTERNAL, "could not encode the form values", err)
	}
	err = json.Unmarshal(raw, req.Interface())
	if err != nil {
		return ez.New(ez.EINVALID, "form values could not be decoded", err)
	}

	for _, f := range op.files {
		headers := form.File[f.name]
		if len(headers) == 0 {
			continue
		}
		field := fieldOf(req.Elem(), f.index)
		if f.multiple {
			field.Set(reflect.ValueOf(headers))
			continue
		}
		field.Set(reflect.ValueOf(headers[0]))
	}
	return nil
}

// formKindsOf returns the coercion kind of every property of the body
// component.
func formKindsOf(ref string, comps map[string]*huma.Schema) map[string]validate.Kind {
	kinds := map[string]validate.Kind{}
	body := comps[strings.TrimPrefix(ref, openapi.RefPrefix)]
	if body == nil {
		return kinds
	}
	for name, prop := range body.Properties {
		kinds[name] = kindOf(resolveSchema(prop, comps), comps)
	}
	return kinds
}

// multipartBody documents a multipart/form-data request body: the body
// component's properties inline, plus one binary property (an array of
// binaries for []*multipart.FileHeader) per file field.
func (op *operation) multipartBody(res *openapi.Result) *huma.RequestBody {
	schema := &huma.Schema{Type: huma.TypeObject, Properties: map[string]*huma.Schema{}}

	ref, _, ok := res.BodyRef(op.reqType)
	if ok {
		body := res.Components[strings.TrimPrefix(ref, openapi.RefPrefix)]
		for name, prop := range body.Properties {
			schema.Properties[name] = prop
		}
		schema.Required = append(schema.Required, body.Required...)
	}

	for _, f := range op.files {
		file := &huma.Schema{Type: huma.TypeString, Format: "binary"}
		if f.multiple {
			file = &huma.Schema{Type: huma.TypeArray, Items: file}
		}
		file.Description = f.doc
		schema.Properties[f.name] = file
		if f.required {
			schema.Required = append(schema.Required, f.name)
		}
	}

	// Required even when every part is optional: readForm refuses a request
	// without a multipart/form-data body.
	return &huma.RequestBody{
		Required: true,
		Content:  map[string]*huma.MediaType{echo.MIMEMultipartForm: {Schema: schema}},
	}
}
