// Copied from github.com/danielgtaylor/huma/v2 v2.39.1 (MIT, see LICENSE in this directory); trimmed to what compose uses.

package huma

import (
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"math/bits"
	"net/netip"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ErrSchemaInvalid is sent when there is a problem building the schema.
var ErrSchemaInvalid = errors.New("schema is invalid")

// DefaultArrayNullable controls whether arrays (and, in compose, maps) are
// nullable by default. Set this to `false` to make arrays non-nullable by
// default, but be aware that any `nil` slice or map will still encode as
// `null` in JSON. See also: https://pkg.go.dev/encoding/json#Marshal.
var DefaultArrayNullable = true

// JSON Schema type constants
const (
	TypeBoolean = "boolean"
	TypeInteger = "integer"
	TypeNumber  = "number"
	TypeString  = "string"
	TypeArray   = "array"
	TypeObject  = "object"
)

// Special JSON Schema formats.
var (
	ipAddrType          = reflect.TypeFor[netip.Addr]()
	rawMessageType      = reflect.TypeFor[json.RawMessage]()
	schemaProviderType  = reflect.TypeFor[SchemaProvider]()
	textUnmarshalerType = reflect.TypeFor[encoding.TextUnmarshaler]()
	timeType            = reflect.TypeFor[time.Time]()
)

func deref(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

// Discriminator object when request bodies or response payloads may be one of a
// number of different schemas can be used to aid in serialization,
// deserialization, and validation. The discriminator is a specific object in a
// schema that is used to inform the consumer of the document of an alternative
// schema based on the value associated with it.
type Discriminator struct {
	// PropertyName in the payload that will hold the discriminator value.
	// REQUIRED.
	PropertyName string `yaml:"propertyName"`

	// Mapping object to hold mappings between payload values and schema names or
	// references.
	Mapping map[string]string `yaml:"mapping,omitempty"`
}

func (d *Discriminator) MarshalJSON() ([]byte, error) {
	return marshalJSON([]jsonFieldInfo{
		{"propertyName", d.PropertyName, omitNever},
		{"mapping", d.Mapping, omitEmpty},
	}, nil)
}

// Schema represents a JSON Schema compatible with OpenAPI 3.1. It is extensible
// with your own custom properties. It supports a subset of the full JSON Schema
// spec, designed specifically for use with Go structs.
//
// Typically, you will use a registry and `huma.SchemaFromType` to generate
// schemas for your types.
//
//	// Create a registry and register a type.
//	registry := huma.NewMapRegistry("#/prefix", huma.DefaultSchemaNamer)
//	schema := huma.SchemaFromType(registry, reflect.TypeOf(MyType{}))
//
// Note that the registry may create references for your types.
type Schema struct {
	Type                 string              `yaml:"type,omitempty"`
	Nullable             bool                `yaml:"-"`
	Title                string              `yaml:"title,omitempty"`
	Description          string              `yaml:"description,omitempty"`
	Ref                  string              `yaml:"$ref,omitempty"`
	Format               string              `yaml:"format,omitempty"`
	ContentEncoding      string              `yaml:"contentEncoding,omitempty"`
	Default              any                 `yaml:"default,omitempty"`
	Examples             []any               `yaml:"examples,omitempty"`
	Items                *Schema             `yaml:"items,omitempty"`
	AdditionalProperties any                 `yaml:"additionalProperties,omitempty"`
	Properties           map[string]*Schema  `yaml:"properties,omitempty"`
	Enum                 []any               `yaml:"enum,omitempty"`
	Const                any                 `yaml:"const,omitempty"`
	Minimum              *float64            `yaml:"minimum,omitempty"`
	ExclusiveMinimum     *float64            `yaml:"exclusiveMinimum,omitempty"`
	Maximum              *float64            `yaml:"maximum,omitempty"`
	ExclusiveMaximum     *float64            `yaml:"exclusiveMaximum,omitempty"`
	MultipleOf           *float64            `yaml:"multipleOf,omitempty"`
	MinLength            *int                `yaml:"minLength,omitempty"`
	MaxLength            *int                `yaml:"maxLength,omitempty"`
	Pattern              string              `yaml:"pattern,omitempty"`
	PatternDescription   string              `yaml:"patternDescription,omitempty"`
	MinItems             *int                `yaml:"minItems,omitempty"`
	MaxItems             *int                `yaml:"maxItems,omitempty"`
	UniqueItems          bool                `yaml:"uniqueItems,omitempty"`
	Required             []string            `yaml:"required,omitempty"`
	MinProperties        *int                `yaml:"minProperties,omitempty"`
	MaxProperties        *int                `yaml:"maxProperties,omitempty"`
	ReadOnly             bool                `yaml:"readOnly,omitempty"`
	WriteOnly            bool                `yaml:"writeOnly,omitempty"`
	Deprecated           bool                `yaml:"deprecated,omitempty"`
	Extensions           map[string]any      `yaml:",inline"`
	DependentRequired    map[string][]string `yaml:"dependentRequired,omitempty"`

	OneOf []*Schema `yaml:"oneOf,omitempty"`
	AnyOf []*Schema `yaml:"anyOf,omitempty"`
	AllOf []*Schema `yaml:"allOf,omitempty"`
	Not   *Schema   `yaml:"not,omitempty"`

	// OpenAPI specific fields
	Discriminator *Discriminator `yaml:"discriminator,omitempty"`

	hidden bool `yaml:"-"`
}

// MarshalJSON marshals the schema into JSON, respecting the `Extensions` map
// to marshal extensions inline.
func (s *Schema) MarshalJSON() ([]byte, error) {
	var typ any = s.Type
	if s.Nullable {
		typ = []string{s.Type, "null"}
	}

	var contentMediaType string
	if s.Format == "binary" {
		contentMediaType = "application/octet-stream"
	}

	props := s.Properties
	for _, ps := range props {
		if ps.hidden {
			// Copy the map to avoid modifying the original schema.
			props = make(map[string]*Schema, len(s.Properties))
			for k, v := range s.Properties {
				if !v.hidden {
					props[k] = v
				}
			}
			break
		}
	}

	return marshalJSON([]jsonFieldInfo{
		{"type", typ, omitEmpty},
		{"title", s.Title, omitEmpty},
		{"description", s.Description, omitEmpty},
		{"$ref", s.Ref, omitEmpty},
		{"format", s.Format, omitEmpty},
		{"contentMediaType", contentMediaType, omitEmpty},
		{"contentEncoding", s.ContentEncoding, omitEmpty},
		{"default", s.Default, omitNil},
		{"examples", s.Examples, omitEmpty},
		{"items", s.Items, omitEmpty},
		{"additionalProperties", s.AdditionalProperties, omitNil},
		{"properties", props, omitEmpty},
		{"enum", s.Enum, omitEmpty},
		{"const", s.Const, omitNil},
		{"minimum", s.Minimum, omitEmpty},
		{"exclusiveMinimum", s.ExclusiveMinimum, omitEmpty},
		{"maximum", s.Maximum, omitEmpty},
		{"exclusiveMaximum", s.ExclusiveMaximum, omitEmpty},
		{"multipleOf", s.MultipleOf, omitEmpty},
		{"minLength", s.MinLength, omitEmpty},
		{"maxLength", s.MaxLength, omitEmpty},
		{"pattern", s.Pattern, omitEmpty},
		{"patternDescription", s.PatternDescription, omitEmpty},
		{"minItems", s.MinItems, omitEmpty},
		{"maxItems", s.MaxItems, omitEmpty},
		{"uniqueItems", s.UniqueItems, omitEmpty},
		{"required", s.Required, omitEmpty},
		{"dependentRequired", s.DependentRequired, omitEmpty},
		{"minProperties", s.MinProperties, omitEmpty},
		{"maxProperties", s.MaxProperties, omitEmpty},
		{"readOnly", s.ReadOnly, omitEmpty},
		{"writeOnly", s.WriteOnly, omitEmpty},
		{"deprecated", s.Deprecated, omitEmpty},
		{"oneOf", s.OneOf, omitEmpty},
		{"anyOf", s.AnyOf, omitEmpty},
		{"allOf", s.AllOf, omitEmpty},
		{"not", s.Not, omitEmpty},
		{"discriminator", s.Discriminator, omitEmpty},
	}, s.Extensions)
}

// Unhide clears the hidden flag a hidden:"true" field tag set, so the
// property is marshalled again. compose: the strict contract lists hidden
// properties, which a response still sends.
func (s *Schema) Unhide() {
	s.hidden = false
}

// compilePatterns panics when a pattern is not a valid Go regular
// expression. huma does this in PrecomputeMessages, which precomputes
// huma.Validate's error messages; compose never calls huma.Validate, so the
// copy keeps only the check.
func (s *Schema) compilePatterns() {
	if s.Pattern != "" {
		regexp.MustCompile(s.Pattern)
	}

	if s.Items != nil {
		s.Items.compilePatterns()
	}

	for _, prop := range s.Properties {
		prop.compilePatterns()
	}

	for _, sub := range s.OneOf {
		sub.compilePatterns()
	}

	for _, sub := range s.AnyOf {
		sub.compilePatterns()
	}

	for _, sub := range s.AllOf {
		sub.compilePatterns()
	}

	if s.Not != nil {
		s.Not.compilePatterns()
	}
}

func boolTag(f reflect.StructField, tag string, def bool) bool {
	if v := f.Tag.Get(tag); v != "" {
		switch v {
		case "true":
			return true
		case "false":
			return false
		default:
			panic(fmt.Errorf("invalid bool tag '%s' for field '%s': %v", tag, f.Name, v))
		}
	}
	return def
}

func intTag(f reflect.StructField, tag string) *int {
	if v := f.Tag.Get(tag); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return &i
		} else {
			panic(fmt.Errorf("invalid int tag '%s' for field '%s': %v (%w)", tag, f.Name, v, err))
		}
	}
	return nil
}

