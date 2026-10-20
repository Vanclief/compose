package openapi

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/vanclief/ez"
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
// param tag and property names that huma's Downgrade would rename.
func lintType(t reflect.Type) error {
	var problems []string
	lintWalk(t, map[reflect.Type]bool{}, &problems)
	if len(problems) == 0 {
		return nil
	}
	return ez.New(ez.EINVALID, "openapi: "+strings.Join(problems, "; "), nil)
}

func lintWalk(t reflect.Type, seen map[reflect.Type]bool, problems *[]string) {
	t = deref(t)
	if seen[t] || isOpaque(t) {
		return
	}
	seen[t] = true

	switch t.Kind() {
	case reflect.Slice, reflect.Array, reflect.Map:
		lintWalk(t.Elem(), seen, problems)
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

		lintWalk(f.Type, seen, problems)
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

// lintField rejects a nullable tag on a struct reference (huma cannot
// express it; use a pointer) and minimum/maximum tags that exceed the range
// of a sized integer field (the value would pass the schema and fail Go
// conversion).
func lintField(t reflect.Type, f reflect.StructField) []string {
	var problems []string
	ft := deref(f.Type)
	if f.Tag.Get("nullable") == "true" && ft.Kind() == reflect.Struct && !isOpaque(ft) {
		problems = append(problems, fmt.Sprintf("%s.%s: nullable tag is not supported on object references; use a pointer", t, f.Name))
	}

	if (ft.Kind() == reflect.Slice || ft.Kind() == reflect.Array) && ft.Elem().Kind() != reflect.Uint8 {
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
