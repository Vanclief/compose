package openapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/vanclief/ez"

	"github.com/vanclief/compose/components/rest/api/internal/huma"
)

// SchemaProvider is a type that says what its JSON is: JSONSchema returns
// the JSON Schema of the type's JSON as JSON text. Apps know it as
// api.SchemaProvider, which documents it.
type SchemaProvider interface {
	JSONSchema(refs *SchemaRefs) string
}

// SchemaRefs gives a SchemaProvider the schema of another Go type. Apps know
// it as api.SchemaRefs. It serves the JSONSchema call it is passed to, and
// returns {} after it.
type SchemaRefs struct {
	reg   *registry
	owner reflect.Type
	// refs holds every $ref For returned, which the text may use.
	refs  map[string]bool
	asked []reflect.Type
	err   error
}

// For returns, as JSON text, the schema of the Go type t as the registry
// publishes it: for a type the document holds as a component (a struct
// documented by its fields, an enum, a type with a JSONSchema method),
// {"$ref":"#/components/schemas/<Name>"},
// and for a pointer to an enum or to a type with a JSONSchema method, or a
// types.Optional of any of these, anyOf [{"$ref": ...}, {"type": "null"}];
// for any other, its inline schema, whose $refs the text may use as well. A
// nil *SchemaRefs, as in a unit test of a JSONSchema method, returns {}.
// When the registry cannot give the schema, For returns {} and registration
// fails with the reason.
func (r *SchemaRefs) For(t reflect.Type) string {
	if r == nil || r.reg == nil || r.err != nil {
		return "{}"
	}
	if t == nil {
		r.err = ez.New(ez.EINVALID, fmt.Sprintf("%s: its JSONSchema method called refs.For with a nil type", r.owner), nil)
		return "{}"
	}
	r.asked = append(r.asked, t)

	s, err := r.reg.schemaFor(t, r.owner)
	if err != nil {
		r.err = err
		return "{}"
	}
	walkSchema(s, func(n *huma.Schema) {
		if n.Ref != "" {
			r.refs[n.Ref] = true
		}
	})
	raw, err := json.Marshal(s)
	if err != nil {
		r.err = ez.New(ez.EINTERNAL, fmt.Sprintf("%s: could not encode the schema of %s for refs.For", r.owner, t), err)
		return "{}"
	}
	return string(raw)
}

// schemaFor returns the schema of t for For, with null written as the
// published documents write it (see normalizeNull). A panic of the registry
// is returned as an error, so it does not cross the app's method; one of
// another JSONSchema method goes on. A schema that refers to a type whose
// JSONSchema method is running is refused when no struct component has
// begun to be built since that method was called: the type would ask for
// its own schema. Through the properties of a struct the reference is the
// struct's, as it would be had the struct been built first.
func (r *registry) schemaFor(t reflect.Type, owner reflect.Type) (s *huma.Schema, err error) {
	defer func() {
		rec := recover()
		if rec == nil {
			return
		}
		_, app := rec.(appPanic)
		if app {
			panic(rec)
		}
		ezErr, ok := rec.(*ez.Error)
		if ok {
			err = ezErr
			return
		}
		err = ez.New(ez.EINVALID, fmt.Sprintf("%s: refs.For(%s): %v", owner, t, rec), nil)
	}()

	s = copySchema(r.Schema(t, true, owner.Name()+"Schema"))
	if s == nil {
		return nil, ez.New(ez.EINVALID, fmt.Sprintf("%s: its JSONSchema method asked refs.For for %s, which has no JSON schema", owner, t), nil)
	}
	walkSchema(s, normalizeNull)
	for _, name := range refsOf(s) {
		running := r.types[name]
		structs, ok := r.building[running]
		if ok && structs == r.structs {
			return nil, ez.New(ez.EINVALID, fmt.Sprintf("%s: its JSONSchema method asked refs.For for %s, which refers to %s, whose JSONSchema method is running; a JSONSchema method cannot ask for its own type's schema, directly or through another type's JSONSchema method", owner, t, running), nil)
		}
	}
	return s, nil
}

