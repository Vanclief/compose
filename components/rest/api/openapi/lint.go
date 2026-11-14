package openapi

import (
	"fmt"
	"mime/multipart"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/vanclief/ez"

	"github.com/vanclief/compose/components/rest/api/internal/huma"
	"github.com/vanclief/compose/primitives/enums"
)

// allowedTags are the struct tag keys accepted on registered types. Anything
// else is most likely a typo (minimun) that huma would silently ignore.
var allowedTags = map[string]bool{
	"json": true, "path": true, "query": true, "header": true, "form": true, "required": true,
	"default": true, "minimum": true, "maximum": true, "exclusiveMinimum": true,
	"exclusiveMaximum": true, "multipleOf": true, "minLength": true, "maxLength": true,
	"pattern": true, "minItems": true, "maxItems": true, "uniqueItems": true, "enum": true,
	"format": true, "doc": true, "example": true, "deprecated": true, "hidden": true,
	"nullable": true, "bun": true, "db": true, "yaml": true, "xml": true,
	"mapstructure": true, "validate": true,
}

// lintType checks every struct reachable from t for unknown tags, Echo's
// param tag and property names that huma's Downgrade would rename. A request
// type is also refused a default tag: the registry applies no default, so
// the document would promise a value the handler never gets; a path,
// query, header or form tag outside the request type and the structs it
// embeds, where the registry never reads it; a map keyed by an enum type,
// whose keys nothing validates (see enumMapKey); nullable:"false" on a
// pointer body field that is not required:"true", or nullable:"true" on one
// that is, which the request shape makes nullable or non-null whatever the
// tag says (see requestShape); nullable:"true" on a body field that cannot
// hold null (see holdsNull); uniqueItems on items the schema compares
// otherwise than the decoder (see uniqueItemsApplies); minLength below 1 on
// a netip.Prefix or netip.AddrPort, whose decoder reads "" as the zero value
// (see refusesEmptyText); exclusiveMinimum or
// exclusiveMaximum on a float (see floatBounded); a bound, length or item
// tag huma would put on a schema of another JSON type, where nothing checks
// it (see misplacedTags); and a nullable tag on the blank field, which makes
// the whole struct nullable. The structs the type embeds are walked whether
// or not they are exported or tagged json:"-": buildParams reads their
// parameters all the same. A request is also refused a path, query, header
// or form tag on an unexported or embedded field, which nothing reads, or on
// a field another one hides (see shadowedParamTags), nullable:"false" on a
// types.Optional of a struct (see ignoresNonNull), and minItems or maxItems
// on a fixed-size array. A response type is refused a pointer-receiver
// marshaler held by value in a map (see lostMarshalers). A field that holds
// a url.URL is refused, as is one that holds an interface type with methods
// in a request (see unsupportedType).
func lintType(t reflect.Type, request bool) error {
	var problems []string
	lintWalk(t, request, map[reflect.Type]bool{}, &problems)
	if request {
		lintParamTags(t, true, map[paramVisit]bool{}, &problems)
		problems = append(problems, shadowedParamTags(t)...)
	} else {
		lostMarshalers(t, true, map[marshalVisit]bool{}, &problems)
	}
	if len(problems) == 0 {
		return nil
	}
	return ez.New(ez.EINVALID, "openapi: "+strings.Join(problems, "; "), nil)
}

