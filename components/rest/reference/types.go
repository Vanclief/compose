// Package reference renders an OpenAPI 3.x document as a self-hosted API
// reference page: an alternative to Scalar and Redoc that ships inside the
// backend, with no CDN assets.
//
// The model below covers the subset of OpenAPI 3.0 and 3.1 that matters for
// documentation. Unknown fields are ignored. Values that differ between 3.0 and
// 3.1 (nullable versus "null" in the type list, exclusiveMinimum as a bool
// versus a number) are normalized by Parse so the renderer sees one shape.
package reference

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/vanclief/ez"
	"gopkg.in/yaml.v3"
)

// Document is a parsed OpenAPI document.
type Document struct {
	OpenAPI    string                `json:"openapi" yaml:"openapi"`
	Info       Info                  `json:"info" yaml:"info"`
	Servers    []Server              `json:"servers" yaml:"servers"`
	Paths      Ordered[*PathItem]    `json:"paths" yaml:"paths"`
	Components Components            `json:"components" yaml:"components"`
	Security   []map[string][]string `json:"security" yaml:"security"`
	Tags       []Tag                 `json:"tags" yaml:"tags"`
}

// Info is the document header.
type Info struct {
	Title          string   `json:"title" yaml:"title"`
	Version        string   `json:"version" yaml:"version"`
	Summary        string   `json:"summary" yaml:"summary"`
	Description    string   `json:"description" yaml:"description"`
	TermsOfService string   `json:"termsOfService" yaml:"termsOfService"`
	Contact        *Contact `json:"contact" yaml:"contact"`
	License        *License `json:"license" yaml:"license"`
}

// Contact is info.contact.
type Contact struct {
	Name  string `json:"name" yaml:"name"`
	URL   string `json:"url" yaml:"url"`
	Email string `json:"email" yaml:"email"`
}

// License is info.license.
type License struct {
	Name string `json:"name" yaml:"name"`
	URL  string `json:"url" yaml:"url"`
}

// Server is one entry of servers.
type Server struct {
	URL         string                    `json:"url" yaml:"url"`
	Description string                    `json:"description" yaml:"description"`
	Variables   map[string]ServerVariable `json:"variables" yaml:"variables"`
}

// ServerVariable is one entry of server.variables.
type ServerVariable struct {
	Default     string   `json:"default" yaml:"default"`
	Enum        []string `json:"enum" yaml:"enum"`
	Description string   `json:"description" yaml:"description"`
}

// Expand returns the server URL with every {variable} replaced by its default.
func (s Server) Expand() string {
	out := s.URL
	for name, v := range s.Variables {
		out = strings.ReplaceAll(out, "{"+name+"}", v.Default)
	}
	return out
}

// Tag is one entry of tags. Operations are grouped under tags in the sidebar.
type Tag struct {
	Name        string `json:"name" yaml:"name"`
	Description string `json:"description" yaml:"description"`
}

// Components holds the reusable objects $ref points at.
type Components struct {
	Schemas         Ordered[*Schema]         `json:"schemas" yaml:"schemas"`
	Responses       Ordered[*Response]       `json:"responses" yaml:"responses"`
	Parameters      Ordered[*Parameter]      `json:"parameters" yaml:"parameters"`
	Examples        Ordered[*Example]        `json:"examples" yaml:"examples"`
	RequestBodies   Ordered[*RequestBody]    `json:"requestBodies" yaml:"requestBodies"`
	Headers         Ordered[*Header]         `json:"headers" yaml:"headers"`
	SecuritySchemes Ordered[*SecurityScheme] `json:"securitySchemes" yaml:"securitySchemes"`
}

// PathItem is one entry of paths.
type PathItem struct {
	Ref         string       `json:"$ref" yaml:"$ref"`
	Summary     string       `json:"summary" yaml:"summary"`
	Description string       `json:"description" yaml:"description"`
	Get         *Operation   `json:"get" yaml:"get"`
	Put         *Operation   `json:"put" yaml:"put"`
	Post        *Operation   `json:"post" yaml:"post"`
	Delete      *Operation   `json:"delete" yaml:"delete"`
	Options     *Operation   `json:"options" yaml:"options"`
	Head        *Operation   `json:"head" yaml:"head"`
	Patch       *Operation   `json:"patch" yaml:"patch"`
	Trace       *Operation   `json:"trace" yaml:"trace"`
	Parameters  []*Parameter `json:"parameters" yaml:"parameters"`
}