// providedSchema returns the $ref to the component of t, a type with a
// JSONSchema method, built from the text the method returns on first use.
// A pointer marks it nullable, as an enum's; Finalize writes it as
// anyOf [ref, null]. The placeholder registered first lets a struct the
// method asks for hold t again. A struct that gets the method from an
// embedded field is refused before the method is called: through an
// embedded pointer, which is nil, the call would panic.
func (r *registry) providedSchema(t reflect.Type, pointer bool, hint string) *huma.Schema {
	name, ok := r.names[t]
	if ok {
		return &huma.Schema{Ref: RefPrefix + name, Nullable: pointer}
	}
	if t.Kind() == reflect.Struct {
		embedded, promoted := promotedSchema(t)
		if promoted {
			panic(ez.New(ez.EINVALID, promotedSchemaProblem(t, embedded), nil))
		}
	}

	name = r.schemaName(t, hint)
	_, exists := r.schemas[name]
	if exists {
		if t.Name() != "" {
			panic(ez.New(ez.EINVALID, fmt.Sprintf("duplicate component name %s for %s and %s%s", name, t, r.types[name], collisionHint), nil))
		}
		name = r.freeName(name)
	}
	r.schemas[name] = &huma.Schema{}
	r.types[name] = t
	r.names[t] = name

	r.building[t] = r.structs
	defer delete(r.building, t)
	refs := &SchemaRefs{reg: r, owner: t, refs: map[string]bool{}}
	text := callProvider(t, refs)
	refs.reg = nil
	if refs.err != nil {
		panic(refs.err)
	}

	s, err := parseSchemaText(text, refs.refs)
	if err != nil {
		panic(ez.New(ez.EINVALID, fmt.Sprintf("%s: %s", t, ez.ErrorMessage(err)), err))
	}
	r.schemas[name] = s
	r.asks[t] = refs.asked
	return &huma.Schema{Ref: RefPrefix + name, Nullable: pointer}
}

// appPanic carries a panic of an app's JSONSchema method past the recovers
// that turn the registry's own panics into errors (see recoverPanic).
type appPanic struct {
	value any
}

// callProvider calls the JSONSchema method of t. A panic in it is the app's
// and goes on as an appPanic; the stack it was raised on is still there.
func callProvider(t reflect.Type, refs *SchemaRefs) string {
	returned := false
	defer func() {
		if returned {
			return
		}
		rec := recover()
		if rec == nil {
			// runtime.Goexit, which goes on by itself.
			return
		}
		_, ok := rec.(appPanic)
		if !ok {
			rec = appPanic{value: rec}
		}
		panic(rec)
	}()
	text := reflect.New(t).Interface().(SchemaProvider).JSONSchema(refs)
	returned = true
	return text
}

// textError is a problem with a JSONSchema text at a JSON pointer.
type textError struct {
	pointer string
	msg     string
}

func fail(pointer string, format string, args ...any) {
	panic(textError{pointer: pointer, msg: fmt.Sprintf(format, args...)})
}

// annotations are the keywords that say nothing of which values a schema
// accepts.
var annotations = map[string]bool{
	"title": true, "description": true, "default": true, "examples": true, "example": true,
	"deprecated": true,
}

// parseSchemaText builds the schema the JSONSchema text describes, refusing
// what the registry would not publish and validate as written: text that is
// not one JSON object or holds a key twice, a keyword other than those
// schema reads, a $ref For did not return (refs), null written in a way
// OpenAPI 3.0 cannot carry, and what the OpenAPI 3.0 document would carry
// otherwise than written.
func parseSchemaText(text string, refs map[string]bool) (s *huma.Schema, err error) {
	defer func() {
		rec := recover()
		if rec == nil {
			return
		}
		te, ok := rec.(textError)
		if !ok {
			panic(rec)
		}
		where := "the JSONSchema text"
		if te.pointer != "" {
			where += " at " + te.pointer
		}
		err = ez.New(ez.EINVALID, where+": "+te.msg, nil)
	}()

	if !utf8.ValidString(text) {
		fail("", "it is not valid UTF-8")
	}
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	v := decodeValue(dec, "")
	_, err = dec.Token()
	if !errors.Is(err, io.EOF) {
		fail("", "it holds more after the JSON value")
	}
	p := &textParser{refs: refs}
	return p.schema(v, "", false), nil
}