func lintWalk(t reflect.Type, request bool, seen map[reflect.Type]bool, problems *[]string) {
	t = deref(t)
	elem, ok := optionalElem(t)
	if ok {
		lintWalk(elem, request, seen, problems)
		return
	}
	if seen[t] || isOpaque(t) {
		return
	}
	seen[t] = true

	switch t.Kind() {
	case reflect.Slice, reflect.Array, reflect.Map:
		lintWalk(t.Elem(), request, seen, problems)
		return
	case reflect.Struct:
	default:
		return
	}

	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if request && (!f.IsExported() || f.Anonymous) {
			for _, key := range paramTags {
				_, tagged := f.Tag.Lookup(key)
				if tagged {
					*problems = append(*problems, fmt.Sprintf("%s.%s: the %s tag is never read on an unexported or embedded field; declare an exported, named field", t, f.Name, key))
				}
			}
		}
		if !f.IsExported() {
			_, nullableStruct := f.Tag.Lookup("nullable")
			if request && f.Name == "_" && nullableStruct {
				*problems = append(*problems, fmt.Sprintf("%s: a nullable tag on the blank field is not supported in a request type: null would pass for the whole struct and leave its zero value unchecked; use a pointer field", t))
			}
			if f.Anonymous {
				lintWalk(f.Type, request, seen, problems)
			}
			continue
		}

		for _, key := range tagKeys(f.Tag) {
			if key == "param" {
				*problems = append(*problems, fmt.Sprintf("%s.%s: param is Echo's binder tag, use path", t, f.Name))
				continue
			}
			if key == "default" && request {
				*problems = append(*problems, fmt.Sprintf("%s.%s: the default tag is not supported on a request type, the registry does not apply it; set the default in the handler and describe it with doc", t, f.Name))
				continue
			}
			if !allowedTags[key] {
				*problems = append(*problems, fmt.Sprintf("%s.%s: unknown struct tag %q", t, f.Name, key))
			}
		}

		// huma merges an embedded struct with no JSON name into the
		// enclosing object without reading its boolean tags, which the lint
		// and the key check read there (nullable, hidden), so they are held
		// to "true" or "false" here as on any other field.
		// The schema names a property by its json tag's name as written;
		// encoding/json names the key by its own rules (jsonTagKey).
		tagName, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if tagName != "" && tagName != "-" && jsonTagKey(tagName) != tagName {
			key := jsonTagKey(tagName)
			if key == "" {
				key = f.Name
			}
			*problems = append(*problems, fmt.Sprintf("%s.%s: encoding/json does not take the json tag name %q as it is written and uses the key %q, which the schema would not list; name the key with letters, digits, spaces and ASCII punctuation other than a backslash, a quote, a backtick or a comma", t, f.Name, tagName, key))
		}
		if f.Anonymous && tagName == "" && deref(f.Type).Kind() == reflect.Struct {
			for _, tag := range []string{"required", "nullable", "deprecated", "hidden", "uniqueItems"} {
				_, err := huma.ParseBoolTag(f, tag, false)
				if err != nil {
					*problems = append(*problems, fmt.Sprintf("%s.%s: %s", t, f.Name, ez.ErrorMessage(err)))
				}
			}
		}

		*problems = append(*problems, lintField(t, f)...)
		if request {
			*problems = append(*problems, misplacedTags(t, f)...)
		}

		if request && f.Tag.Get("uniqueItems") == "true" && !uniqueItemsApplies(f.Type) {
			*problems = append(*problems, fmt.Sprintf("%s.%s: uniqueItems is not supported on %s in a request type: the schema compares the items' text, which the decoder may read as equal values; check uniqueness in Validate()", t, f.Name, f.Type))
		}

		if request && refusesEmptyText(f.Type) {
			minLength, err := strconv.Atoi(f.Tag.Get("minLength"))
			if err == nil && minLength < 1 {
				*problems = append(*problems, fmt.Sprintf("%s.%s: minLength %d is not supported on %s in a request type: its decoder reads \"\" as the zero value without an error, and the schema's minLength 1 is what refuses it; drop the tag or set it to 1 or more", t, f.Name, minLength, f.Type))
			}
		}

		if request && floatBounded(f.Type) {
			for _, key := range []string{"exclusiveMinimum", "exclusiveMaximum"} {
				_, tagged := f.Tag.Lookup(key)
				if tagged {
					*problems = append(*problems, fmt.Sprintf("%s.%s: %s is not supported on %s in a request type: the schema checks the number as the client wrote it, and the float can round it onto the bound; use an inclusive bound, a json.Number field, or a check in Validate()", t, f.Name, key, f.Type))
				}
			}
		}

		name := jsonName(f)
		if name == "-" {
			if f.Anonymous {
				lintWalk(f.Type, request, seen, problems)
			}
			continue
		}
		if name == "contentEncoding" || name == "contentMediaType" {
			*problems = append(*problems, fmt.Sprintf("%s.%s: property name %q is renamed by the 3.0 downgrade", t, f.Name, name))
		}
		if request {
			key := enumMapKey(f.Type)
			if key != nil {
				*problems = append(*problems, fmt.Sprintf("%s.%s: map keys are not validated, so a key of the enum type %s could hold any string; use a string key and check it in Validate()", t, f.Name, key))
			}
			if f.Type.Kind() == reflect.Pointer && f.Tag.Get("nullable") == "false" && f.Tag.Get("required") != "true" {
				*problems = append(*problems, fmt.Sprintf("%s.%s: nullable:\"false\" has no effect on a pointer field of a request type, which accepts null unless it is required; tag it required:\"true\" to refuse null, or drop the pointer", t, f.Name))
			}
			if f.Type.Kind() == reflect.Pointer && f.Tag.Get("nullable") == "true" && f.Tag.Get("required") == "true" {
				*problems = append(*problems, fmt.Sprintf("%s.%s: nullable:\"true\" has no effect on a required pointer field of a request type, which refuses null; for a key that must be present and may be null, use types.Optional[T] with required:\"true\"", t, f.Name))
			}
			if f.Tag.Get("nullable") == "false" && ignoresNonNull(f.Type) {
				*problems = append(*problems, fmt.Sprintf("%s.%s: nullable:\"false\" has no effect on %s in a request type, whose schema accepts null whatever the tag says; refuse null in Validate()", t, f.Name, f.Type))
			}
			// lintField refuses the tag on a struct.
			isStruct := f.Type.Kind() == reflect.Struct && !isOpaque(f.Type)
			if f.Tag.Get("nullable") == "true" && !holdsNull(f.Type) && !isStruct {
				*problems = append(*problems, fmt.Sprintf("%s.%s: nullable:\"true\" is not supported on a request field of type %s: null would leave the zero value unchecked; use *%s or types.Optional[%s]", t, f.Name, f.Type, f.Type, f.Type))
			}
		}
		problem := unsupportedType(heldType(f.Type), request)
		if problem != "" {
			*problems = append(*problems, fmt.Sprintf("%s.%s: %s", t, f.Name, problem))
		}

		lintWalk(f.Type, request, seen, problems)
	}
}

