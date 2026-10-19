package apidocs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

// ExampleMode selects which side of the wire an example is for: request
// examples drop readOnly properties, response examples drop writeOnly ones.
type ExampleMode int

const (
	ForRequest ExampleMode = iota
	ForResponse
)

// maxExampleDepth bounds nesting for schemas whose recursion is not caught by
// the named-schema check, such as inline allOf members that point back at an
// ancestor.
const maxExampleDepth = 12

// ExampleValue builds a sample value for s the way Redoc and Scalar do.
//
// Explicit values win in the order example, examples[0], default, const,
// enum[0]; otherwise a value is generated from the type and format. Objects
// are returned as *Ordered[any] so properties keep their declared order when
// passed to ToJSON. oneOf and anyOf use their first member. A named schema
// that recurs on the current path yields an empty value of its type ({} for
// objects, [] for arrays, nil for scalars).
func (d *Document) ExampleValue(s *Schema, mode ExampleMode) any {
	e := &exampler{doc: d, mode: mode, stack: map[string]bool{}}
	return e.value(s, 0)
}

// ExampleJSON renders ExampleValue as indented JSON.
func (d *Document) ExampleJSON(s *Schema, mode ExampleMode) string {
	return ToJSON(d.ExampleValue(s, mode))
}

// MediaExample picks the example for a media type: an explicit example, the
// first named example, or a value generated from the schema. ok is false when
// the media type has none of these.
func (d *Document) MediaExample(mt *MediaType, mode ExampleMode) (any, bool) {
	if mt == nil {
		return nil, false
	}
	if mt.Example != nil {
		return mt.Example, true
	}
	for _, key := range mt.Examples.Keys {
		ex := d.Example(mt.Examples.Map[key])
		if ex != nil && ex.Value != nil {
			return ex.Value, true
		}
	}
	if mt.Schema != nil {
		return d.ExampleValue(mt.Schema, mode), true
	}
	return nil, false
}

// MediaExampleJSON renders MediaExample as indented JSON, or "" when the media
// type has no example.
func (d *Document) MediaExampleJSON(mt *MediaType, mode ExampleMode) string {
	v, ok := d.MediaExample(mt, mode)
	if !ok {
		return ""
	}
	return ToJSON(v)
}

// ToJSON renders an example value as JSON indented with two spaces, keeping
// *Ordered[any] key order and leaving <, > and & unescaped. nil renders as
// null.
func ToJSON(v any) string {
	var compact bytes.Buffer
	appendJSON(&compact, v)

	var out bytes.Buffer
	err := json.Indent(&out, compact.Bytes(), "", "  ")
	if err != nil {
		return compact.String()
	}
	return out.String()
}

// appendJSON writes v as compact JSON. Containers are walked by hand because
// json.Marshal escapes HTML inside custom marshalers, and values decoded from
// YAML may hold map[any]any, which encoding/json rejects.
func appendJSON(buf *bytes.Buffer, v any) {
	switch t := v.(type) {
	case nil:
		buf.WriteString("null")
	case *Ordered[any]:
		if t == nil {
			buf.WriteString("null")
			return
		}
		appendObject(buf, t.Keys, t.Map)
	case Ordered[any]:
		appendObject(buf, t.Keys, t.Map)
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		appendObject(buf, keys, t)
	case map[any]any:
		m := make(map[string]any, len(t))
		keys := make([]string, 0, len(t))
		for k, val := range t {
			key := fmt.Sprint(k)
			m[key] = val
			keys = append(keys, key)
		}
		sort.Strings(keys)
		appendObject(buf, keys, m)
	case []any:
		buf.WriteByte('[')
		for i, item := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			appendJSON(buf, item)
		}
		buf.WriteByte(']')
	default:
		appendScalar(buf, v)
	}
}

func appendObject(buf *bytes.Buffer, keys []string, m map[string]any) {
	buf.WriteByte('{')
	for i, key := range keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		appendScalar(buf, key)
		buf.WriteByte(':')
		appendJSON(buf, m[key])
	}
	buf.WriteByte('}')
}

