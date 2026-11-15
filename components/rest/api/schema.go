package api

import "github.com/vanclief/compose/components/rest/api/openapi"

// SchemaProvider is a type that says what its JSON is. The registry
// documents a type by its Go fields, which is not what goes over the wire
// for a type that writes or reads its own JSON (MarshalJSON, UnmarshalJSON,
// MarshalText). A response struct that writes its own JSON, and has no
// UnmarshalText that makes it a string, fails registration unless it has a
// JSONSchema method.
//
// JSONSchema returns the JSON Schema (draft 2020-12, as OpenAPI 3.1 reads
// it) of the type's JSON as JSON text: one JSON object, with no key twice
// in any object. The type becomes a component of the documents, named as a
// struct would be, and every place that holds it refers to it: a field, a
// slice item, a map value, the T of a types.Optional[T], or the whole
// response. A pointer (in a response one without omitempty, in a request one
// that is not required), or a types.Optional, adds null as
// anyOf [ref, {"type": "null"}] (nullable: true with allOf [ref] in the
// OpenAPI 3.0 document). A request value is validated against the schema,
// and one that passes reaches the handler decoded by the type's own
// decoder. The text is published and validated as written: compose neither
// closes nor opens its objects, in the documents or in the strict contract
// that apitest checks.
//
// The text may use type (a JSON Schema type, or a list of one type and
// "null"), title, description, format (but binary), contentEncoding,
// pattern, enum, const, default, examples, example, minimum, maximum,
// exclusiveMinimum, exclusiveMaximum, multipleOf, minLength, maxLength,
// minItems, maxItems, uniqueItems, minProperties, maxProperties, required,
// properties, additionalProperties, items (one schema), allOf, anyOf,
// oneOf, not, readOnly, writeOnly, deprecated, $ref and keys that start
// with x-. Registration fails, naming the type and the JSON pointer of the
// place, for any other keyword, a schema that is not an object (true,
// false), an empty enum, a const or default of null, both example and
// examples, and a $ref beside an allOf. OpenAPI 3.0 has no null type, so
// {"type": "null"} may only be the one null member of an anyOf with other
// members, whose schema holds nothing but annotations (title, description,
// default, examples, example, deprecated, readOnly, writeOnly, x-) beside
// it. A $ref must be one that refs.For returned during the same call.
// Registration fails as well for what the OpenAPI 3.0 document would carry
// otherwise than written: a number a float64 does not hold as written,
// anywhere in the text, a property named contentEncoding or
// contentMediaType, a property named application/octet-stream with the
// empty schema {}, examples with more than one item, format beside
// contentEncoding, a key the 3.0 document would rewrite as a schema keyword
// inside an example, default, enum, const or x- value, an enum or const in
// a member of an anyOf with a null member, and an enum that lists null
// where the type does not; write an enum that lets null through as
// {"type": ["string", "null"], "enum": ["a", null]}. The OpenAPI 3.0
// document carries const as it is, which 3.0 does not define.
//
// The type must code its own JSON where it is used, since encoding/json
// otherwise reads or writes it by its Go type, which JSONSchema does not
// change: in a request a decoder (UnmarshalJSON or UnmarshalText, and on
// encoding/json/v2 UnmarshalJSONFrom), in a response an encoder
// (MarshalJSON or MarshalText, and on encoding/json/v2 MarshalJSONTo or
// AppendText). One that codes it as text states a schema of type "string".
// Registration fails for a struct that gets JSONSchema or its coder from an
// embedded field, an enums.Set type with a JSONSchema method, such a type
// as a path, query or header parameter or a form value, whose text the
// schema does not describe, a format tag on it or on its items, and in a
// request a bound, length, pattern or enum tag on it or on its items and a
// minItems, maxItems or uniqueItems tag on it, which belong in the schema.
// A request field fails too when the text states a default at its root,
// which the registry does not apply and encoding/json never decodes for an
// absent key, and when it is a required:"true" pointer and the text lets
// null through at its root, as encoding/json then sets the pointer to nil.
//
// The types JSONSchema asks refs.For for are linted and key-checked as the
// fields of a type that holds the provider are, in the same direction. It
// cannot ask for its own type's schema, directly or through other types'
// JSONSchema methods alone; where the ask reaches the type through the
// properties of a struct documented by its fields, it gets the type's $ref,
// whichever route registers first. JSONSchema is called at registration, possibly more
// than once, and must return the same text every time. A panic in it is not
// recovered.
//
// For example, a type written as "12.50 MXN", and one written as either a
// Money or a percentage:
//
//	type Money struct {
//		Cents    int64
//		Currency string
//	}
//
//	func (m Money) MarshalJSON() ([]byte, error) { ... }
//
//	func (m *Money) UnmarshalJSON(data []byte) error { ... }
//
//	func (Money) JSONSchema(refs *api.SchemaRefs) string {
//		return `{"type": "string", "pattern": "^[0-9]+\\.[0-9]{2} [A-Z]{3}$"}`
//	}
//
//	type Discount struct {
//		Amount  *Money
//		Percent int
//	}
//
//	func (d Discount) MarshalJSON() ([]byte, error) { ... }
//
//	func (d *Discount) UnmarshalJSON(data []byte) error { ... }
//
//	func (Discount) JSONSchema(refs *api.SchemaRefs) string {
//		return `{"anyOf": [` + refs.For(reflect.TypeFor[Money]()) +
//			`, {"type": "integer", "minimum": 0, "maximum": 100}]}`
//	}
type SchemaProvider = openapi.SchemaProvider

// SchemaRefs gives a SchemaProvider the schema of another Go type:
// refs.For(t) returns, as JSON text, the schema of the Go type t as the
// registry publishes it. For a type the document holds as a component (a
// struct documented by its fields, an enums.Set type, a type with a
// JSONSchema method) that is {"$ref":"#/components/schemas/<Name>"}, which
// the document holds when the text uses it; a pointer to an enums.Set type
// or to a type with a JSONSchema method, or a types.Optional of any of
// these, gives anyOf [{"$ref": ...}, {"type": "null"}]; for any other, a
// struct with a text decoder among them, it is the type's inline schema. A
// type asked for is linted and key-checked whether or not the text uses its
// schema. A nil *SchemaRefs, as in a
// unit test of a JSONSchema method, returns {}, and so does one kept past
// the JSONSchema call it was passed to.
type SchemaRefs = openapi.SchemaRefs