// scalarTags are the tags huma puts on a field's schema, or on the items of
// its array, with the JSON type each constrains.
var scalarTags = []struct {
	tag  string
	want string
}{
	{"minimum", huma.TypeNumber}, {"maximum", huma.TypeNumber},
	{"exclusiveMinimum", huma.TypeNumber}, {"exclusiveMaximum", huma.TypeNumber},
	{"multipleOf", huma.TypeNumber},
	{"minLength", huma.TypeString}, {"maxLength", huma.TypeString}, {"pattern", huma.TypeString},
}

// arrayTags are the tags huma puts on the field's own schema, which must be
// an array.
var arrayTags = []string{"minItems", "maxItems", "uniqueItems"}

// misplacedTags reports the bound, length and item tags of a request field
// that huma puts on a schema of another JSON type: maximum on a map (huma
// leaves it on the object, not on the values), on a struct or on a slice of
// slices (on the inner array), minLength on a number, minItems on a string,
// and any of them on a field whose schema has no type (see typeless).
// The validator ignores a keyword on a type it does not constrain, so the
// value the tag meant to refuse would reach the handler.
func misplacedTags(t reflect.Type, f reflect.StructField) []string {
	// With no schema type a keyword checks only the values of its own JSON
	// type: maxLength on a json.RawMessage passes every object.
	if typeless(f.Type) {
		var problems []string
		for _, st := range scalarTags {
			_, tagged := f.Tag.Lookup(st.tag)
			if tagged {
				problems = append(problems, fmt.Sprintf("%s.%s: %s is not supported on %s in a request type: its schema has no type, so the tag would check only a %s and pass any other value; check the value in Validate()", t, f.Name, st.tag, f.Type, st.want))
			}
		}
		for _, tag := range arrayTags {
			_, tagged := f.Tag.Lookup(tag)
			if tagged {
				problems = append(problems, fmt.Sprintf("%s.%s: %s is not supported on %s in a request type: its schema has no type, so the tag would check only an array and pass any other value; check the value in Validate()", t, f.Name, tag, f.Type))
			}
		}
		return problems
	}

	kind := jsonKind(f.Type)
	target := kind
	if kind == huma.TypeArray {
		ft := deref(f.Type)
		elem, ok := optionalElem(ft)
		if ok {
			ft = deref(elem)
		}
		target = jsonKind(ft.Elem())
	}

	var problems []string
	for _, st := range scalarTags {
		_, tagged := f.Tag.Lookup(st.tag)
		if !tagged || target == "" || target == st.want || (st.want == huma.TypeNumber && target == huma.TypeInteger) {
			continue
		}
		problems = append(problems, fmt.Sprintf("%s.%s: %s constrains a %s, and on %s the schema would carry it on %s, where nothing checks it; check the value in Validate()", t, f.Name, st.tag, st.want, f.Type, article(target)))
	}
	for _, tag := range arrayTags {
		_, tagged := f.Tag.Lookup(tag)
		if !tagged || kind == "" {
			continue
		}
		if kind == huma.TypeArray {
			// The tag replaces the length huma gives a [N]T, and
			// encoding/json fills a shorter array with zero values and
			// drops the items of a longer one.
			fixed := arrayType(f.Type)
			if fixed != nil && tag != "uniqueItems" {
				problems = append(problems, fmt.Sprintf("%s.%s: %s is not supported on %s in a request type: the array always has %d items, and encoding/json would fill a shorter one with zero values the schema never saw", t, f.Name, tag, f.Type, fixed.Len()))
			}
			continue
		}
		problems = append(problems, fmt.Sprintf("%s.%s: %s constrains an array, and on %s the schema would carry it on %s, where nothing checks it; check the value in Validate()", t, f.Name, tag, f.Type, article(kind)))
	}
	return problems
}