// Methods lists the operations of the path item in the canonical HTTP order.
func (p *PathItem) Methods() []MethodOperation {
	all := []MethodOperation{
		{"GET", p.Get}, {"POST", p.Post}, {"PUT", p.Put}, {"PATCH", p.Patch},
		{"DELETE", p.Delete}, {"HEAD", p.Head}, {"OPTIONS", p.Options}, {"TRACE", p.Trace},
	}
	var out []MethodOperation
	for _, m := range all {
		if m.Operation != nil {
			out = append(out, m)
		}
	}
	return out
}

// MethodOperation pairs an HTTP method with its operation.
type MethodOperation struct {
	Method    string
	Operation *Operation
}

// Operation is one HTTP method of a path.
type Operation struct {
	Tags        []string           `json:"tags" yaml:"tags"`
	Summary     string             `json:"summary" yaml:"summary"`
	Description string             `json:"description" yaml:"description"`
	OperationID string             `json:"operationId" yaml:"operationId"`
	Parameters  []*Parameter       `json:"parameters" yaml:"parameters"`
	RequestBody *RequestBody       `json:"requestBody" yaml:"requestBody"`
	Responses   Ordered[*Response] `json:"responses" yaml:"responses"`
	Deprecated  bool               `json:"deprecated" yaml:"deprecated"`
	Servers     []Server           `json:"servers" yaml:"servers"`
	// Security is nil when the operation inherits the document security, and
	// an empty slice when the operation is explicitly public.
	Security *[]map[string][]string `json:"security" yaml:"security"`
}

// Parameter is a path, query, header or cookie parameter.
type Parameter struct {
	Ref         string              `json:"$ref" yaml:"$ref"`
	Name        string              `json:"name" yaml:"name"`
	In          string              `json:"in" yaml:"in"`
	Description string              `json:"description" yaml:"description"`
	Required    bool                `json:"required" yaml:"required"`
	Deprecated  bool                `json:"deprecated" yaml:"deprecated"`
	Style       string              `json:"style" yaml:"style"`
	Explode     *bool               `json:"explode" yaml:"explode"`
	Schema      *Schema             `json:"schema" yaml:"schema"`
	Example     any                 `json:"example" yaml:"example"`
	Examples    Ordered[*Example]   `json:"examples" yaml:"examples"`
	Content     Ordered[*MediaType] `json:"content" yaml:"content"`
}

// Header is a response header; the same shape as Parameter without name and in.
type Header = Parameter

// RequestBody is operation.requestBody.
type RequestBody struct {
	Ref         string              `json:"$ref" yaml:"$ref"`
	Description string              `json:"description" yaml:"description"`
	Required    bool                `json:"required" yaml:"required"`
	Content     Ordered[*MediaType] `json:"content" yaml:"content"`
}

// MediaType is one content type of a body or response.
type MediaType struct {
	Schema   *Schema           `json:"schema" yaml:"schema"`
	Example  any               `json:"example" yaml:"example"`
	Examples Ordered[*Example] `json:"examples" yaml:"examples"`
}

// Example is a named example.
type Example struct {
	Ref           string `json:"$ref" yaml:"$ref"`
	Summary       string `json:"summary" yaml:"summary"`
	Description   string `json:"description" yaml:"description"`
	Value         any    `json:"value" yaml:"value"`
	ExternalValue string `json:"externalValue" yaml:"externalValue"`
}

// Response is one status code of operation.responses.
type Response struct {
	Ref         string              `json:"$ref" yaml:"$ref"`
	Description string              `json:"description" yaml:"description"`
	Headers     Ordered[*Header]    `json:"headers" yaml:"headers"`
	Content     Ordered[*MediaType] `json:"content" yaml:"content"`
}

// SecurityScheme is one entry of components.securitySchemes.
type SecurityScheme struct {
	Type         string `json:"type" yaml:"type"`
	Description  string `json:"description" yaml:"description"`
	Name         string `json:"name" yaml:"name"`
	In           string `json:"in" yaml:"in"`
	Scheme       string `json:"scheme" yaml:"scheme"`
	BearerFormat string `json:"bearerFormat" yaml:"bearerFormat"`
}

