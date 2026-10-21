package reference

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
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

// maxExampleModels bounds how many named object schemas a generated example
// nests, the way Redoc's sampler keeps related models from ballooning: the
// root model, its child and its grandchild expand, deeper models render as {}
// (or [] for arrays of them). Inline objects do not count, and explicit
// examples are used at any depth.
const maxExampleModels = 3

// maxExampleValues bounds the values one generated example holds, counting
// each copy of a repeated array item. The model cap does not see unnamed or
// non-object schemas, so a named oneOf whose members hold several fields of
// the next named oneOf grows by its width at every level, up to
// maxExampleDepth levels; past this many values the rest render empty. Real
// examples stay far below it: the longest emma example is under 500 lines.
const maxExampleValues = 5000

// explicitExampleUnit is the JSON size an explicit example counts as one
// value for: an explicit example counts one value per KiB of JSON, so the
// cap also bounds bytes (about maxExampleValues KiB per example).
const explicitExampleUnit = 1024

// ExampleValue builds a sample value for s the way Redoc and Scalar do.
//
// Explicit values win in the order example, examples[0], default, const,
// enum[0]; otherwise a value is generated from the type and format. Objects
// are returned as *Ordered[any] so properties keep their declared order when
// passed to ToJSON. oneOf and anyOf use their first member. A named schema
// that recurs on the current path yields an empty value of its type ({} for
// objects, [] for arrays, nil for scalars). Generated values nest at most
// three named object schemas deep.
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
		err = enc.Encode(fmt.Sprint(v))
	}
	if err != nil {
		// Encoding a plain string cannot fail, but never emit invalid JSON.
		buf.WriteString(strconv.Quote(fmt.Sprint(v)))
		return
	}
	buf.Write(bytes.TrimRight(tmp.Bytes(), "\n"))
}

type exampler struct {
	doc  *Document
	mode ExampleMode
	// stack holds the component names on the current path.
	stack map[string]bool
	// models counts the named object schemas on the current path, at most
	// maxModels (maxExampleModels when 0).
	models    int
	maxModels int
	// values counts the values generated so far, see maxExampleValues.
	values int
	// notes, when set, collects a type note per value in generation order;
	// step and level place the current value, see exampleTypes.
	notes   *[]string
	step    string
	level   int
	display func(string) string // component name to the label the notes use
}

// exampleTypes describes the values of the example generated for s, one
// line per value in document order: the value's depth as that many spaces,
// its step from the parent (a property name, "[]" for array items, "" for
// the root), a tab and a note. A note is "<kind>:<label>", with a trailing
// "!" when the value is the author's own example rather than a generated
// one. Kinds are s string, f formatted string, n number, b boolean, e enum,
// o object, m model (a component schema object, labelled display(name)),
// a array, l list of models and x anything. An array's label is its item's
// with "[]"; an enum's is "enum", or its name when named. A value the spec
// describes ends its note with a tab and the description, see
// noteDescription, and a note with a detail (see noteDetail) adds a tab and
// that, after an empty description if need be. A list of pets reads
// "\tm:ListResponse", " items\tl:Pet[]", "  []\tm:Pet", "   id\tn:int64".
func (d *Document) exampleTypes(s *Schema, mode ExampleMode, display func(string) string) []string {
	var notes []string
	e := &exampler{doc: d, mode: mode, stack: map[string]bool{}, notes: &notes, display: display}
	e.value(s, 0)
	return notes
}

// modelExample is the example of a component schema s for its own page in
// the model dialog, with its type notes: models it holds stay empty, and the
// inspector grafts in their own examples when they are expanded.
func (d *Document) modelExample(s *Schema, display func(string) string) (any, []string) {
	var notes []string
	e := &exampler{doc: d, mode: ForResponse, stack: map[string]bool{}, notes: &notes, display: display, maxModels: 1}
	v := e.value(s, 0)
	return v, notes
}

// mediaTypes is exampleTypes for the schema of a media type, joined with
// newlines, for the client to label the values of MediaExample. Notes are
// all marked as the author's when the media type carries its own example.
// "" when the media type has no schema.
func (d *Document) mediaTypes(mt *MediaType, mode ExampleMode, display func(string) string) string {
	if mt == nil || mt.Schema == nil {
		return ""
	}
	notes := d.exampleTypes(mt.Schema, mode, display)
	if mt.Example != nil || mt.Examples.Len() > 0 {
		for i, note := range notes {
			notes[i] = ownNote(note)
		}
	}
	return strings.Join(notes, "\n")
}

// note records the type of the value at the current path.
func (e *exampler) note(s *Schema, explicit bool) {
	if e.notes == nil {
		return
	}
	flat := e.doc.Flatten(s)
	if flat == nil {
		return
	}
	kind, label, detail := e.typeNote(s, flat, 0)
	if explicit {
		label += "!"
	}
	note := strings.Repeat(" ", e.level) + e.step + "\t" + kind + ":" + label
	desc := e.noteDescription(s, flat)
	extra := detail.json()
	if desc != "" || extra != "" {
		note += "\t" + desc
	}
	if extra != "" {
		note += "\t" + extra
	}
	*e.notes = append(*e.notes, note)
}