// arrayType returns the fixed-size array a field of type t is (after a
// pointer and a types.Optional), or nil.
func arrayType(t reflect.Type) reflect.Type {
	t = deref(t)
	elem, ok := optionalElem(t)
	if ok {
		t = deref(elem)
	}
	if t.Kind() != reflect.Array {
		return nil
	}
	return t
}

func article(jsonType string) string {
	if jsonType == huma.TypeArray || jsonType == huma.TypeObject || jsonType == huma.TypeInteger {
		return "an " + jsonType
	}
	return "a " + jsonType
}

// jsonKind returns the JSON type of the schema the reflector gives t (after
// a pointer and a types.Optional), or "" when t decides it itself (a
// huma.SchemaProvider) or it has none (an interface, a json.RawMessage).
func jsonKind(t reflect.Type) string {
	t = deref(t)
	elem, ok := optionalElem(t)
	if ok {
		t = deref(elem)
	}

	builtin, ok := builtinSchemas[t]
	if ok {
		return builtin.Type
	}
	ptr := reflect.PointerTo(t)
	if t == rawMessageType || t.Implements(schemaProviderType) || ptr.Implements(schemaProviderType) {
		return ""
	}
	if t == timeType || t.Implements(textUnmarshalerType) || ptr.Implements(textUnmarshalerType) {
		return huma.TypeString
	}

	switch t.Kind() {
	case reflect.Bool:
		return huma.TypeBoolean
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return huma.TypeInteger
	case reflect.Float32, reflect.Float64:
		return huma.TypeNumber
	case reflect.String:
		return huma.TypeString
	case reflect.Slice:
		// A []byte is a base64 string.
		if t.Elem().Kind() == reflect.Uint8 {
			return huma.TypeString
		}
		return huma.TypeArray
	case reflect.Array:
		return huma.TypeArray
	case reflect.Map, reflect.Struct:
		return huma.TypeObject
	}
	return ""
}

// holdsNull reports whether a body field of type t holds the null a client
// sends apart from a value: a pointer, slice, map, interface,
// types.Optional, or a type builtinSchemas marks nullable (uuid.NullUUID).
// Any other type keeps its zero value.
func holdsNull(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Map, reflect.Interface:
		return true
	}
	if builtinSchemas[t].Nullable {
		return true
	}
	_, ok := optionalElem(t)
	return ok
}

// ignoresNonNull reports whether nullable:"false" on a field of type t
// changes nothing: a types.Optional of a struct, whose schema is
// anyOf [ref, null] before huma reads the tag.
func ignoresNonNull(t reflect.Type) bool {
	elem, ok := optionalElem(t)
	if !ok {
		return false
	}
	t = deref(elem)
	return t.Kind() == reflect.Struct && !isOpaque(t)
}

// typeless reports whether the schema of t (after a pointer and a
// types.Optional) has no type: an interface or a json.RawMessage. huma
// would write a nullable tag there as the type list ["", "null"], which no
// OpenAPI reader accepts.
func typeless(t reflect.Type) bool {
	t = deref(t)
	elem, ok := optionalElem(t)
	if ok {
		t = deref(elem)
	}
	return t.Kind() == reflect.Interface || t == rawMessageType
}

