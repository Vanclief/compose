package openapi

import (
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/vanclief/ez"

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
// embeds, where the registry never reads it; and a map keyed by an enum
// type, whose keys nothing validates (see enumMapKey). A field that holds a
// url.URL is refused, as is one that holds an interface type with methods
// in a request (see unsupportedType).
func lintType(t reflect.Type, request bool) error {
	var problems []string
	lintWalk(t, request, map[reflect.Type]bool{}, &problems)
	if request {
		lintParamTags(t, true, map[paramVisit]bool{}, &problems)
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
		if !f.IsExported() {
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

		*problems = append(*problems, lintField(t, f)...)

		name := jsonName(f)
		if name == "-" {
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
		}
		problem := unsupportedType(heldType(f.Type), request)
		if problem != "" {
			*problems = append(*problems, fmt.Sprintf("%s.%s: %s", t, f.Name, problem))
		}

		lintWalk(f.Type, request, seen, problems)
	}
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

// paramVisit is a struct lintParamTags walked, and whether it was reached
// from the request type through embedding only. A struct both embedded and
// reached through a named field is walked both ways.
type paramVisit struct {
	t        reflect.Type
	embedded bool
}

// lintParamTags reports a field tagged path, query, header or form in a
// struct reached from the request type other than through embedding: a
// named field, a slice, array or map element, or an Optional's element.
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
			continue
		}

		if !embedded {
			for _, key := range paramTags {
				_, tagged := f.Tag.Lookup(key)
				if tagged {
					*problems = append(*problems, fmt.Sprintf("%s.%s: the %s tag is ignored on a nested struct; parameters and files are read only from the request type and the structs it embeds", t, f.Name, key))
				}
			}
		}

		if f.Anonymous && embedded {
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

// lintField rejects the json string option (the schema would describe the
// value unquoted while encoding/json reads and writes it as a string), a
// format tag on a type that sets its own format or reads its own text (see
// formatTagApplies), a nullable tag on a struct reference (huma cannot
// express it; use a pointer) and minimum/maximum tags that exceed the range
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

	ft := deref(f.Type)
	if f.Tag.Get("nullable") == "true" && ft.Kind() == reflect.Struct && !isOpaque(ft) {
		problems = append(problems, fmt.Sprintf("%s.%s: nullable tag is not supported on object references; use a pointer", t, f.Name))
	}

	elem, ok := optionalElem(ft)
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

// decodesOwnText reports whether t, or a pointer to it, implements
// encoding.TextUnmarshaler or json.Unmarshaler.
func decodesOwnText(t reflect.Type) bool {
	ptr := reflect.PointerTo(t)
	return ptr.Implements(textUnmarshalerType) || ptr.Implements(jsonUnmarshalerType)
}
