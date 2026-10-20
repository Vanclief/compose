package openapi

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/vanclief/ez"
)

// Param is a path, query or header parameter of a request type.
type Param struct {
	Name       string       // wire name, header names lowercased
	In         string       // "path" | "query" | "header"
	Required   bool         // always true for path params
	Schema     *huma.Schema // inline schema (may be a $ref for enum types); Description lives in Doc
	Index      []int        // reflect field index path into the request struct
	Doc        string       // from doc tag
	Deprecated bool
}

var paramLocations = []string{"path", "query", "header"}

// buildParams walks the visible fields of t (including embedded structs) and
// returns one Param per path/query/header tagged field.
func buildParams(reg *registry, t reflect.Type) ([]Param, error) {
	var params []Param
	var problems []string
	seen := map[string]bool{}

	for _, f := range reflect.VisibleFields(t) {
		if !f.IsExported() || f.Anonymous {
			continue
		}

		var in, name string
		for _, loc := range paramLocations {
			value, ok := f.Tag.Lookup(loc)
			if !ok {
				continue
			}
			if in != "" {
				problems = append(problems, fmt.Sprintf("%s.%s: has both %s and %s tags", t, f.Name, in, loc))
			}
			in = loc
			name = value
		}
		if in == "" {
			continue
		}

		where := fmt.Sprintf("%s.%s", t, f.Name)
		if name == "" {
			problems = append(problems, fmt.Sprintf("%s: empty %s tag", where, in))
			continue
		}
		if jsonName(f) != "-" {
			problems = append(problems, fmt.Sprintf("%s: %s parameter fields must carry json:\"-\"", where, in))
		}
		if !scalarParamType(f.Type) {
			problems = append(problems, fmt.Sprintf("%s: struct %s parameters are not supported", where, in))
			continue
		}

		if in == "header" {
			name = strings.ToLower(name)
		}
		key := in + ":" + name
		if seen[key] {
			problems = append(problems, fmt.Sprintf("%s: duplicate %s parameter %q", where, in, name))
			continue
		}
		seen[key] = true

		schema := huma.SchemaFromField(reg, f, "")
		deprecated := schema.Deprecated
		schema.Nullable = false
		schema.Description = ""
		schema.Deprecated = false

		params = append(params, Param{
			Name:       name,
			In:         in,
			Required:   in == "path" || f.Tag.Get("required") == "true",
			Schema:     schema,
			Index:      f.Index,
			Doc:        f.Tag.Get("doc"),
			Deprecated: deprecated,
		})
	}

	if len(problems) > 0 {
		return nil, ez.New(ez.EINVALID, "openapi: "+strings.Join(problems, "; "), nil)
	}

	return params, nil
}

// scalarParamType rejects structs (and slices of structs) that cannot be
// parsed from text.
func scalarParamType(t reflect.Type) bool {
	t = deref(t)
	if t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
		if isOpaque(t) {
			return true
		}
		t = deref(t.Elem())
	}
	return t.Kind() != reflect.Struct || isOpaque(t)
}

func copyParams(params []Param) []Param {
	out := make([]Param, len(params))
	for i, p := range params {
		p.Schema = copySchema(p.Schema)
		p.Index = append([]int(nil), p.Index...)
		out[i] = p
	}
	return out
}

// jsonName returns the JSON property name of f ("-" when skipped).
func jsonName(f reflect.StructField) string {
	name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
	if name == "" {
		return f.Name
	}
	return name
}