// Discriminator is schema.discriminator.
type Discriminator struct {
	PropertyName string            `json:"propertyName" yaml:"propertyName"`
	Mapping      map[string]string `json:"mapping" yaml:"mapping"`
}

// Schema is a JSON Schema as used by OpenAPI 3.0 and 3.1.
//
// Type always holds the non-null types; Nullable is true when 3.0 said
// nullable: true or when 3.1 listed "null" in the type array. Exclusive bounds
// always hold the numeric form.
type Schema struct {
	Ref           string           `json:"$ref" yaml:"$ref"`
	Title         string           `json:"title" yaml:"title"`
	Description   string           `json:"description" yaml:"description"`
	Type          TypeList         `json:"type" yaml:"type"`
	Format        string           `json:"format" yaml:"format"`
	Nullable      bool             `json:"nullable" yaml:"nullable"`
	Enum          []any            `json:"enum" yaml:"enum"`
	Const         any              `json:"const" yaml:"const"`
	Default       any              `json:"default" yaml:"default"`
	Example       any              `json:"example" yaml:"example"`
	Examples      []any            `json:"examples" yaml:"examples"`
	Deprecated    bool             `json:"deprecated" yaml:"deprecated"`
	ReadOnly      bool             `json:"readOnly" yaml:"readOnly"`
	WriteOnly     bool             `json:"writeOnly" yaml:"writeOnly"`
	Properties    Ordered[*Schema] `json:"properties" yaml:"properties"`
	Required      []string         `json:"required" yaml:"required"`
	Items         *Schema          `json:"items" yaml:"items"`
	AllOf         []*Schema        `json:"allOf" yaml:"allOf"`
	OneOf         []*Schema        `json:"oneOf" yaml:"oneOf"`
	AnyOf         []*Schema        `json:"anyOf" yaml:"anyOf"`
	Not           *Schema          `json:"not" yaml:"not"`
	Discriminator *Discriminator   `json:"discriminator" yaml:"discriminator"`

	// AdditionalProperties is nil when unset, which JSON Schema reads as
	// allowed. Use AdditionalSchema to get the schema of extra properties.
	AdditionalProperties *AdditionalProperties `json:"additionalProperties" yaml:"additionalProperties"`

	Minimum          *float64 `json:"minimum" yaml:"minimum"`
	Maximum          *float64 `json:"maximum" yaml:"maximum"`
	ExclusiveMinimum *Bound   `json:"exclusiveMinimum" yaml:"exclusiveMinimum"`
	ExclusiveMaximum *Bound   `json:"exclusiveMaximum" yaml:"exclusiveMaximum"`
	MultipleOf       *float64 `json:"multipleOf" yaml:"multipleOf"`
	MinLength        *int     `json:"minLength" yaml:"minLength"`
	MaxLength        *int     `json:"maxLength" yaml:"maxLength"`
	Pattern          string   `json:"pattern" yaml:"pattern"`
	MinItems         *int     `json:"minItems" yaml:"minItems"`
	MaxItems         *int     `json:"maxItems" yaml:"maxItems"`
	UniqueItems      bool     `json:"uniqueItems" yaml:"uniqueItems"`
	MinProperties    *int     `json:"minProperties" yaml:"minProperties"`
	MaxProperties    *int     `json:"maxProperties" yaml:"maxProperties"`
}

// schemaFields has the fields of Schema without its methods, so the custom
// unmarshalers can decode the object form without recursing into themselves.
type schemaFields Schema

// UnmarshalJSON accepts a schema object or a 3.1 boolean schema: true is the
// empty schema (anything) and false is {"not": {}} (nothing).
func (s *Schema) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if string(trimmed) == "true" || string(trimmed) == "false" {
		*s = booleanSchema(string(trimmed) == "true")
		return nil
	}
	err := json.Unmarshal(data, (*schemaFields)(s))
	if err != nil {
		return ez.Wrap(err)
	}
	return nil
}

// UnmarshalYAML accepts a schema mapping or a 3.1 boolean schema.
func (s *Schema) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode && node.ShortTag() == "!!bool" {
		var allowed bool
		err := node.Decode(&allowed)
		if err != nil {
			return ez.Wrap(err)
		}
		*s = booleanSchema(allowed)
		return nil
	}
	err := node.Decode((*schemaFields)(s))
	if err != nil {
		return ez.Wrap(err)
	}
	return nil
}