func floatTag(f reflect.StructField, tag string) *float64 {
	if v := f.Tag.Get(tag); v != "" {
		if i, err := strconv.ParseFloat(v, 64); err == nil {
			return &i
		} else {
			panic(fmt.Errorf("invalid float tag '%s' for field '%s': %v (%w)", tag, f.Name, v, err))
		}
	}
	return nil
}

func stringTag(f reflect.StructField, tag string, def string) string {
	if v := f.Tag.Get(tag); v != "" {
		return v
	}
	return def
}

// ensureType panics if the given value does not match the JSON Schema type.
func ensureType(r Registry, fieldName string, s *Schema, value string, v any) {
	if s.Ref != "" {
		s = r.SchemaFromRef(s.Ref)
		if s == nil {
			// We may not have access to this type, e.g., custom schema provided
			// by the user with remote refs. Skip validation.
			return
		}
	}

	switch s.Type {
	case TypeBoolean:
		if _, ok := v.(bool); !ok {
			panic(fmt.Errorf("invalid boolean tag value '%s' for field '%s': %w", value, fieldName, ErrSchemaInvalid))
		}
	case TypeInteger, TypeNumber:
		if _, ok := v.(float64); !ok {
			panic(fmt.Errorf("invalid number tag value '%s' for field '%s': %w", value, fieldName, ErrSchemaInvalid))
		}

		if s.Type == TypeInteger {
			if v.(float64) != float64(int(v.(float64))) {
				panic(fmt.Errorf("invalid integer tag value '%s' for field '%s': %w", value, fieldName, ErrSchemaInvalid))
			}
		}
	case TypeString:
		if _, ok := v.(string); !ok {
			panic(fmt.Errorf("invalid string tag value '%s' for field '%s': %w", value, fieldName, ErrSchemaInvalid))
		}
	case TypeArray:
		if _, ok := v.([]any); !ok {
			panic(fmt.Errorf("invalid array tag value '%s' for field '%s': %w", value, fieldName, ErrSchemaInvalid))
		}

		if s.Items != nil {
			for i, item := range v.([]any) {
				b, _ := json.Marshal(item)
				ensureType(r, fieldName+"["+strconv.Itoa(i)+"]", s.Items, string(b), item)
			}
		}
	case TypeObject:
		if _, ok := v.(map[string]any); !ok {
			panic(fmt.Errorf("invalid object tag value '%s' for field '%s': %w", value, fieldName, ErrSchemaInvalid))
		}

		for name, prop := range s.Properties {
			if val, ok := v.(map[string]any)[name]; ok {
				b, _ := json.Marshal(val)
				ensureType(r, fieldName+"."+name, prop, string(b), val)
			}
		}
	}
}

