package api

import (
	"strconv"
	"strings"

	"github.com/danielgtaylor/huma/v2"

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
// `retries` without the validator ever seeing it.
func bodyCollisions(body any, schema *huma.Schema, components map[string]*huma.Schema) []validate.FieldError {
	var fields []validate.FieldError
	walkCollisions(body, schema, components, "", &fields, 0)
	return fields
}

const maxCollisionDepth = 64

func walkCollisions(value any, schema *huma.Schema, components map[string]*huma.Schema, path string, fields *[]validate.FieldError, depth int) {
	if schema == nil || value == nil || depth > maxCollisionDepth {
		return
	}

	schema = resolveSchema(schema, components)
	if schema == nil {
		return
	}

	switch v := value.(type) {
	case map[string]any:
		if len(schema.Properties) == 0 {
			// Maps with additionalProperties schemas: check each value. Map
			// keys are client-chosen and unbounded, so the path shows "*".
			extra, ok := schema.AdditionalProperties.(*huma.Schema)
			if ok {
				for _, item := range v {
					walkCollisions(item, extra, components, join(path, mapKeySegment), fields, depth+1)
				}
			}
			return
		}

		for key, item := range v {
			prop, ok := schema.Properties[key]
			if ok {
				walkCollisions(item, prop, components, join(path, key), fields, depth+1)
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
			walkCollisions(item, schema.Items, components, join(path, strconv.Itoa(i)), fields, depth+1)
		}
	}
}

// resolveSchema follows $ref and picks the non-null branch of a nullable
// anyOf/oneOf wrapper, returning the schema that describes the value.
func resolveSchema(schema *huma.Schema, components map[string]*huma.Schema) *huma.Schema {
	for i := 0; i < maxCollisionDepth && schema != nil; i++ {
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