// decodeValue decodes the next JSON value of dec, at pointer, refusing an
// object that holds a key twice.
func decodeValue(dec *json.Decoder, pointer string) any {
	tok := nextToken(dec, pointer)
	n, isNumber := tok.(json.Number)
	if isNumber {
		exactNumber(n, pointer)
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return tok
	}

	var v any
	switch delim {
	case '{':
		obj := map[string]any{}
		for dec.More() {
			key := nextToken(dec, pointer).(string)
			_, dup := obj[key]
			if dup {
				fail(pointer, "the key %q appears twice", key)
			}
			obj[key] = decodeValue(dec, pointer+"/"+escapePointer(key))
		}
		v = obj
	case '[':
		list := []any{}
		for i := 0; dec.More(); i++ {
			list = append(list, decodeValue(dec, pointer+"/"+strconv.Itoa(i)))
		}
		v = list
	}
	nextToken(dec, pointer)
	return v
}

// nextToken returns the next token of dec, refusing text that is not JSON.
// Text that ends inside a value gets the message encoding/json/v2 gives it
// (the toolchain default from Go 1.27), where encoding/json says only EOF.
func nextToken(dec *json.Decoder, pointer string) json.Token {
	tok, err := dec.Token()
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		fail(pointer, "it is not valid JSON: unexpected end of JSON input")
	}
	if err != nil {
		fail(pointer, "it is not valid JSON: %v", err)
	}
	return tok
}

func escapePointer(key string) string {
	return strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
}

type textParser struct {
	refs map[string]bool
}