// appendScalar encodes v with HTML escaping disabled. Values encoding/json
// cannot represent (NaN, channels) fall back to their fmt form as a string.
func appendScalar(buf *bytes.Buffer, v any) {
	var tmp bytes.Buffer
	enc := json.NewEncoder(&tmp)
	enc.SetEscapeHTML(false)
	err := enc.Encode(v)
	if err != nil {
		tmp.Reset()
		enc.Encode(fmt.Sprint(v))
	}
	buf.Write(bytes.TrimRight(tmp.Bytes(), "\n"))
}

type exampler struct {
	doc  *Document
	mode ExampleMode
	// stack holds the component names on the current path.
	stack map[string]bool
}

func (e *exampler) value(s *Schema, depth int) any {
	if s == nil {
		return nil
	}

	// 3.1 allows example next to $ref, so look before resolving.
	v, ok := explicitExample(s)
	if ok {
		return v
	}

	_, name := e.doc.Schema(s)
	flat := e.doc.Flatten(s)
	if flat == nil {
		return nil
	}

	v, ok = explicitExample(flat)
	if ok {
		return v
	}

	if depth > maxExampleDepth || (name != "" && e.stack[name]) {
		return emptyExample(flat)
	}
	if name != "" {
		e.stack[name] = true
		defer delete(e.stack, name)
	}

	return e.generate(flat, depth)
}

// generate builds a value from a flattened schema with no explicit example.
func (e *exampler) generate(s *Schema, depth int) any {
	if len(s.OneOf) > 0 || len(s.AnyOf) > 0 {
		return e.variant(s, depth)
	}

	switch schemaType(s) {
	case "object":
		return e.object(s, depth)
	case "array":
		return e.array(s, depth)
	case "string":
		return stringExample(s)
	case "integer":
		return integerExample(s)
	case "number":
		return numberExample(s)
	case "boolean":
		return true
	}
	return nil
}

// variant generates the first oneOf (else anyOf) member merged with the
// parent, so a parent that declares properties next to bare constraint
// members such as anyOf: [{required: [a]}, {required: [b]}] keeps its shape.
func (e *exampler) variant(s *Schema, depth int) any {
	members := s.OneOf
	if len(members) == 0 {
		members = s.AnyOf
	}
	member := members[0]

	_, memberName := e.doc.Schema(member)
	if memberName != "" && e.stack[memberName] {
		return emptyExample(s)
	}
	if memberName != "" {
		e.stack[memberName] = true
		defer delete(e.stack, memberName)
	}

	memberFlat := e.doc.Flatten(member)
	if memberFlat == nil {
		memberFlat = &Schema{}
	}

	v, ok := explicitExample(memberFlat)
	if !ok {
		merged := *s
		merged.Properties = Ordered[*Schema]{}
		for _, key := range s.Properties.Keys {
			merged.Properties.Set(key, s.Properties.Map[key])
		}
		merged.Required = append([]string(nil), s.Required...)
		if len(s.OneOf) > 0 {
			merged.OneOf = nil
		} else {
			merged.AnyOf = nil
		}
		merged.Discriminator = nil
		mergeInto(&merged, memberFlat)
		v = e.generate(&merged, depth)
	}

	obj, isObject := v.(*Ordered[any])
	if s.Discriminator != nil && s.Discriminator.PropertyName != "" && isObject {
		tag := discriminatorValue(s.Discriminator, member.Ref, memberName)
		if tag != "" {
			obj.Set(s.Discriminator.PropertyName, tag)
		}
	}

	return v
}

// discriminatorValue returns the mapping key that points at the member, or
// the member's component name, which OpenAPI uses as the implicit mapping.
func discriminatorValue(disc *Discriminator, ref, name string) string {
	keys := make([]string, 0, len(disc.Mapping))
	for k := range disc.Mapping {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		target := disc.Mapping[k]
		if (ref != "" && target == ref) || (name != "" && (target == name || RefName(target, refSchemas) == name)) {
			return k
		}
	}
	return name
}

