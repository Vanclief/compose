package mcp

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/vanclief/ez"

	"github.com/vanclief/compose/components/rest/reference"
)

// ToolName is the name of the tool the function becomes when promoted: its
// catalog name with the dot replaced by a hyphen, units.list as units-list,
// since claude.ai and the LLM APIs only allow [a-zA-Z0-9_-]{1,64}. Module and
// function names have no hyphen, so the catalog name maps back. ToolName only
// swaps the dot: New checks promoted names against ^[a-zA-Z0-9_-]{1,64}$, and
// a host listing other functions should check them too.
func (f *Function) ToolName() string {
	return strings.Replace(f.Name, ".", "-", 1)
}

// maxSchemaNodes bounds the work of InputSchema. Shared references are
// expanded inline and only cycles stop the expansion, so a chain of schemas
// each referencing the next twice grows exponentially; past this many nodes
// the schema is also too large for a model to use.
const maxSchemaNodes = 1000

// InputSchema is the JSON schema of the function's flat arguments, the one
// a promoted tool advertises.
//
// For an operation it has one property per argument, with the full JSON
// Schema of each argument, refs resolved, the required arguments in
// required, path arguments always among them, and no other properties
// allowed. An argument name used twice, such as a path and a query id, is one
// property, required when either is. A schema of more than 1000 nodes is
// ez.EINVALID. A composite has the schema reflected from its Input
// type, or any object when Input is not a struct, since Run takes anything.
func (f *Function) InputSchema() (*jsonschema.Schema, error) {
	if f.Composite {
		input := derefType(reflect.TypeOf(f.composite.Input))
		if input == nil || input.Kind() != reflect.Struct {
			return &jsonschema.Schema{Type: "object"}, nil
		}
		schema, err := jsonschema.ForType(input, nil)
		if err != nil {
			return nil, ez.New(ez.EINVALID, "mcp: cannot derive a JSON Schema for "+input.String(), err)
		}
		return schema, nil
	}

	b := &schemaBuilder{doc: f.doc, visiting: map[any]bool{}}
	schema := &jsonschema.Schema{Type: "object", AdditionalProperties: noSchema()}
	for _, a := range f.Args {
		prop := b.convertSchema(a.Schema, a.In != "path")
		if a.Description != "" {
			prop.Description = a.Description
		}
		if schema.Properties == nil {
			schema.Properties = map[string]*jsonschema.Schema{}
		}
		_, seen := schema.Properties[a.Name]
		if !seen {
			schema.Properties[a.Name] = prop
			schema.PropertyOrder = append(schema.PropertyOrder, a.Name)
		}
		if (a.Required || a.In == "path") && !slices.Contains(schema.Required, a.Name) {
			schema.Required = append(schema.Required, a.Name)
		}
	}
	if b.nodes > maxSchemaNodes {
		return nil, ez.New(ez.EINVALID, fmt.Sprintf("mcp: the input schema of %s has more than %d nodes; shared references expand inline, so promote a function with a smaller body", f.Name, maxSchemaNodes), nil)
	}
	return schema, nil
}

// schemaTypes is the type list of s, "object" for a schema that only lists
// properties, as OpenAPI documents mean it.
func schemaTypes(s *reference.Schema) []string {
	types := append([]string(nil), s.Type...)
	if len(types) == 0 && s.Properties.Len() > 0 {
		types = append(types, "object")
	}
	return types
}

// typeStub is the schema of s cut to its types and description.
func typeStub(s *reference.Schema) *jsonschema.Schema {
	out := &jsonschema.Schema{Description: s.Description}
	types := schemaTypes(s)
	if len(types) == 1 {
		out.Type = types[0]
	}
	if len(types) > 1 {
		out.Types = types
	}
	return out
}

// exclusiveBound returns the exclusive and inclusive bounds of a JSON Schema
// from the document's. OpenAPI 3.0 writes exclusiveMinimum: true next to
// minimum, which JSON Schema writes as exclusiveMinimum alone.
func exclusiveBound(exclusive *reference.Bound, inclusive *float64) (*float64, *float64) {
	if exclusive == nil {
		return nil, inclusive
	}
	if exclusive.Value != nil {
		return exclusive.Value, inclusive
	}
	if exclusive.Flag {
		return inclusive, nil
	}
	return nil, inclusive
}

// noSchema is the schema no value matches; jsonschema-go writes {not: {}} as
// false.
func noSchema() *jsonschema.Schema {
	return &jsonschema.Schema{Not: &jsonschema.Schema{}}
}

// schemaBuilder converts document schemas into the JSON Schemas of tool
// arguments.
//
// visiting holds the schemas being converted, by resolved schema and by
// component name, so recursion through a ref or an inline allOf ends. nodes
// counts the schemas converted; past maxSchemaNodes convert stops expanding.
type schemaBuilder struct {
	doc      *reference.Document
	visiting map[any]bool
	nodes    int
}

// convert converts a document schema: types, nullability, format, enum,
// const, default, the number, string, array and object bounds, items,
// properties, alternatives and not, with refs resolved and readOnly
// properties left out as the body arguments leave them out. A schema met
// again inside itself is cut to its types, so recursive types end.
// Nullable and the description are taken from the wrapper s too, since
// resolving a ref drops them; the wrapper's description comes first.
//
// grug: the schema is built from the flattened document schema, which is
// what describe_function shows, so a few JSON Schema shapes are advertised
// more loosely or more strictly than written: allOf is merged by
// Document.Flatten, which keeps the first value of a scalar bound and drops
// a member's not; keywords beside a $ref other than description and nullable
// are dropped (OpenAPI 3.0 forbids them); const: null reads as no const; a
// schema that only lists properties is typed object; and a nullable schema
// with oneOf admits null. The REST API still validates. Emitting allOf and
// $defs instead of flattening would be the change if a spec needs one of
// these.
func (b *schemaBuilder) convert(s *reference.Schema) *jsonschema.Schema {
	return b.convertSchema(s, true)
}