// convertType panics if the given value does not match or cannot be converted
// to the field's Go type.
func convertType(fieldName string, t reflect.Type, v any) any {
	if v == nil {
		return v
	}

	tv := reflect.TypeOf(v)
	if tv == t {
		return v
	}

	// Directly convert equal underlying types, avoiding traversal.
	// e.g., json.RawMessage -> []byte.
	if tv.ConvertibleTo(t) {
		return reflect.ValueOf(v).Convert(t).Interface()
	}

	val := reflect.ValueOf(v)

	if tv.Kind() == reflect.Slice {
		// Slices can't be cast due to the different layouts. Instead, we make a
		// new instance of the destination slice, and convert each value in
		// the original to the new type.
		tmp := reflect.MakeSlice(t, 0, val.Len())
		for i := 0; i < val.Len(); i++ {
			item := val.Index(i)
			if item.Kind() == reflect.Interface {
				// E.g. []any and we want the underlying type.
				item = item.Elem()
			}

			item = reflect.Indirect(item)
			typ := deref(t.Elem())
			if !item.Type().ConvertibleTo(typ) {
				panic(fmt.Errorf("unable to convert %v to %v for field '%s': %w", item.Interface(), t.Elem(), fieldName, ErrSchemaInvalid))
			}

			value := item.Convert(typ)
			if t.Elem().Kind() == reflect.Pointer {
				// Special case: if the field is a pointer, we need to get a pointer
				// to the converted value.
				ptr := reflect.New(value.Type())
				ptr.Elem().Set(value)
				value = ptr
			}

			tmp = reflect.Append(tmp, value)
		}

		return tmp.Interface()
	}

	if !tv.ConvertibleTo(deref(t)) {
		panic(fmt.Errorf("unable to convert %v to %v for field '%s': %w", tv, t, fieldName, ErrSchemaInvalid))
	}

	converted := val.Convert(deref(t))
	if t.Kind() == reflect.Pointer {
		// Special case: if the field is a pointer, we need to get a pointer
		// to the converted value.
		tmp := reflect.New(t.Elem())
		tmp.Elem().Set(converted)
		converted = tmp
	}

	return converted.Interface()
}