// lostMarshalers reports the types a response type holds by value inside a
// map value whose JSON or text marshaler has a pointer receiver (big.Int):
// a map value is not addressable, so encoding/json cannot call the
// marshaler and writes the type's fields instead of what its schema says.
// The response itself is marshalled through a pointer (see the registry's
// writeResponse), so struct fields, slice items and what a pointer or a
// types.Optional holds are addressable.
func lostMarshalers(t reflect.Type, addressable bool, seen map[marshalVisit]bool, problems *[]string) {
	if t.Kind() == reflect.Pointer {
		lostMarshalers(t.Elem(), true, seen, problems)
		return
	}
	elem, ok := optionalElem(t)
	if ok {
		lostMarshalers(elem, true, seen, problems)
		return
	}
	visit := marshalVisit{t: t, addressable: addressable}
	if seen[visit] {
		return
	}
	seen[visit] = true

	ptr := reflect.PointerTo(t)
	byValue := encodesOwnJSON(t)
	byPointer := encodesOwnJSON(ptr)
	if byPointer && !byValue && !addressable {
		*problems = append(*problems, fmt.Sprintf("%s is held by value in a map of a response type: encoding/json cannot call its marshaler there, which has a pointer receiver, and would send its fields instead; hold *%s", t, t))
	}
	if byPointer || byValue {
		return
	}

	switch t.Kind() {
	case reflect.Slice:
		lostMarshalers(t.Elem(), true, seen, problems)
	case reflect.Array:
		lostMarshalers(t.Elem(), addressable, seen, problems)
	case reflect.Map:
		lostMarshalers(t.Elem(), false, seen, problems)
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.IsExported() || f.Anonymous {
				lostMarshalers(f.Type, addressable, seen, problems)
			}
		}
	}
}

// marshalVisit is a type lostMarshalers walked, and whether it was
// addressable there.
type marshalVisit struct {
	t           reflect.Type
	addressable bool
}

// floatBounded reports whether a bound tag on a field of type t (after a
// pointer and a types.Optional) lands on a float32 or float64, the field
// itself or the items of its slice or array, where huma puts the tag. The
// schema checks the digits the client wrote (see validate.MaxExactNumber)
// and the decoder then rounds them, so 1.00000000000000000001 passes
// exclusiveMinimum 1 and reaches the handler as 1. An inclusive bound holds
// through the rounding. A type with a schema or decoder of its own
// (json.Number) is not a float here.
func floatBounded(t reflect.Type) bool {
	t = deref(t)
	elem, ok := optionalElem(t)
	if ok {
		t = deref(elem)
	}
	if isOpaque(t) || decodesOwnText(t) {
		return false
	}
	if t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
		t = deref(t.Elem())
		if isOpaque(t) || decodesOwnText(t) {
			return false
		}
	}
	return t.Kind() == reflect.Float32 || t.Kind() == reflect.Float64
}

// uniqueItemsApplies reports whether uniqueItems on a field of type t (after
// a pointer and a types.Optional) compares the items as the decoder holds
// them: the items, behind pointers, are strings, integers or booleans with
// no schema or decoder of their own. Any other item's text can differ where
// the decoded values are equal (a uuid.UUID in upper and lower case). A type
// with no items to compare is not refused.
func uniqueItemsApplies(t reflect.Type) bool {
	t = deref(t)
	elem, ok := optionalElem(t)
	if ok {
		t = deref(elem)
	}
	if isOpaque(t) || decodesOwnText(t) || (t.Kind() != reflect.Slice && t.Kind() != reflect.Array) {
		return true
	}

	item := deref(t.Elem())
	if isOpaque(item) || decodesOwnText(item) {
		return false
	}
	switch item.Kind() {
	case reflect.String, reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return true
	}
	return false
}

// enumMapKey returns the key type of a map keyed by an enum type that t is,
// or holds through pointers, a types.Optional, slices, arrays and map
// values; nil when there is none. encoding/json assigns a string-kind key
// as it is, and the schema has no rule for keys. A type that decodes itself
// is not looked into.
func enumMapKey(t reflect.Type) reflect.Type {
	t = deref(t)
	elem, ok := optionalElem(t)
	if ok {
		return enumMapKey(elem)
	}
	if isOpaque(t) || decodesOwnText(t) {
		return nil
	}

	switch t.Kind() {
	case reflect.Slice, reflect.Array:
		return enumMapKey(t.Elem())
	case reflect.Map:
		_, isEnum := enums.Values(t.Key())
		if isEnum {
			return t.Key()
		}
		return enumMapKey(t.Elem())
	}
	return nil
}