// schema builds the schema v holds at pointer. nullMember is true for a
// member of an anyOf, the one place a schema of type null alone may be.
func (p *textParser) schema(v any, pointer string, nullMember bool) *huma.Schema {
	obj, ok := v.(map[string]any)
	if !ok {
		fail(pointer, "a schema must be a JSON object, not %s", jsonTypeOf(v))
	}

	s := &huma.Schema{}
	for _, key := range sortedKeys(obj) {
		val := obj[key]
		at := pointer + "/" + escapePointer(key)
		switch key {
		case "type":
			p.typeList(s, val, at)
		case "title":
			s.Title = p.text(val, at)
		case "description":
			s.Description = p.text(val, at)
		case "format":
			s.Format = p.text(val, at)
			if s.Format == "binary" {
				fail(at, "the format binary is not supported: the document would add a contentMediaType the text does not hold")
			}
			_, encoded := obj["contentEncoding"]
			if encoded {
				fail(at, "format and contentEncoding together are not supported: the OpenAPI 3.0 document writes a contentEncoding of base64 as the format, in place of this one, and the registry refuses the pair for any contentEncoding; use one of them")
			}
		case "contentEncoding":
			s.ContentEncoding = p.text(val, at)
		case "pattern":
			s.Pattern = p.text(val, at)
			_, err := regexp.Compile(s.Pattern)
			if err != nil {
				fail(at, "the pattern is not a valid regular expression: %v", err)
			}
		case "$ref":
			s.Ref = p.text(val, at)
			if !p.refs[s.Ref] {
				fail(at, "the $ref %s is not one refs.For returned during this call; refer to another type with refs.For(reflect.TypeFor[T]())", s.Ref)
			}
			_, allOf := obj["allOf"]
			if allOf {
				fail(at, "a $ref beside an allOf is not supported: OpenAPI 3.0 ignores the keywords beside a $ref, and its document can only move the $ref into an allOf where there is none; list the $ref in the allOf")
			}
		case "default":
			if val == nil {
				fail(at, "a default of null is not supported: the document would leave it out")
			}
			p.value(val, at)
			s.Default = val
		case "const":
			if val == nil {
				fail(at, `a const of null is not supported: the document would leave it out; write {"type": "null"} as a member of an anyOf`)
			}
			p.value(val, at)
			s.Const = val
		case "examples":
			s.Examples = p.list(val, at)
			if len(s.Examples) > 1 {
				fail(at, "examples with more than one item are not supported: the OpenAPI 3.0 document has room for one of them")
			}
			p.value(val, at)
		case "example":
			_, both := obj["examples"]
			if both {
				fail(at, "example and examples together are not supported: the OpenAPI 3.0 document has room for one of them")
			}
			p.value(val, at)
			if s.Extensions == nil {
				s.Extensions = map[string]any{}
			}
			s.Extensions[key] = val
		case "enum":
			s.Enum = p.list(val, at)
			if len(s.Enum) == 0 {
				fail(at, "an empty enum is not supported: the document would leave it out and accept any value")
			}
			p.value(val, at)
		case "minimum":
			s.Minimum = p.number(val, at)
		case "maximum":
			s.Maximum = p.number(val, at)
		case "exclusiveMinimum":
			s.ExclusiveMinimum = p.number(val, at)
		case "exclusiveMaximum":
			s.ExclusiveMaximum = p.number(val, at)
		case "multipleOf":
			s.MultipleOf = p.number(val, at)
			if *s.MultipleOf <= 0 {
				fail(at, "multipleOf must be greater than 0")
			}
		case "minLength":
			s.MinLength = p.count(val, at)
		case "maxLength":
			s.MaxLength = p.count(val, at)
		case "minItems":
			s.MinItems = p.count(val, at)
		case "maxItems":
			s.MaxItems = p.count(val, at)
		case "minProperties":
			s.MinProperties = p.count(val, at)
		case "maxProperties":
			s.MaxProperties = p.count(val, at)
		case "uniqueItems":
			s.UniqueItems = p.flag(val, at)
		case "readOnly", "writeOnly":
			fail(at, "%s is not supported: the validator reads it as an annotation, so a required property it marks is still asked of a request (readOnly) or a response (writeOnly) while the OpenAPI 3.0 document tells clients otherwise; leave it out, and list the property in required only where it is always sent", key)
		case "deprecated":
			s.Deprecated = p.flag(val, at)
		case "required":
			for i, item := range p.list(val, at) {
				name, ok := item.(string)
				if !ok {
					fail(at+"/"+strconv.Itoa(i), "required lists property names, which are strings")
				}
				if slices.Contains(s.Required, name) {
					fail(at, "the property %q is listed twice", name)
				}
				if renamedByDowngrade(name) {
					fail(at+"/"+strconv.Itoa(i), renamedPropertyProblem, name)
				}
				s.Required = append(s.Required, name)
			}
		case "properties":
			props, ok := val.(map[string]any)
			if !ok {
				fail(at, "properties must be a JSON object")
			}
			s.Properties = map[string]*huma.Schema{}
			for _, name := range sortedKeys(props) {
				if renamedByDowngrade(name) {
					fail(at+"/"+escapePointer(name), renamedPropertyProblem, name)
				}
				empty, isObject := props[name].(map[string]any)
				if name == "application/octet-stream" && isObject && len(empty) == 0 {
					fail(at+"/"+escapePointer(name), "the property %q with the empty schema {} is not supported: the OpenAPI 3.0 document would rewrite it as a binary upload; give it a schema that is not empty, or use another name", name)
				}
				s.Properties[name] = p.schema(props[name], at+"/"+escapePointer(name), false)
			}
		case "additionalProperties":
			allowed, ok := val.(bool)
			if ok {
				s.AdditionalProperties = allowed
				continue
			}
			s.AdditionalProperties = p.schema(val, at, false)
		case "items":
			s.Items = p.schema(val, at, false)
		case "not":
			s.Not = p.schema(val, at, false)
		case "allOf":
			s.AllOf = p.schemas(val, at, false)
		case "oneOf":
			s.OneOf = p.schemas(val, at, false)
		case "anyOf":
			s.AnyOf = p.schemas(val, at, true)
			p.nullAnyOf(obj, s.AnyOf, at)
		case "nullable":
			fail(at, `nullable is not a JSON Schema keyword (it is OpenAPI 3.0's); write null in the type, as ["string", "null"]`)
		default:
			if !strings.HasPrefix(key, "x-") {
				fail(at, "%q is not a keyword the registry publishes and validates (see api.SchemaProvider)", key)
			}
			p.value(val, at)
			if s.Extensions == nil {
				s.Extensions = map[string]any{}
			}
			s.Extensions[key] = val
		}
	}

	if s.Type == "null" && !s.Nullable && (!nullMember || len(obj) != 1) {
		fail(pointer+"/type", `a schema of type null alone is not supported, as OpenAPI 3.0 has no null type: write null in a type list, as ["string", "null"], or as a member {"type": "null"} of an anyOf`)
	}
	if slices.Contains(s.Enum, nil) && !s.Nullable {
		fail(pointer+"/enum", "an enum that lists null is not supported unless the type lists null too: the OpenAPI 3.0 document carries the enum without nullable: true, which refuses null; "+nullEnumAdvice)
	}
	return s
}

// renamedPropertyProblem refuses a property name the 3.0 downgrade renames
// (see renamedByDowngrade).
const renamedPropertyProblem = "the property name %q is renamed by the 3.0 downgrade; use another name"