func (e *exampler) object(s *Schema, depth int) any {
	out := &Ordered[any]{Map: map[string]any{}}

	for _, key := range s.Properties.Keys {
		prop := s.Properties.Map[key]
		if prop == nil {
			continue
		}
		resolved := e.doc.Flatten(prop)
		readOnly := prop.ReadOnly || (resolved != nil && resolved.ReadOnly)
		writeOnly := prop.WriteOnly || (resolved != nil && resolved.WriteOnly)
		if e.mode == ForRequest && readOnly {
			continue
		}
		if e.mode == ForResponse && writeOnly {
			continue
		}
		out.Set(key, e.value(prop, depth+1))
	}

	if s.Properties.Len() == 0 {
		extra := s.AdditionalSchema()
		if extra != nil {
			out.Set("key", e.value(extra, depth+1))
		}
	}

	return out
}

func (e *exampler) array(s *Schema, depth int) any {
	if s.Items == nil {
		return []any{}
	}

	count := 1
	if s.MinItems != nil && *s.MinItems > count {
		count = min(*s.MinItems, 3)
	}

	item := e.value(s.Items, depth+1)
	out := make([]any, count)
	for i := range out {
		out[i] = item
	}
	return out
}

// explicitExample returns the first author-provided value of s.
func explicitExample(s *Schema) (any, bool) {
	if s.Example != nil {
		return s.Example, true
	}
	if len(s.Examples) > 0 {
		return s.Examples[0], true
	}
	if s.Default != nil {
		return s.Default, true
	}
	if s.Const != nil {
		return s.Const, true
	}
	for _, v := range s.Enum {
		if v != nil {
			return v, true
		}
	}
	return nil, false
}

// schemaType returns the primary type, inferring object or array for untyped
// schemas that declare properties or items.
func schemaType(s *Schema) string {
	t := s.Type.Primary()
	if t != "" {
		return t
	}
	if s.Properties.Len() > 0 || s.AdditionalSchema() != nil {
		return "object"
	}
	if s.Items != nil {
		return "array"
	}
	return ""
}

// emptyExample is the value used where recursion is cut off.
func emptyExample(s *Schema) any {
	switch schemaType(s) {
	case "object":
		return &Ordered[any]{Map: map[string]any{}}
	case "array":
		return []any{}
	}
	return nil
}

var formatExamples = map[string]string{
	"date-time": "2024-01-15T09:30:00Z",
	"date":      "2024-01-15",
	"time":      "09:30:00Z",
	"uuid":      "123e4567-e89b-12d3-a456-426614174000",
	"email":     "user@example.com",
	"uri":       "https://example.com",
	"url":       "https://example.com",
	"hostname":  "example.com",
	"ipv4":      "192.168.0.1",
	"ipv6":      "2001:db8::1",
	"byte":      "U3dhZ2dlciByb2Nrcw==",
	"binary":    "<binary>",
	"password":  "********",
}

func stringExample(s *Schema) string {
	v, ok := formatExamples[s.Format]
	if !ok {
		v = "string"
	}

	if s.MinLength != nil && len(v) < *s.MinLength {
		v = strings.Repeat(v, *s.MinLength/len(v)+1)[:*s.MinLength]
	}
	if s.MaxLength != nil && *s.MaxLength >= 0 && len(v) > *s.MaxLength {
		v = v[:*s.MaxLength]
	}
	return v
}

func integerExample(s *Schema) int64 {
	if s.Minimum != nil {
		return int64(math.Ceil(*s.Minimum))
	}
	if s.ExclusiveMinimum != nil && s.ExclusiveMinimum.Value != nil {
		return int64(math.Floor(*s.ExclusiveMinimum.Value)) + 1
	}
	return 0
}

func numberExample(s *Schema) float64 {
	if s.Minimum != nil {
		return *s.Minimum
	}
	if s.ExclusiveMinimum != nil && s.ExclusiveMinimum.Value != nil {
		return *s.ExclusiveMinimum.Value + 1
	}
	return 0
}