// unsupportedType explains why a field holding t cannot be described by a
// schema the decoder agrees with, or returns "". encoding/json reads and
// writes a url.URL as an object of its fields, not as a URL string, and
// decodes nothing but null into an interface type with methods.
func unsupportedType(t reflect.Type, request bool) string {
	if t == urlType {
		return "url.URL is not supported: encoding/json reads and writes it as an object, not as a URL string; use a string and parse it with url.Parse"
	}
	if request && t.Kind() == reflect.Interface && t.NumMethod() > 0 {
		return fmt.Sprintf("the interface type %s is not supported in a request: encoding/json cannot decode a value into it; use a concrete type", t)
	}
	return ""
}

// heldType returns the type t holds through pointers, a types.Optional,
// slices, arrays and map values, stopping at a type with a schema of its
// own.
func heldType(t reflect.Type) reflect.Type {
	for {
		t = deref(t)
		elem, ok := optionalElem(t)
		if ok {
			t = elem
			continue
		}
		if isOpaque(t) {
			return t
		}
		switch t.Kind() {
		case reflect.Slice, reflect.Array, reflect.Map:
			t = t.Elem()
			continue
		}
		return t
	}
}

// paramTags are the tags the registry reads only on the request type and the
// structs it embeds (buildParams and the registry's file fields read its
// visible fields).
var paramTags = []string{"path", "query", "header", "form"}

// shadowedParamTags reports a path, query, header or form tag on a field of
// the request type's embedding chain that Go hides: a field of the same name
// sits at a shallower depth, or two at the same depth hide each other.
// buildParams and the registry's file fields read reflect.VisibleFields, so
// the parameter would be neither documented nor read.
func shadowedParamTags(t reflect.Type) []string {
	t = deref(t)
	if t.Kind() != reflect.Struct {
		return nil
	}
	visible := map[string]bool{}
	for _, f := range reflect.VisibleFields(t) {
		visible[fmt.Sprint(f.Index)] = true
	}

	var problems []string
	visiting := map[reflect.Type]bool{}
	var walk func(t reflect.Type, index []int)
	walk = func(t reflect.Type, index []int) {
		t = deref(t)
		if t.Kind() != reflect.Struct || visiting[t] {
			return
		}
		visiting[t] = true
		defer delete(visiting, t)

		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			path := append(slices.Clone(index), i)
			if f.Anonymous {
				walk(f.Type, path)
				continue
			}
			if visible[fmt.Sprint(path)] {
				continue
			}
			for _, key := range paramTags {
				_, tagged := f.Tag.Lookup(key)
				if tagged {
					problems = append(problems, fmt.Sprintf("%s.%s: the %s tag is never read: another field named %s hides this one in the request type; rename one of them", t, f.Name, key, f.Name))
				}
			}
		}
	}
	walk(t, nil)
	return problems
}

// paramVisit is a struct lintParamTags walked, and whether it was reached
// from the request type through embedding only. A struct both embedded and
// reached through a named field is walked both ways.
type paramVisit struct {
	t        reflect.Type
	embedded bool
}

// lintParamTags reports a field tagged path, query, header or form in a
// struct reached from the request type other than through embedding: a
// named field, a slice, array or map element, an Optional's element, or an
// embedded struct with a JSON name, which is a body object: a parameter set
// in it would make an object the client never sent, its body fields
// unchecked.
func lintParamTags(t reflect.Type, embedded bool, seen map[paramVisit]bool, problems *[]string) {
	t = deref(t)
	elem, ok := optionalElem(t)
	if ok {
		lintParamTags(elem, false, seen, problems)
		return
	}
	visit := paramVisit{t: t, embedded: embedded}
	if seen[visit] || isOpaque(t) {
		return
	}
	seen[visit] = true

	switch t.Kind() {
	case reflect.Slice, reflect.Array, reflect.Map:
		lintParamTags(t.Elem(), false, seen, problems)
		return
	case reflect.Struct:
	default:
		return
	}

	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			if f.Anonymous && embedded {
				lintParamTags(f.Type, true, seen, problems)
			}
			continue
		}

		if !embedded {
			for _, key := range paramTags {
				_, tagged := f.Tag.Lookup(key)
				if tagged {
					*problems = append(*problems, fmt.Sprintf("%s.%s: the %s tag is not supported on a nested struct; parameters and files are read only from the request type and the structs it embeds without a JSON name", t, f.Name, key))
				}
			}
		}

		tagName, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if f.Anonymous && embedded && (tagName == "" || tagName == "-") {
			lintParamTags(f.Type, true, seen, problems)
			continue
		}
		if jsonName(f) == "-" {
			continue
		}
		lintParamTags(f.Type, false, seen, problems)
	}
}