// nullEnumAdvice is how to write an enum that lets null through, which the
// OpenAPI 3.0 document carries as nullable: true with null in the enum.
const nullEnumAdvice = `write a type list with null in the enum, as {"type": ["string", "null"], "enum": ["a", "b", null]}`

// value refuses, in v, a value the text holds at at (an example, a default,
// an enum or const value, an x- value), an object whose keys the OpenAPI
// 3.0 document would rewrite as if it were a schema (see rewrittenKey).
func (p *textParser) value(v any, at string) {
	switch v := v.(type) {
	case []any:
		for i, item := range v {
			p.value(item, at+"/"+strconv.Itoa(i))
		}
	case map[string]any:
		for _, key := range sortedKeys(v) {
			if rewrittenKey(v, key) {
				fail(at+"/"+escapePointer(key), "the key %q is not supported in this value: the OpenAPI 3.0 document would rewrite it as if the value were a schema; leave it out of the value", key)
			}
			p.value(v[key], at+"/"+escapePointer(key))
		}
	}
}

// rewrittenKey reports whether the OpenAPI 3.0 document rewrites key of
// obj wherever obj is: huma's Downgrade rewrites a type list, a number
// exclusiveMinimum or exclusiveMaximum, an examples list with an item,
// contentEncoding, contentMediaType, an empty application/octet-stream and
// an openapi of "3.1.0"; the 3.0 fixes of the registry (fixNullable) rewrite
// a $ref with other keys and no allOf, and an anyOf with a {"type": "null"}
// member.
func rewrittenKey(obj map[string]any, key string) bool {
	val := obj[key]
	switch key {
	case "type":
		_, list := val.([]any)
		return list
	case "exclusiveMinimum", "exclusiveMaximum":
		_, number := val.(json.Number)
		return number
	case "examples":
		list, _ := val.([]any)
		return len(list) > 0
	case "contentEncoding", "contentMediaType":
		return true
	case "application/octet-stream":
		m, isObject := val.(map[string]any)
		return isObject && len(m) == 0
	case "openapi":
		return val == "3.1.0"
	case "$ref":
		_, text := val.(string)
		_, allOf := obj["allOf"]
		return text && len(obj) > 1 && !allOf
	case "anyOf":
		list, _ := val.([]any)
		return slices.ContainsFunc(list, func(m any) bool {
			member, isObject := m.(map[string]any)
			return isObject && len(member) == 1 && member["type"] == "null"
		})
	}
	return false
}

// nullAnyOf refuses an anyOf with a null member that OpenAPI 3.0 cannot
// carry. The 3.0 document writes such an anyOf as nullable: true on the
// schema that holds it, so that schema may hold nothing but annotations
// beside it, and merges a single other member into it, so the two may not
// share a keyword. A member other than null may hold no enum or const,
// which nullable: true does not let null past in 3.0.
func (p *textParser) nullAnyOf(obj map[string]any, members []*huma.Schema, at string) {
	nulls := 0
	var others []any
	var otherAt []string
	for i, m := range members {
		if m.Type == "null" && !m.Nullable {
			nulls++
			continue
		}
		others = append(others, obj["anyOf"].([]any)[i])
		otherAt = append(otherAt, at+"/"+strconv.Itoa(i))
	}
	if nulls == 0 {
		return
	}
	if nulls > 1 || len(others) == 0 {
		fail(at, `an anyOf with a null member needs exactly one {"type": "null"} and at least one other member`)
	}
	for i, other := range others {
		for _, key := range []string{"const", "enum"} {
			_, held := other.(map[string]any)[key]
			if held {
				fail(otherAt[i]+"/"+key, "%s in a member of an anyOf with a null member is not supported: the OpenAPI 3.0 document writes the null member as nullable: true, which does not let null past the %s; %s", key, key, nullEnumAdvice)
			}
		}
	}
	for _, key := range sortedKeys(obj) {
		if key != "anyOf" && !annotations[key] && !strings.HasPrefix(key, "x-") {
			fail(at, "a schema with an anyOf that holds null may hold only annotations (title, description, default, examples, example, deprecated, x-) beside it, not %q: OpenAPI 3.0 writes it as nullable, which would let null past %q", key, key)
		}
	}
	if len(others) > 1 {
		return
	}
	for key := range others[0].(map[string]any) {
		_, shared := obj[key]
		if shared {
			fail(at, "the schema and the one member of its anyOf other than null both hold %q, which the OpenAPI 3.0 document merges into one", key)
		}
	}
}

