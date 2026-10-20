package generator

import (
	"reflect"
	"strings"
	"testing"

	"github.com/vanclief/compose/components/rest/handler"
)

// schemaNameByType maps each compose struct to the schema registered for it,
// so struct-typed fields can be checked against their exact $ref target.
var schemaNameByType = map[reflect.Type]string{
	reflect.TypeOf(handler.StandardError{}): standardErrorSchemaName,
	reflect.TypeOf(handler.ErrorResponse{}): errorResponseSchemaName,
}

// The error envelope in registerErrorSchemas is handwritten because the
// schema builder never walks the compose module. This pins it to compose's
// actual structs so a field change there fails here instead of drifting.
func TestErrorSchemasMatchComposeStructs(t *testing.T) {
	schemas := map[string]map[string]any{}
	registerErrorSchemas(schemas)

	assertSchemaMatchesStruct(t, schemas, standardErrorSchemaName, handler.StandardError{})
	assertSchemaMatchesStruct(t, schemas, errorResponseSchemaName, handler.ErrorResponse{})
}

// assertSchemaMatchesStruct checks names, types, requiredness, and $ref
// targets. Field kinds it does not understand fail the test so the schema and
// this test get updated together.
func assertSchemaMatchesStruct(t *testing.T, schemas map[string]map[string]any, schemaName string, s any) {
	t.Helper()

	structType := reflect.TypeOf(s)
	schema := schemas[schemaName]
	if schema == nil {
		t.Fatalf("%s: schema not registered", schemaName)
	}

	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("%s: schema has no properties", structType.Name())
	}

	wantRequired := map[string]struct{}{}
	for i := 0; i < structType.NumField(); i++ {
		field := structType.Field(i)
		tagParts := strings.Split(field.Tag.Get("json"), ",")

		tag := tagParts[0]
		if tag == "" || tag == "-" {
			t.Fatalf("%s.%s: expected a json tag", structType.Name(), field.Name)
		}

		omitempty := false
		for _, part := range tagParts[1:] {
			if part == "omitempty" {
				omitempty = true
			}
		}
		if !omitempty {
			wantRequired[tag] = struct{}{}
		}

		property, ok := properties[tag].(map[string]any)
		if !ok {
			t.Errorf("%s: schema is missing property %q", structType.Name(), tag)
			continue
		}

		switch field.Type.Kind() {
		case reflect.String:
			if property["type"] != "string" {
				t.Errorf("%s.%s: struct field is a string, schema says %v", structType.Name(), tag, property["type"])
			}
		case reflect.Struct:
			want, ok := schemaNameByType[field.Type]
			if !ok {
				t.Errorf("%s.%s: struct type %s not in schemaNameByType — update the schema and this test", structType.Name(), tag, field.Type)
				continue
			}
			ref, _ := property["$ref"].(string)
			if ref != "#/components/schemas/"+want {
				t.Errorf("%s.%s: expected $ref to %s, got %q", structType.Name(), tag, want, ref)
			}
		default:
			t.Errorf("%s.%s: field kind %s not covered — update the schema and this test", structType.Name(), tag, field.Type.Kind())
		}
	}

	required, _ := schema["required"].([]any)
	for _, name := range required {
		_, ok = wantRequired[name.(string)]
		if !ok {
			t.Errorf("%s: schema requires %q but the struct field is omitempty or missing", structType.Name(), name)
		}
	}
	if len(required) != len(wantRequired) {
		t.Errorf("%s: schema requires %d fields, struct has %d always-serialized fields", structType.Name(), len(required), len(wantRequired))
	}

	for name := range properties {
		found := false
		for i := 0; i < structType.NumField(); i++ {
			if strings.Split(structType.Field(i).Tag.Get("json"), ",")[0] == name {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: schema property %q has no struct field", structType.Name(), name)
		}
	}
}