// tagKeys returns the keys of a conventional `key:"value" key2:"value"` tag.
// A malformed remainder is returned as a key so the lint reports it.
func tagKeys(tag reflect.StructTag) []string {
	var keys []string
	s := string(tag)
	for {
		s = strings.TrimLeft(s, " ")
		if s == "" {
			return keys
		}

		i := strings.Index(s, ":\"")
		if i <= 0 {
			return append(keys, s)
		}
		keys = append(keys, s[:i])

		quoted, err := strconv.QuotedPrefix(s[i+1:])
		if err != nil {
			return keys
		}
		s = s[i+1+len(quoted):]
	}
}

// lintField rejects a nullable tag on a type whose schema has no type (see
// typeless), the json string option (the schema would describe the
// value unquoted while encoding/json reads and writes it as a string), a
// format tag on a type that sets its own format or reads its own text (see
// formatTagApplies), a nullable tag on a struct reference or a
// types.Optional of a struct (huma cannot express it) and minimum/maximum
// tags that exceed the range
// of a sized integer field (the value would pass the schema and fail Go
// conversion).
func lintField(t reflect.Type, f reflect.StructField) []string {
	var problems []string
	_, options, _ := strings.Cut(f.Tag.Get("json"), ",")
	if slices.Contains(strings.Split(options, ","), "string") {
		problems = append(problems, fmt.Sprintf("%s.%s: the json string option is not supported; the schema and encoding/json would disagree on the value's type", t, f.Name))
	}
	if f.Tag.Get("format") != "" && !formatTagApplies(f.Type) {
		problems = append(problems, fmt.Sprintf("%s.%s: the format tag is not supported on %s, whose format is set by the type; the schema and the decoder would disagree on the value's text", t, f.Name, f.Type))
	}

	// Parameters and form fields report their own Optional problem.
	if f.Type.Kind() == reflect.Pointer && IsOptional(f.Type) && jsonName(f) != "-" {
		problems = append(problems, fmt.Sprintf("%s.%s: %s is a pointer to types.Optional, which already records an absent key; use it by value", t, f.Name, f.Type))
	}

	_, nullableTag := f.Tag.Lookup("nullable")
	if nullableTag && typeless(f.Type) {
		problems = append(problems, fmt.Sprintf("%s.%s: the nullable tag is not supported on %s, whose schema has no type and accepts null already", t, f.Name, f.Type))
	}

	// The advice differs for a pointer, which already accepts null in a
	// request unless it is required. In a response a nil pointer to a struct
	// needs omitempty (see sentNulls), so types.Optional is the way to send
	// null there. An embedded struct whose fields are promoted has no key of
	// its own, a file field is no JSON value, and a json:"-" field is not
	// read from the body, so the tag means nothing on any of them. huma
	// would write the tag on a types.Optional of a struct as the type list
	// ["", "null"] next to the anyOf that accepts null, which no JSON Schema
	// reader accepts.
	ft := deref(f.Type)
	elem, ok := optionalElem(ft)
	optionalObject := ok && deref(elem).Kind() == reflect.Struct && !isOpaque(deref(elem))
	object := ft.Kind() == reflect.Struct && !isOpaque(ft)
	if f.Tag.Get("nullable") == "true" && (object || optionalObject) {
		_, isForm := f.Tag.Lookup("form")
		tagName, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		switch {
		case f.Anonymous && tagName == "":
			problems = append(problems, fmt.Sprintf("%s.%s: the nullable tag does not apply to an embedded struct, whose fields are promoted into the enclosing object; drop the tag", t, f.Name))
		case isForm && f.Type == reflect.TypeFor[*multipart.FileHeader]():
			problems = append(problems, fmt.Sprintf("%s.%s: the nullable tag does not apply to a file field, which is optional unless it is required:\"true\"; drop the tag", t, f.Name))
		case f.Tag.Get("json") == "-":
			problems = append(problems, fmt.Sprintf("%s.%s: the nullable tag does not apply to a field that is not read from the body (json:\"-\"); drop the tag", t, f.Name))
		case optionalObject:
			problems = append(problems, fmt.Sprintf("%s.%s: the nullable tag is not supported on a types.Optional of a struct, which accepts null already; drop the tag", t, f.Name))
		case f.Type.Kind() == reflect.Pointer:
			problems = append(problems, fmt.Sprintf("%s.%s: nullable tag is not supported on object references; in a request %s already accepts null unless it is required, so drop the tag; to tell null from an absent key, or to send null in a response, use types.Optional[%s], without the nullable tag", t, f.Name, f.Type, ft))
		default:
			problems = append(problems, fmt.Sprintf("%s.%s: nullable tag is not supported on object references; in a request use *%s, which accepts null unless it is required; to tell null from an absent key, or to send null in a response, use types.Optional[%s], without the nullable tag", t, f.Name, ft, ft))
		}
	}

	if ok {
		ft = deref(elem)
	}
	// huma puts bounds on the items of an array, but on the base64 string
	// of a []byte and on the text of a type with its own schema (uuid.UUID).
	if !isOpaque(ft) && (ft.Kind() == reflect.Array || (ft.Kind() == reflect.Slice && ft.Elem().Kind() != reflect.Uint8)) {
		ft = deref(ft.Elem())
	}
	if isOpaque(ft) {
		return problems
	}
	lo, hi, ok := intRange(ft.Kind())
	if !ok {
		return problems
	}

	value, err := strconv.ParseFloat(f.Tag.Get("minimum"), 64)
	if err == nil && value < lo {
		problems = append(problems, fmt.Sprintf("%s.%s: minimum %v is below the %s range", t, f.Name, value, ft.Kind()))
	}
	value, err = strconv.ParseFloat(f.Tag.Get("maximum"), 64)
	if err == nil && hi != nil && value > *hi {
		problems = append(problems, fmt.Sprintf("%s.%s: maximum %v is above the %s range", t, f.Name, value, ft.Kind()))
	}
	return problems
}