// noteDetail is what a type note's label leaves out, written after the
// description as JSON: Base is the type a named schema that is not a model
// stands for (UnixTimestamp is an int64), Values an enum's values, at most
// maxEnumNote of them, and Descs their descriptions from x-enum-descriptions
// in the same order ("" for a value without one).
type noteDetail struct {
	Base   string   `json:"b,omitempty"`
	Values []any    `json:"v,omitempty"`
	Descs  []string `json:"d,omitempty"`
}

func (d noteDetail) json() string {
	if d.Base == "" && len(d.Values) == 0 {
		return ""
	}
	out, err := json.Marshal(d)
	if err != nil {
		return ""
	}
	return string(out)
}

// noteDescription is the description of the value at the current path, on
// one line. A field holding a model is described by the field, not by the
// model, whose description would repeat on every field that holds it. The
// root is left out: the page shows it with the model or response.
func (e *exampler) noteDescription(s, flat *Schema) string {
	if e.step == "" {
		return ""
	}
	desc := s.Description
	if desc == "" && e.doc.SchemaName(s) == "" {
		desc = flat.Description
	}
	return strings.Join(strings.Fields(desc), " ")
}

// ownNote marks a note as the author's example: "!" after the label, before
// any description.
func ownNote(note string) string {
	head, rest, _ := strings.Cut(note, "\t")
	typ, desc, described := strings.Cut(rest, "\t")
	if !strings.HasSuffix(typ, "!") {
		typ += "!"
	}
	out := head + "\t" + typ
	if described {
		out += "\t" + desc
	}
	return out
}

// typeNote is the kind, short label and detail of s, flattened as flat. A
// named object is a model, kind m, labelled with its display name. Any other
// named schema keeps its kind and is labelled with its display name too, its
// base type in the detail unless it is an enum, whose values say more. An array is labelled as its items with "[]" and
// carries their enum values; a list of models is kind l. depth counts the
// array levels above s: past maxExampleDepth the items are "any", so an
// array that holds itself, such as Node: {items: {$ref: Node}}, ends.
func (e *exampler) typeNote(s, flat *Schema, depth int) (string, string, noteDetail) {
	if depth > maxExampleDepth {
		return "x", "any", noteDetail{}
	}
	kind, label := typeNote(flat)
	var detail noteDetail
	if kind == "e" {
		label = "enum"
		detail.Values, detail.Descs = enumNote(flat)
	}
	if kind == "a" && flat.Items != nil {
		itemKind, itemLabel, itemDetail := e.typeNote(flat.Items, e.doc.Flatten(flat.Items), depth+1)
		if itemKind == "m" {
			kind = "l"
		}
		label = itemLabel + "[]"
		detail = itemDetail
		if detail.Base != "" {
			detail.Base += "[]"
		}
	}
	name := e.doc.SchemaName(s)
	switch {
	case name == "" || kind == "l":
		return kind, label, detail
	case kind == "o":
		return "m", e.display(name), noteDetail{}
	}
	if kind != "e" {
		detail.Base = label
	}
	return kind, e.display(name), detail
}

// maxEnumNote caps the enum values a type note lists.
const maxEnumNote = 50

// enumNote is the values of the enum s, at most maxEnumNote of them, and
// their descriptions, or nil when s describes none. Descriptions come from
// x-enum-descriptions or x-enumDescriptions, either a list in the order of
// the values or a map from value to description.
func enumNote(s *Schema) ([]any, []string) {
	values := s.Enum
	if len(values) > maxEnumNote {
		values = values[:maxEnumNote]
	}
	raw, ok := s.Extensions["x-enum-descriptions"]
	if !ok {
		raw = s.Extensions["x-enumDescriptions"]
	}
	descs := make([]string, len(values))
	described := false
	for i, v := range values {
		var desc any
		switch list := raw.(type) {
		case []any:
			if i < len(list) {
				desc = list[i]
			}
		case map[string]any:
			desc = list[fmt.Sprint(v)]
		}
		text, _ := desc.(string)
		descs[i] = strings.Join(strings.Fields(text), " ")
		if descs[i] != "" {
			described = true
		}
	}
	if !described {
		descs = nil
	}
	return values, descs
}

// typeNote is the kind and short label of a flattened schema.
func typeNote(s *Schema) (string, string) {
	if s == nil {
		return "x", "any"
	}
	if len(s.Enum) > 0 {
		values := s.Enum
		if len(values) > maxEnumNote {
			values = values[:maxEnumNote]
		}
		out, err := json.Marshal(values)
		if err != nil {
			return "e", "[]"
		}
		return "e", string(out)
	}
	t := schemaType(s)
	switch t {
	case "string":
		if s.Format != "" {
			return "f", s.Format
		}
		return "s", "string"
	case "integer", "number":
		if s.Format != "" {
			return "n", s.Format
		}
		return "n", t
	case "boolean":
		return "b", "bool"
	case "object":
		return "o", "object"
	case "array":
		return "a", "array"
	}
	if len(s.OneOf) > 0 || len(s.AnyOf) > 0 {
		return "o", "oneOf"
	}
	return "x", "any"
}

