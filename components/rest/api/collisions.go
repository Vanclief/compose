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
// components.
func bodyCollisions(body any, schema *huma.Schema, components map[string]*huma.Schema, provided func(name string) bool) []validate.FieldError {
	var fields []validate.FieldError
	walkCollisions(body, schema, components, provided, false, "", &fields)
	return fields
}

// maxRefHops bounds the $ref and nullable wrapper hops resolveSchema follows.
const maxRefHops = 64

// walkCollisions has no depth limit of its own: it descends the decoded
// body, which readBody has already refused when nested deeper than
// maxBodyDepth, as the schema validator does. own is true while the schema
// is the text of a JSONSchema method, up to the next $ref.
func walkCollisions(value any, schema *huma.Schema, components map[string]*huma.Schema, provided func(name string) bool, own bool, path string, fields *[]validate.FieldError) {
	if schema == nil || value == nil {
		return
	}

	schema, ref := resolveRef(schema, components)
	if schema == nil {
		return
	}
	if ref != "" {
		own = provided(ref)
	}

	switch v := value.(type) {
	case map[string]any:
		if len(schema.Properties) == 0 {
			// Maps with additionalProperties schemas: check each value. Map
			// keys are client-chosen and unbounded, so the path shows "*".
			extra, ok := schema.AdditionalProperties.(*huma.Schema)
			if ok {
				for _, item := range v {
					walkCollisions(item, extra, components, provided, own, join(path, mapKeySegment), fields)
				}
			}
			return
		}

		for key, item := range v {
			prop, ok := schema.Properties[key]
			if ok {
				walkCollisions(item, prop, components, provided, own, join(path, key), fields)
				continue
			}
			if own {
				continue
			}
			for name := range schema.Properties {
				if strings.EqualFold(key, name) {
					*fields = append(*fields, validate.FieldError{
						In:      "body",
						Name:    join(path, key),
						Message: "is not a known field, did you mean " + strconv.Quote(name),
					})
					break
				}
			}
		}
	case []any:
		for i, item := range v {
			walkCollisions(item, schema.Items, components, provided, own, join(path, strconv.Itoa(i)), fields)
		}
	}
}

// resolveSchema follows $ref and picks the non-null branch of a nullable
// anyOf/oneOf wrapper, returning the schema that describes the value.
func resolveSchema(schema *huma.Schema, components map[string]*huma.Schema) *huma.Schema {
	schema, _ = resolveRef(schema, components)
	return schema
}

// resolveRef is resolveSchema that also returns the name of the last
// component it followed a $ref to, or "" when it followed none.
func resolveRef(schema *huma.Schema, components map[string]*huma.Schema) (*huma.Schema, string) {
	ref := ""
	for i := 0; i < maxRefHops && schema != nil; i++ {
		if schema.Ref != "" {
			ref = strings.TrimPrefix(schema.Ref, openapi.RefPrefix)
			schema = components[ref]
			continue
		}

		branches := schema.AnyOf
		if len(branches) == 0 {
			branches = schema.OneOf
		}
		if len(branches) == 0 {
			return schema, ref
		}

		var next *huma.Schema
		for _, branch := range branches {
			if branch.Type == "null" {
				continue
			}
			if next != nil {
				// Ambiguous unions are not produced by the reflector; skip.
				return nil, ref
			}
			next = branch
		}
		schema = next
	}

	return nil, ref
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}