func jsonTagValue(r Registry, fieldName string, s *Schema, value string) any {
	if s.Ref != "" {
		s = r.SchemaFromRef(s.Ref)
		if s == nil {
			return nil
		}
	}

	// Special case: strings don't need quotes.
	if s.Type == TypeString {
		return value
	}

	// Special case: array of strings with comma-separated values and no quotes.
	if s.Type == TypeArray && s.Items != nil && s.Items.Type == TypeString && value[0] != '[' {
		var values []string
		for s := range strings.SplitSeq(value, ",") {
			values = append(values, strings.TrimSpace(s))
		}
		return values
	}

	var v any
	if err := json.Unmarshal([]byte(value), &v); err != nil {
		panic(fmt.Errorf("invalid %s tag value '%s' for field '%s': %w", s.Type, value, fieldName, err))
	}

	ensureType(r, fieldName, s, value, v)

	return v
}

// jsonTag returns a value of the schema's type for the given tag string.
// Uses JSON parsing if the schema is not a string.
func jsonTag(r Registry, f reflect.StructField, s *Schema, name string) any {
	t := f.Type
	if value := f.Tag.Get(name); value != "" {
		return convertType(f.Name, t, jsonTagValue(r, f.Name, s, value))
	}
	return nil
}

// SchemaFromField generates a schema for a given struct field. If the field
// is a struct (or slice/map of structs) then the registry is used to
// potentially get a reference to that type.
//
// This is used by `huma.SchemaFromType` when it encounters a struct, and
// is used to generate schemas for path/query/header parameters.
func SchemaFromField(registry Registry, f reflect.StructField, hint string) *Schema {
	fs := registry.Schema(f.Type, true, hint)
	if fs == nil {
		return fs
	}
	fs.Description = stringTag(f, "doc", fs.Description)
	if fs.Format == "date-time" && f.Tag.Get("header") != "" {
		// Special case: this is a header and uses a different date/time format.
		// Note that it can still be overridden by the `format` or `timeFormat`
		// tags later.
		fs.Format = "date-time-http"
	}
	if format := f.Tag.Get("format"); format != "" {
		targetSchema(fs).Format = format
	}
	if timeFmt := f.Tag.Get("timeFormat"); timeFmt != "" {
		s := targetSchema(fs)
		switch timeFmt {
		case "2006-01-02":
			s.Format = "date"
		case "15:04:05":
			s.Format = "time"
		default:
			s.Format = timeFmt
		}
	}
	fs.ContentEncoding = stringTag(f, "encoding", fs.ContentEncoding)
	if defaultValue := jsonTag(registry, f, fs, "default"); defaultValue != nil {
		fs.Default = defaultValue
	}

	if value, ok := f.Tag.Lookup("example"); ok {
		if e := jsonTagValue(registry, f.Name, fs, value); e != nil {
			fs.Examples = []any{e}
		}
	}

	if enum := f.Tag.Get("enum"); enum != "" {
		s := targetSchema(fs)
		var enumValues []any
		for e := range strings.SplitSeq(enum, ",") {
			enumValues = append(enumValues, jsonTagValue(registry, f.Name, s, e))
		}
		s.Enum = enumValues
	}

	if constValue := f.Tag.Get("const"); constValue != "" {
		s := targetSchema(fs)
		s.Const = jsonTagValue(registry, f.Name, s, constValue)
	}

	fs.Nullable = boolTag(f, "nullable", fs.Nullable)
	if fs.Nullable && fs.Ref != "" && registry.SchemaFromRef(fs.Ref).Type == "object" {
		// Nullability is only supported for scalar types for now. Objects are
		// much more complicated. The `null` type lives within the object
		// definition (requiring multiple copies of the object) or needs to use
		// `anyOf` or `not` which is not supported by all code generators, or is
		// supported poorly & generates hard-to-use code. This is less than ideal
		// but a compromise for now to support some nullability built-in.
		panic(fmt.Errorf("nullable is not supported for field '%s' which is type '%s'", f.Name, fs.Ref))
	}

	if v := floatTag(f, "minimum"); v != nil {
		targetSchema(fs).Minimum = v
	}
	if v := floatTag(f, "exclusiveMinimum"); v != nil {
		targetSchema(fs).ExclusiveMinimum = v
	}
	if v := floatTag(f, "maximum"); v != nil {
		targetSchema(fs).Maximum = v
	}
	if v := floatTag(f, "exclusiveMaximum"); v != nil {
		targetSchema(fs).ExclusiveMaximum = v
	}
	if v := floatTag(f, "multipleOf"); v != nil {
		targetSchema(fs).MultipleOf = v
	}
	if v := intTag(f, "minLength"); v != nil {
		targetSchema(fs).MinLength = v
	}
	if v := intTag(f, "maxLength"); v != nil {
		targetSchema(fs).MaxLength = v
	}
	if v := f.Tag.Get("pattern"); v != "" {
		targetSchema(fs).Pattern = v
	}
	if v := f.Tag.Get("patternDescription"); v != "" {
		targetSchema(fs).PatternDescription = v
	}
	if v := intTag(f, "minItems"); v != nil {
		fs.MinItems = v
	}
	if v := intTag(f, "maxItems"); v != nil {
		fs.MaxItems = v
	}
	if v := intTag(f, "minProperties"); v != nil {
		fs.MinProperties = v
	}
	if v := intTag(f, "maxProperties"); v != nil {
		fs.MaxProperties = v
	}
	fs.UniqueItems = boolTag(f, "uniqueItems", fs.UniqueItems)
	fs.ReadOnly = boolTag(f, "readOnly", fs.ReadOnly)
	fs.WriteOnly = boolTag(f, "writeOnly", fs.WriteOnly)
	fs.Deprecated = boolTag(f, "deprecated", fs.Deprecated)
	fs.compilePatterns()

	fs.hidden = boolTag(f, "hidden", fs.hidden)

	return fs
}