func (e *exampler) value(s *Schema, depth int) any {
	if s == nil {
		return nil
	}

	// 3.1 allows example next to $ref, so look before resolving.
	v, ok := explicitExample(s)
	if ok {
		e.note(s, true)
		return e.explicit(s, v)
	}

	// SchemaName sees through allOf wrappers such as
	// {allOf: [{$ref: Node}], description: ...}, so a model that recurs through
	// one is still caught by the stack and counted by the model cap.
	name := e.doc.SchemaName(s)
	flat := e.doc.Flatten(s)
	if flat == nil {
		return nil
	}

	v, ok = explicitExample(flat)
	e.note(s, ok)
	if ok {
		return e.explicit(flat, v)
	}

	e.values++
	if depth > maxExampleDepth || e.values > maxExampleValues || (name != "" && e.stack[name]) {
		return emptyExample(flat)
	}
	if name != "" {
		e.stack[name] = true
		defer delete(e.stack, name)
	}
	if name == "" || schemaType(flat) != "object" {
		return e.generate(flat, depth)
	}

	if e.models >= e.modelCap() {
		return emptyExample(flat)
	}
	e.models++
	v = e.generate(flat, depth)
	e.models--
	return v
}

// explicit charges the explicit example v of s against maxExampleValues by
// its encoded size, and returns the empty value of s once past the cap.
func (e *exampler) explicit(s *Schema, v any) any {
	var buf bytes.Buffer
	appendJSON(&buf, v)
	e.values += 1 + buf.Len()/explicitExampleUnit
	if e.values <= maxExampleValues {
		return v
	}
	flat := e.doc.Flatten(s)
	if flat == nil {
		return nil
	}
	return emptyExample(flat)
}

func (e *exampler) modelCap() int {
	if e.maxModels > 0 {
		return e.maxModels
	}
	return maxExampleModels
}

// capped reports whether s is a named object schema without an explicit
// example that the model nesting cap would cut off.
func (e *exampler) capped(s *Schema) bool {
	if e.models < e.modelCap() {
		return false
	}
	_, ok := explicitExample(s)
	if ok {
		return false
	}
	if e.doc.SchemaName(s) == "" {
		return false
	}
	flat := e.doc.Flatten(s)
	if flat == nil {
		return false
	}
	_, ok = explicitExample(flat)
	return !ok && schemaType(flat) == "object"
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
	if member == nil {
		// Parse drops nil members, but a schema built in Go may hold one.
		member = &Schema{}
	}

	memberName := e.doc.SchemaName(member)
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
	if ok {
		v = e.explicit(memberFlat, v)
	} else {
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
		out.Set(key, e.at(key, prop, depth+1))
	}

	if s.Properties.Len() == 0 {
		extra := s.AdditionalSchema()
		if extra != nil {
			out.Set("key", e.at("key", extra, depth+1))
		}
	}

	return out
}

// at generates the value of a property ("[]" for array items) one level
// below the current one.
func (e *exampler) at(step string, s *Schema, depth int) any {
	parent := e.step
	e.step = step
	e.level++
	v := e.value(s, depth)
	e.level--
	e.step = parent
	return v
}

func (e *exampler) array(s *Schema, depth int) any {
	if s.Items == nil || e.capped(s.Items) {
		return []any{}
	}

	count := 1
	if s.MinItems != nil && *s.MinItems > count {
		count = min(*s.MinItems, 3)
	}

	before := e.values
	item := e.at("[]", s.Items, depth+1)
	// The item is shared by every copy, but each copy is written out, so
	// copies count too and are dropped once they would pass the cap.
	extra := (e.values - before) * (count - 1)
	if e.values+extra > maxExampleValues {
		count = 1
		extra = 0
	}
	e.values += extra
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
	// An enum's first value that says something: "" is often the zero value
	// of the generator's language rather than a value anyone sends.
	var first any
	for _, v := range s.Enum {
		if v == nil {
			continue
		}
		if v != "" {
			return v, true
		}
		if first == nil {
			first = v
		}
	}
	if first != nil {
		return first, true
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

// maxExampleString caps the length a minLength pads a generated string to,
// so a huge minLength cannot exhaust memory.
const maxExampleString = 1024

func stringExample(s *Schema) string {
	v, ok := formatExamples[s.Format]
	if !ok {
		v = "string"
	}

	if s.MinLength != nil && len(v) > 0 && len(v) < *s.MinLength {
		n := min(*s.MinLength, maxExampleString)
		v = strings.Repeat(v, n/len(v)+1)[:n]
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