func booleanSchema(allowed bool) Schema {
	if allowed {
		return Schema{}
	}
	return Schema{Not: &Schema{}}
}

// AdditionalSchema returns the schema for additional properties, or nil when
// they are a free-form or forbidden.
func (s *Schema) AdditionalSchema() *Schema {
	if s.AdditionalProperties == nil {
		return nil
	}
	return s.AdditionalProperties.Schema
}

// IsRequired reports whether name is listed in required.
func (s *Schema) IsRequired(name string) bool {
	for _, r := range s.Required {
		if r == name {
			return true
		}
	}
	return false
}

// AdditionalProperties decodes either a boolean or a schema.
type AdditionalProperties struct {
	Allowed bool
	Schema  *Schema
}

// UnmarshalJSON accepts true, false or a schema object.
func (a *AdditionalProperties) UnmarshalJSON(data []byte) error {
	var allowed bool
	err := json.Unmarshal(data, &allowed)
	if err == nil {
		a.Allowed = allowed
		a.Schema = nil
		return nil
	}

	var schema Schema
	err = json.Unmarshal(data, &schema)
	if err != nil {
		return ez.Wrap(err)
	}
	a.Allowed = true
	a.Schema = &schema
	return nil
}

// UnmarshalYAML accepts true, false or a schema mapping.
func (a *AdditionalProperties) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		var allowed bool
		err := node.Decode(&allowed)
		if err != nil {
			return ez.Wrap(err)
		}
		a.Allowed = allowed
		a.Schema = nil
		return nil
	}

	var schema Schema
	err := node.Decode(&schema)
	if err != nil {
		return ez.Wrap(err)
	}
	a.Allowed = true
	a.Schema = &schema
	return nil
}

// TypeList decodes "type" as either a string (3.0) or an array (3.1).
type TypeList []string

// UnmarshalJSON accepts a string or an array of strings. A JSON null leaves
// the list empty.
func (t *TypeList) UnmarshalJSON(data []byte) error {
	if string(bytes.TrimSpace(data)) == "null" {
		*t = nil
		return nil
	}

	var single string
	err := json.Unmarshal(data, &single)
	if err == nil {
		*t = TypeList{single}
		return nil
	}

	var many []string
	err = json.Unmarshal(data, &many)
	if err != nil {
		return ez.Wrap(err)
	}
	*t = TypeList(many)
	return nil
}

// UnmarshalYAML accepts a scalar or a sequence of scalars.
func (t *TypeList) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		*t = TypeList{node.Value}
		return nil
	}

	var many []string
	err := node.Decode(&many)
	if err != nil {
		return ez.Wrap(err)
	}
	*t = TypeList(many)
	return nil
}

// Has reports whether the list contains name.
func (t TypeList) Has(name string) bool {
	for _, v := range t {
		if v == name {
			return true
		}
	}
	return false
}

// Primary returns the first non-null type, or "" when the schema is untyped.
func (t TypeList) Primary() string {
	for _, v := range t {
		if v != "null" {
			return v
		}
	}
	return ""
}

// Bound decodes exclusiveMinimum and exclusiveMaximum, which are booleans in
// 3.0 (qualifying minimum and maximum) and numbers in 3.1.
type Bound struct {
	Flag  bool
	Value *float64
}

// UnmarshalJSON accepts a bool or a number.
func (b *Bound) UnmarshalJSON(data []byte) error {
	var flag bool
	err := json.Unmarshal(data, &flag)
	if err == nil {
		b.Flag = flag
		b.Value = nil
		return nil
	}

	var value float64
	err = json.Unmarshal(data, &value)
	if err != nil {
		return ez.Wrap(err)
	}
	b.Flag = false
	b.Value = &value
	return nil
}

// UnmarshalYAML accepts a bool or a number.
func (b *Bound) UnmarshalYAML(node *yaml.Node) error {
	var flag bool
	err := node.Decode(&flag)
	if err == nil {
		b.Flag = flag
		b.Value = nil
		return nil
	}

	var value float64
	err = node.Decode(&value)
	if err != nil {
		return ez.New(ez.EINVALID, fmt.Sprintf("line %d: exclusive bound must be a bool or a number", node.Line), err)
	}
	b.Flag = false
	b.Value = &value
	return nil
}