// convertSchema is convert; with nullable false the schema never admits
// null, as a path argument never does.
//
// A nullable schema is built without null first. With types and no
// alternatives it adds "null" to the types and leaves the enum and const as
// declared: OpenAPI 3.0.3 says nullable adds null to the type only and other
// constraints keep their meaning, so an enum without null still refuses it.
// Null from an explicit {type: "null"} member of oneOf or anyOf (NullMember)
// is valid whatever the enum or const say, so such a schema goes under
// anyOf with {type: "null"}. With nothing at all it is {type: "null"},
// except in OpenAPI 3.0, where nullable without a type means any value, as
// describe_function's any? says; otherwise it becomes anyOf the schema and
// {type: "null"}, which admits null whatever the schema holds.
func (b *schemaBuilder) convertSchema(s *reference.Schema, nullable bool) *jsonschema.Schema {
	out := &jsonschema.Schema{}
	b.nodes++
	if b.nodes > maxSchemaNodes {
		return out
	}
	wrapper := s
	name := b.doc.SchemaName(s)
	resolved, _ := b.doc.Schema(s)
	s = b.doc.Flatten(s)
	if s == nil {
		return out
	}
	if b.visiting[resolved] || (name != "" && b.visiting[name]) {
		// A schema met inside itself is cut to its types, so a oneOf of a
		// string and a recursive Node still tells the two apart.
		return typeStub(s)
	}
	b.visiting[resolved] = true
	defer delete(b.visiting, resolved)
	if name != "" {
		b.visiting[name] = true
		defer delete(b.visiting, name)
	}

	description := wrapper.Description
	if description == "" {
		description = s.Description
	}
	out.Format = s.Format
	types := schemaTypes(s)
	if len(types) == 1 {
		out.Type = types[0]
	}
	if len(types) > 1 {
		out.Types = types
	}
	if len(s.Enum) > 0 {
		out.Enum = append([]any(nil), s.Enum...)
	}
	if s.Const != nil {
		value := s.Const
		out.Const = &value
	}
	if s.Default != nil {
		// A default parsed from the document always encodes again.
		data, err := json.Marshal(s.Default)
		if err == nil {
			out.Default = data
		}
	}
	out.ExclusiveMinimum, out.Minimum = exclusiveBound(s.ExclusiveMinimum, s.Minimum)
	out.ExclusiveMaximum, out.Maximum = exclusiveBound(s.ExclusiveMaximum, s.Maximum)
	out.MultipleOf = s.MultipleOf
	out.MinLength = s.MinLength
	out.MaxLength = s.MaxLength
	out.Pattern = s.Pattern
	out.MinItems = s.MinItems
	out.MaxItems = s.MaxItems
	out.UniqueItems = s.UniqueItems
	out.MinProperties = s.MinProperties
	out.MaxProperties = s.MaxProperties
	if s.Items != nil {
		out.Items = b.convert(s.Items)
	}

	for _, key := range s.Properties.Keys {
		prop := s.Properties.Map[key]
		if readOnly(b.doc, prop) {
			continue
		}
		if out.Properties == nil {
			out.Properties = map[string]*jsonschema.Schema{}
		}
		out.Properties[key] = b.convert(prop)
		out.PropertyOrder = append(out.PropertyOrder, key)
		if s.IsRequired(key) {
			out.Required = append(out.Required, key)
		}
	}
	// A required name without a local property, as in a oneOf member or a
	// not that only says which property must be present, is a constraint
	// on its own and stays.
	for _, key := range s.Required {
		_, local := s.Properties.Map[key]
		if !local && !slices.Contains(out.Required, key) {
			out.Required = append(out.Required, key)
		}
	}
	if s.AdditionalProperties != nil {
		if s.AdditionalProperties.Schema != nil {
			out.AdditionalProperties = b.convert(s.AdditionalProperties.Schema)
		}
		if s.AdditionalProperties.Schema == nil && !s.AdditionalProperties.Allowed {
			out.AdditionalProperties = noSchema()
		}
	}

	for _, member := range s.OneOf {
		out.OneOf = append(out.OneOf, b.convert(member))
	}
	for _, member := range s.AnyOf {
		out.AnyOf = append(out.AnyOf, b.convert(member))
	}
	if s.Not != nil {
		out.Not = b.convert(s.Not)
	}

	if !nullable || (!s.Nullable && !wrapper.Nullable) {
		out.Description = description
		return out
	}
	member := s.NullMember || wrapper.NullMember
	constrained := len(out.Enum) > 0 || out.Const != nil
	if len(types) > 0 && len(out.OneOf) == 0 && len(out.AnyOf) == 0 && !(member && constrained) {
		out.Type = ""
		out.Types = append(types, "null")
		out.Description = description
		return out
	}
	if reflect.DeepEqual(*out, jsonschema.Schema{}) {
		if strings.HasPrefix(b.doc.OpenAPI, "3.0") {
			out.Description = description
			return out
		}
		return &jsonschema.Schema{Type: "null", Description: description}
	}
	return &jsonschema.Schema{Description: description, AnyOf: []*jsonschema.Schema{out, {Type: "null"}}}
}
