package validate

import (
	"bytes"
	"encoding/json"
	"net/netip"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/vanclief/compose/types"
	"github.com/vanclief/ez"
)

// baseURL roots every resource at an absolute, never-fetched location so that
// "spec.json#/..." refs inside operation schemas resolve to the document.
// santhosh would otherwise turn relative names into file paths under the cwd.
const baseURL = "mem:///"

// specResource is the resource name operation schemas use to reference the document.
const specResource = "spec.json"

// Compiler compiles operation schemas against one OpenAPI 3.1 document.
// It is not safe for concurrent use.
type Compiler struct {
	compiler *jsonschema.Compiler
}

// Schema is a compiled operation schema. It is safe for concurrent use.
type Schema struct {
	schema *jsonschema.Schema
}

// NewCompiler loads the OpenAPI 3.1 document as resource "spec.json" and
// registers the formats compose relies on, email, phone and ip (uuid and
// date-time are built in; unknown formats such as int64 or double are ignored
// by santhosh).
func NewCompiler(doc json.RawMessage) (*Compiler, error) {
	decoded, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc))
	if err != nil {
		return nil, ez.New(ez.EINTERNAL, "OpenAPI document is not valid JSON", err)
	}

	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.AssertFormat()
	compiler.RegisterFormat(&jsonschema.Format{Name: "email", Validate: validateEmail})
	compiler.RegisterFormat(&jsonschema.Format{Name: "phone", Validate: validatePhone})
	compiler.RegisterFormat(&jsonschema.Format{Name: "ip", Validate: validateIP})

	err = compiler.AddResource(baseURL+specResource, decoded)
	if err != nil {
		return nil, ez.New(ez.EINTERNAL, "could not load OpenAPI document", err)
	}

	return &Compiler{compiler: compiler}, nil
}

// Compile compiles a synthetic operation schema. name must be unique per call
// (for example "op:<operationId>") and must not be "spec.json".
func (c *Compiler) Compile(name string, schema map[string]any) (*Schema, error) {
	// Round-trip through JSON so Go-typed values ([]string, int) become the
	// plain JSON values santhosh expects.
	raw, err := json.Marshal(schema)
	if err != nil {
		return nil, ez.New(ez.EINTERNAL, "could not encode schema "+name, err)
	}

	decoded, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, ez.New(ez.EINTERNAL, "could not decode schema "+name, err)
	}

	url := baseURL + name

	err = c.compiler.AddResource(url, decoded)
	if err != nil {
		return nil, ez.New(ez.EINTERNAL, "could not add schema "+name, err)
	}

	compiled, err := c.compiler.Compile(url)
	if err != nil {
		return nil, ez.New(ez.EINTERNAL, "could not compile schema "+name, err)
	}

	return &Schema{schema: compiled}, nil
}

// Validate returns nil or an ez.EINVALID error carrying FieldErrors: the
// schema's violations together with extra, the fields the caller found
// failing by a check of its own, in one NewFieldsError. It maps violations
// to fields until more than MaxFields failed, in the order of their
// locations, and then stops: the message then ends with "; and more"
// instead of a count. Errors that are not schema violations are returned as
// ez.EINTERNAL, without extra.
func (s *Schema) Validate(instance any, extra ...FieldError) error {
	err := s.schema.Validate(instance)
	if err == nil {
		return NewFieldsError(extra)
	}

	verr, ok := err.(*jsonschema.ValidationError)
	if !ok {
		return ez.New(ez.EINTERNAL, "request validation failed", err)
	}

	fields, truncated := collect(s.schema, verr)
	if len(fields) == 0 {
		return ez.New(ez.EINVALID, "request is invalid", nil)
	}

	return newFieldsError(append(fields, extra...), truncated)
}

func validateEmail(v any) error {
	s, ok := v.(string)
	if !ok {
		return nil
	}

	_, err := types.NewEmail(s)
	return err
}

func validatePhone(v any) error {
	s, ok := v.(string)
	if !ok {
		return nil
	}

	return types.ValidatePhoneNumber(s)
}

// validateIP accepts the addresses netip.ParseAddr accepts, of either family
// and with a zone, and refuses the empty string, which the text decoders of
// net.IP and netip.Addr would turn into no address without an error.
func validateIP(v any) error {
	s, ok := v.(string)
	if !ok {
		return nil
	}

	_, err := netip.ParseAddr(s)
	return err
}