func (p *textParser) schemas(v any, at string, nullMember bool) []*huma.Schema {
	list := p.list(v, at)
	if len(list) == 0 {
		fail(at, "it needs at least one schema")
	}
	out := make([]*huma.Schema, len(list))
	for i, item := range list {
		out[i] = p.schema(item, at+"/"+strconv.Itoa(i), nullMember)
	}
	return out
}

// jsonSchemaTypes are the types of JSON Schema.
var jsonSchemaTypes = []string{huma.TypeString, huma.TypeNumber, huma.TypeInteger, huma.TypeBoolean, huma.TypeArray, huma.TypeObject, "null"}

// typeList reads a type, or a list of one type and null, which huma holds
// as Type and Nullable.
func (p *textParser) typeList(s *huma.Schema, v any, at string) {
	names := []any{v}
	list, isList := v.([]any)
	if isList {
		names = list
	}
	if len(names) == 0 {
		fail(at, "the type list is empty")
	}
	var types []string
	for _, n := range names {
		name, ok := n.(string)
		if !ok || !slices.Contains(jsonSchemaTypes, name) {
			fail(at, "%s is not a JSON Schema type (string, number, integer, boolean, array, object or null)", jsonText(n))
		}
		if slices.Contains(types, name) {
			fail(at, "the type %s is listed twice", name)
		}
		types = append(types, name)
	}

	nullable := slices.Contains(types, "null")
	if nullable && len(types) > 1 {
		types = slices.DeleteFunc(types, func(name string) bool {
			return name == "null"
		})
	}
	if len(types) > 1 {
		fail(at, "a type list of more than one type besides null is not supported, as OpenAPI 3.0 has none; use an anyOf")
	}
	s.Type = types[0]
	s.Nullable = nullable && s.Type != "null"
}

func (p *textParser) text(v any, at string) string {
	text, ok := v.(string)
	if !ok {
		fail(at, "it must be a string, not %s", jsonTypeOf(v))
	}
	return text
}

func (p *textParser) flag(v any, at string) bool {
	flag, ok := v.(bool)
	if !ok {
		fail(at, "it must be true or false, not %s", jsonTypeOf(v))
	}
	return flag
}

func (p *textParser) list(v any, at string) []any {
	list, ok := v.([]any)
	if !ok {
		fail(at, "it must be a JSON array, not %s", jsonTypeOf(v))
	}
	return list
}

// number reads the number of a keyword (see exactNumber).
func (p *textParser) number(v any, at string) *float64 {
	n, ok := v.(json.Number)
	if !ok {
		fail(at, "it must be a number, not %s", jsonTypeOf(v))
	}
	f := exactNumber(n, at)
	return &f
}

// exactNumber returns n, a number anywhere in the text, as a float64,
// refusing one a float64 does not hold as written: the OpenAPI 3.0
// document is written from float64s, so 9007199254740993 would be
// published as 9007199254740992.
func exactNumber(n json.Number, at string) float64 {
	f, err := strconv.ParseFloat(n.String(), 64)
	if err != nil {
		fail(at, "the number %s cannot be published as written, as a float64 does not hold it", n)
	}
	// big.Rat refuses an exponent past its limit, as in 1e-99999999, which
	// a float64 does not hold as written either.
	written, ok := new(big.Rat).SetString(n.String())
	if !ok {
		fail(at, "the number %s cannot be published as written, as a float64 does not hold it", n)
	}
	// The shortest text of a finite float64 always parses.
	published, _ := new(big.Rat).SetString(strconv.FormatFloat(f, 'g', -1, 64))
	if written.Cmp(published) != 0 {
		fail(at, "the number %s cannot be published as written, as a float64 does not hold it", n)
	}
	return f
}

func (p *textParser) count(v any, at string) *int {
	n, ok := v.(json.Number)
	if !ok {
		fail(at, "it must be a number, not %s", jsonTypeOf(v))
	}
	i, err := strconv.Atoi(n.String())
	if err != nil || i < 0 {
		fail(at, "it must be a whole number of 0 or more, written without a fraction or an exponent, not %s", n)
	}
	return &i
}

func jsonTypeOf(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "a boolean"
	case json.Number:
		return "a number"
	case string:
		return "a string"
	case []any:
		return "an array"
	}
	return "an object"
}

func jsonText(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(raw)
}
