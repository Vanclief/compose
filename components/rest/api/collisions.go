package api

import (
	"strconv"
	"strings"

	"github.com/vanclief/compose/components/rest/api/internal/huma"
	"github.com/vanclief/compose/components/rest/api/openapi"
	"github.com/vanclief/compose/components/rest/api/validate"
)

// mapKeySegment replaces map keys in reported paths.
const mapKeySegment = "*"

// bodyCollisions walks a decoded body alongside its schema and reports every
// key that is unknown to the schema but matches a declared property ignoring
// case. Paths keep declared property names, array indices and the colliding
// key itself (it case-folds to a declared name, so it is bounded); map keys
// become "*". encoding/json matches struct fields case-insensitively at every
// depth, so without this check a nested {"Retries": 1} would bind to
// `retries` without the validator ever seeing it. The properties a
// JSONSchema method's text declares (provided reports its components) are
// read by the type's own decoder, not matched that way, so no key is
// reported against them; the walk still follows the text's $refs to struct
// components, through allOf, anyOf and oneOf too.
func bodyCollisions(body any, schema *huma.Schema, components map[string]*huma.Schema, provided func(name string) bool) []validate.FieldError {
	var fields []validate.FieldError
	walkCollisions(body, []applied{{schema: schema}}, components, provided, "", &fields)
	return fields
}

// maxRefHops bounds the $ref and nullable wrapper hops resolveSchema follows.
const maxRefHops = 64

// walkCollisions has no depth limit of its own: it descends the decoded
// body, which readBody has already refused when nested deeper than
// maxBodyDepth, as the schema validator does. roots are the schemas the
// value is held by; every schema that applies to it with them is looked at
// (see appliedSchemas): a key is reported when it is a property of none of
// them and case-folds onto a property of one that is not a JSONSchema
// method's text. Each value is walked once, against all the schemas that
// declare it, so the walk does not grow with the number of union branches
// that declare the same property at every depth of a recursive text.
func walkCollisions(value any, roots []applied, components map[string]*huma.Schema, provided func(name string) bool, path string, fields *[]validate.FieldError) {
	switch v := value.(type) {
	case map[string]any:
		list := appliedTo(roots, components, provided)
		for key, item := range v {
			var props []applied
			for _, a := range list {
				prop, ok := a.schema.Properties[key]
				if ok && prop != nil {
					props = append(props, applied{schema: prop, own: a.own})
				}
			}
			if len(props) > 0 {
				walkCollisions(item, props, components, provided, join(path, key), fields)
				continue
			}
			// Map keys are client-chosen and unbounded, so the path shows "*".
			var extras []applied
			for _, a := range list {
				extra, ok := a.schema.AdditionalProperties.(*huma.Schema)
				if ok && extra != nil {
					extras = append(extras, applied{schema: extra, own: a.own})
				}
			}
			if len(extras) > 0 {
				walkCollisions(item, extras, components, provided, join(path, mapKeySegment), fields)
			}
			reportCollision(key, list, path, fields)
		}
	case []any:
		list := appliedTo(roots, components, provided)
		var items []applied
		for _, a := range list {
			if a.schema.Items != nil {
				items = append(items, applied{schema: a.schema.Items, own: a.own})
			}
		}
		if len(items) == 0 {
			return
		}
		for i, item := range v {
			walkCollisions(item, items, components, provided, join(path, strconv.Itoa(i)), fields)
		}
	}
}

// reportCollision reports key, a key of an object at path that no schema in
// list declares, when it case-folds onto a property of a schema in list that
// is not a JSONSchema method's text.
func reportCollision(key string, list []applied, path string, fields *[]validate.FieldError) {
	for _, a := range list {
		if a.own {
			continue
		}
		for prop := range a.schema.Properties {
			if strings.EqualFold(key, prop) {
				*fields = append(*fields, validate.FieldError{
					In:      "body",
					Name:    join(path, key),
					Message: "is not a known field, did you mean " + strconv.Quote(prop),
				})
				return
			}
		}
	}
}

// applied is a schema that applies to a value, and whether it is the text of
// a JSONSchema method.
type applied struct {
	schema *huma.Schema
	own    bool
}

// appliedTo returns roots and every schema that applies to the same value
// with one of them, each once.
func appliedTo(roots []applied, components map[string]*huma.Schema, provided func(name string) bool) []applied {
	var list []applied
	for _, root := range roots {
		appliedSchemas(root.schema, components, provided, root.own, &list)
	}
	return list
}

// appliedSchemas appends to out schema, unless out holds it, and every
// schema that applies to the same value with it: the component of a $ref, whose own is provided(name),
// and the members of allOf, anyOf and oneOf, at any depth. The reflector
// writes no union but anyOf [X, {"type": "null"}], which gives X as before;
// the others come from a JSONSchema method's text.
func appliedSchemas(schema *huma.Schema, components map[string]*huma.Schema, provided func(name string) bool, own bool, out *[]applied) {
	if schema == nil {
		return
	}
	for _, a := range *out {
		if a.schema == schema {
			return
		}
	}
	*out = append(*out, applied{schema: schema, own: own})
	if schema.Ref != "" {
		name := strings.TrimPrefix(schema.Ref, openapi.RefPrefix)
		appliedSchemas(components[name], components, provided, provided(name), out)
	}
	for _, list := range [][]*huma.Schema{schema.AllOf, schema.AnyOf, schema.OneOf} {
		for _, member := range list {
			appliedSchemas(member, components, provided, own, out)
		}
	}
}

// resolveSchema follows $ref and picks the non-null branch of a nullable
// anyOf/oneOf wrapper, returning the schema that describes the value.
func resolveSchema(schema *huma.Schema, components map[string]*huma.Schema) *huma.Schema {
	for i := 0; i < maxRefHops && schema != nil; i++ {
		if schema.Ref != "" {
			schema = components[strings.TrimPrefix(schema.Ref, openapi.RefPrefix)]
			continue
		}

		branches := schema.AnyOf
		if len(branches) == 0 {
			branches = schema.OneOf
		}
		if len(branches) == 0 {
			return schema
		}

		var next *huma.Schema
		for _, branch := range branches {
			if branch.Type == "null" {
				continue
			}
			if next != nil {
				// Ambiguous unions are not produced by the reflector; skip.
				return nil
			}
			next = branch
		}
		schema = next
	}

	return nil
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}
