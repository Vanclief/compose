package validate

import (
	"bytes"
	"encoding/json"

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
// registers the formats compose relies on (uuid and date-time are built in;
// unknown formats such as int64 or double are ignored by santhosh).
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

	err = compiler.AddResource(baseURL+specResource, decoded)
	if err != nil {
		return nil, ez.New(ez.EINTERNAL, "could not load OpenAPI document", err)
	}

	return &Compiler{compiler: compiler}, nil
}

// Compile compiles a synthetic operation schema. name must be unique per call
// (for example "op:<operationId>.json") and must not be "spec.json".
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

// Validate returns nil or an ez.EINVALID error carrying FieldErrors.
// Errors that are not schema violations are returned as ez.EINTERNAL.
func (s *Schema) Validate(instance any) error {
	err := s.schema.Validate(instance)
	if err == nil {
		return nil
	}

	verr, ok := err.(*jsonschema.ValidationError)
	if !ok {
		return ez.New(ez.EINTERNAL, "request validation failed", err)
	}

	fields := collect(s.schema, verr)
	if len(fields) == 0 {
		return ez.New(ez.EINVALID, "request is invalid", nil)
	}

	return NewFieldsError(fields)
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