// targetSchema returns the schema to apply scalar constraints to
// (the items schema for arrays, otherwise the schema itself).
func targetSchema(fs *Schema) *Schema {
	if fs.Type == TypeArray {
		return fs.Items
	}
	return fs
}

// fieldInfo stores information about a field, which may come from an
// embedded type. The `Parent` stores the field's direct parent.
type fieldInfo struct {
	Parent reflect.Type
	Field  reflect.StructField
}

// getFields performs a breadth-first search for all fields, including embedded
// ones. It may return multiple fields with the same name, the first of which
// represents the outermost declaration.
func getFields(typ reflect.Type, visited map[reflect.Type]struct{}) []fieldInfo {
	fields := make([]fieldInfo, 0, typ.NumField())
	var embedded []reflect.StructField

	if _, ok := visited[typ]; ok {
		return fields
	}
	visited[typ] = struct{}{}

	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if !f.IsExported() {
			continue
		}

		// An anonymous (embedded) field is merged into the parent object when
		// it has no explicit JSON name. This covers both an absent tag and a
		// name-less tag such as `json:",inline"` or `json:",omitempty"`. Giving
		// the field a name (e.g. `json:"meta"`) makes it a nested object
		// instead, matching encoding/json's behavior.
		jsonName, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if f.Anonymous && jsonName == "" {
			embedded = append(embedded, f)
			continue
		}

		fields = append(fields, fieldInfo{typ, f})
	}

	for _, f := range embedded {
		newTyp := f.Type
		for newTyp.Kind() == reflect.Pointer {
			newTyp = newTyp.Elem()
		}
		if newTyp.Kind() == reflect.Struct {
			fields = append(fields, getFields(newTyp, visited)...)
		}
	}

	return fields
}