// formatTagApplies reports whether a format tag on a field of type t (after
// a pointer and a types.Optional) describes text the registry decodes as it
// is: a string kind, or a slice of them (huma puts the tag on the items),
// that has no format in builtinSchemas and does not decode its own text.
// Elsewhere the decoder reads the type's own text (RFC 3339 for a
// time.Time), which the tag would contradict.
func formatTagApplies(t reflect.Type) bool {
	t = deref(t)
	elem, ok := optionalElem(t)
	if ok {
		t = deref(elem)
	}
	if t.Kind() == reflect.Slice && !decodesOwnText(t) {
		t = deref(t.Elem())
	}
	return t.Kind() == reflect.String && builtinSchemas[t].Format == "" && !decodesOwnText(t)
}

// refusesEmptyText reports whether a minLength tag on a field of type t
// replaces the minLength 1 that builtinSchemas gives a type whose decoder
// reads "" as its zero value (netip.Prefix). The tag reaches that type
// through any number of pointers and types.Optional, in any order, and
// through one slice or array, on whose items huma puts it: *[]*T,
// []types.Optional[T] and types.Optional[[]T] all carry it to T. Under a
// second slice or array huma puts it on the inner array, on a map it stays
// on the object, and on a type with its own text decoder it is that type's
// own; misplacedTags refuses the first two, and none reaches T.
func refusesEmptyText(t reflect.Type) bool {
	items := false
	for {
		t = deref(t)
		_, builtin := builtinSchemas[t]
		if builtin {
			return builtinSchemas[t].MinLength != nil
		}
		elem, ok := optionalElem(t)
		if ok {
			t = elem
			continue
		}
		if isOpaque(t) || items || (t.Kind() != reflect.Slice && t.Kind() != reflect.Array) {
			return false
		}
		items = true
		t = t.Elem()
	}
}

// decodesOwnText reports whether t, or a pointer to it, implements
// encoding.TextUnmarshaler or decodes its own JSON (decodesOwnJSON).
func decodesOwnText(t reflect.Type) bool {
	ptr := reflect.PointerTo(t)
	return ptr.Implements(textUnmarshalerType) || decodesOwnJSON(ptr)
}