// SchemaProvider is an interface that can be implemented by types to provide
// a custom schema for themselves, overriding the built-in schema generation.
// This can be used by custom types with their own special serialization rules.
type SchemaProvider interface {
	Schema(r Registry) *Schema
}

// SchemaFromType returns a schema for a given type, using the registry to
// possibly create references for nested structs.
//
//	// Create a registry and register a type.
//	registry := huma.NewMapRegistry("#/prefix", huma.DefaultSchemaNamer)
//	schema := huma.SchemaFromType(registry, reflect.TypeOf(MyType{}))
func SchemaFromType(r Registry, t reflect.Type) *Schema {
	isPointer := t.Kind() == reflect.Pointer

	s := Schema{}
	t = deref(t)

	ptrT := reflect.PointerTo(t)
	if t.Implements(schemaProviderType) || ptrT.Implements(schemaProviderType) {
		// Special case: type provides its own schema. Do not try to generate.
		custom := reflect.New(t).Interface().(SchemaProvider).Schema(r)
		custom.compilePatterns()
		return custom
	}

	// Handle special cases for known stdlib types. compose: net.IP is a
	// builtin of the openapi registry, and url.URL is refused (encoding/json
	// reads and writes it as an object).
	switch t {
	case timeType:
		return &Schema{Type: TypeString, Nullable: isPointer, Format: "date-time"}
	case ipAddrType:
		return &Schema{Type: TypeString, Nullable: isPointer, Format: "ip"}
	case rawMessageType:
		return &Schema{}
	}

	if t.Implements(textUnmarshalerType) || ptrT.Implements(textUnmarshalerType) {
		// Special case: types that implement encoding.TextUnmarshaler are able to
		// be loaded from plain text, and so should be treated as strings.
		// This behavior can be overridden by implementing `huma.SchemaProvider`
		// and returning a custom schema.
		return &Schema{Type: TypeString, Nullable: isPointer}
	}

	minZero := 0.0
	switch t.Kind() {
	case reflect.Bool:
		s.Type = TypeBoolean
	case reflect.Int:
		s.Type = TypeInteger
		if bits.UintSize == 32 {
			s.Format = "int32"
		} else {
			s.Format = "int64"
		}
	case reflect.Int8, reflect.Int16, reflect.Int32:
		s.Type = TypeInteger
		s.Format = "int32"
	case reflect.Int64:
		s.Type = TypeInteger
		s.Format = "int64"
	case reflect.Uint:
		s.Type = TypeInteger
		if bits.UintSize == 32 {
			s.Format = "int32"
		} else {
			s.Format = "int64"
		}
		s.Minimum = &minZero
	case reflect.Uint8, reflect.Uint16, reflect.Uint32:
		// Unsigned integers can't be negative.
		s.Type = TypeInteger
		s.Format = "int32"
		s.Minimum = &minZero
	case reflect.Uint64:
		// Unsigned integers can't be negative.
		s.Type = TypeInteger
		s.Format = "int64"
		s.Minimum = &minZero
	case reflect.Float32:
		s.Type = TypeNumber
		s.Format = "float"
	case reflect.Float64:
		s.Type = TypeNumber
		s.Format = "double"
	case reflect.String:
		s.Type = TypeString
	case reflect.Slice, reflect.Array:
		if t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8 {
			// Special case: []byte will be serialized as a base64 string.
			// compose: or as null when nil, so it is as nullable as any
			// other slice. A [N]byte array is a JSON array of numbers.
			s.Type = TypeString
			s.ContentEncoding = "base64"
			s.Nullable = DefaultArrayNullable
			return &s
		}
		s.Type = TypeArray
		s.Nullable = DefaultArrayNullable
		s.Items = r.Schema(t.Elem(), true, t.Name()+"Item")

		if t.Kind() == reflect.Array {
			l := t.Len()
			s.MinItems = &l
			s.MaxItems = &l
		}
	case reflect.Map:
		s.Type = TypeObject
		// compose: a nil map encodes as `null` like a nil slice, so maps
		// follow the array default (huma leaves them non-nullable).
		s.Nullable = DefaultArrayNullable
		s.AdditionalProperties = r.Schema(t.Elem(), true, t.Name()+"Value")
	case reflect.Struct:
		var required []string
		fieldSet := map[string]struct{}{}
		props := map[string]*Schema{}
		dependentRequiredMap := map[string][]string{}
		for _, info := range getFields(t, make(map[reflect.Type]struct{})) {
			f := info.Field

			if _, ok := fieldSet[f.Name]; ok {
				// This field was overridden by an ancestor type, so we
				// should ignore it.
				continue
			}

			fieldSet[f.Name] = struct{}{}

			// Controls whether the field is required or not. All fields start as
			// required, then can be made optional with the `omitempty` JSON tag,
			// `omitzero` JSON tag, or it can be overridden manually via the
			// `required` tag.
			fieldRequired := true

			name := f.Name
			if j := f.Tag.Get("json"); j != "" {
				if n := strings.Split(j, ",")[0]; n != "" {
					name = n
				}
				if strings.Contains(j, "omitempty") {
					fieldRequired = false
				}
				if strings.Contains(j, "omitzero") {
					fieldRequired = false
				}
			}
			if name == "-" {
				// This field is deliberately ignored.
				continue
			}

			if _, ok := f.Tag.Lookup("required"); ok {
				fieldRequired = boolTag(f, "required", false)
			}

			if dr := f.Tag.Get("dependentRequired"); strings.TrimSpace(dr) != "" {
				dependentRequiredMap[name] = strings.Split(dr, ",")
			}

			fs := SchemaFromField(r, f, t.Name()+f.Name+"Struct")
			if fs != nil {
				props[name] = fs

				if fs.hidden {
					// This field is deliberately ignored. It may still exist but won't
					// be documented as a required field.
					fieldRequired = false
				}

				if fieldRequired {
					required = append(required, name)
				}

				// Special case: pointer with omitempty and not manually set to
				// nullable, which will never get `null` sent over the wire.
				if f.Type.Kind() == reflect.Pointer && strings.Contains(f.Tag.Get("json"), "omitempty") && f.Tag.Get("nullable") != "true" {
					fs.Nullable = false
				}
			}
		}
		s.Type = TypeObject

		// Check if the dependent fields exist. If they don't, panic with the correct message.
		var errs []string
		depKeys := make([]string, 0, len(dependentRequiredMap))
		for field := range dependentRequiredMap {
			depKeys = append(depKeys, field)
		}
		sort.Strings(depKeys)
		for _, field := range depKeys {
			dependents := dependentRequiredMap[field]
			for _, dependent := range dependents {
				if _, ok := props[dependent]; ok {
					continue
				}
				errs = append(errs, fmt.Sprintf("dependent field '%s' for field '%s' does not exist", dependent, field))
			}
		}
		if errs != nil {
			panic(errors.New(strings.Join(errs, "; ")))
		}

		additionalProps := false
		if f, ok := t.FieldByName("_"); ok {
			if _, ok = f.Tag.Lookup("additionalProperties"); ok {
				additionalProps = boolTag(f, "additionalProperties", false)
			}

			if _, ok = f.Tag.Lookup("nullable"); ok {
				// Allow overriding nullability per struct.
				s.Nullable = boolTag(f, "nullable", false)
			}
		}

		s.AdditionalProperties = additionalProps
		s.Properties = props
		s.Required = required
		s.DependentRequired = dependentRequiredMap
		s.compilePatterns()
	case reflect.Interface:
		// Interfaces mean any object.
	default:
		return nil
	}

	switch s.Type {
	case TypeBoolean, TypeInteger, TypeNumber, TypeString:
		// Scalar types which are pointers are nullable by default. This can be
		// overridden via the `nullable:"false"` field tag in structs.
		s.Nullable = isPointer
	}

	return &s
}
