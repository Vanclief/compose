package apidocs

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

const exampleSpec = `
openapi: 3.1.0
info:
  title: Pets
  version: "1"
components:
  examples:
    Rex:
      value:
        name: Rex
  schemas:
    Pet:
      type: object
      properties:
        id:
          type: string
          format: uuid
          readOnly: true
        name:
          type: string
        password:
          type: string
          format: password
          writeOnly: true
        born_at:
          type: string
          format: date-time
        born_on:
          type: string
          format: date
        nickname:
          type: [string, "null"]
        status:
          type: string
          enum: [available, sold]
        weight:
          type: number
          minimum: 1.5
        age:
          type: integer
          format: int32
        vaccinated:
          type: boolean
        tags:
          type: array
          items:
            type: string
        labels:
          type: object
          additionalProperties:
            type: integer
        meta:
          type: object
        owner:
          $ref: '#/components/schemas/Owner'
    Owner:
      type: object
      properties:
        email:
          type: string
          format: email
        best_friend:
          $ref: '#/components/schemas/Owner'
        pets:
          type: array
          items:
            $ref: '#/components/schemas/Owner'
    Node:
      type: object
      properties:
        children:
          type: array
          items:
            allOf:
              - $ref: '#/components/schemas/Node'
    Cat:
      type: object
      properties:
        kind:
          type: string
        meows:
          type: boolean
    Dog:
      type: object
      properties:
        kind:
          type: string
        barks:
          type: boolean
    Animal:
      oneOf:
        - $ref: '#/components/schemas/Cat'
        - $ref: '#/components/schemas/Dog'
      discriminator:
        propertyName: kind
        mapping:
          cat: '#/components/schemas/Cat'
          dog: '#/components/schemas/Dog'
    Implicit:
      oneOf:
        - $ref: '#/components/schemas/Dog'
      discriminator:
        propertyName: kind
    Contact:
      type: object
      properties:
        email:
          type: string
        phone:
          type: string
      anyOf:
        - required: [email]
        - required: [phone]
    Choice:
      oneOf:
        - type: integer
        - type: string
    Explicit:
      type: string
      format: uuid
      example: from-example
      examples: [from-examples]
      default: from-default
      enum: [from-enum]
    Defaulted:
      type: string
      default: from-default
      enum: [from-enum]
    Padded:
      type: string
      minLength: 10
    Many:
      type: array
      minItems: 5
      items:
        type: integer
    Composed:
      allOf:
        - $ref: '#/components/schemas/Cat'
        - type: object
          properties:
            lives:
              type: integer
              minimum: 9
    Html:
      type: string
      example: "<b>&</b>"
paths: {}
`

func parseSpec(t *testing.T, spec string) *Document {
	t.Helper()
	doc, err := Parse([]byte(spec))
	require.NoError(t, err)
	return doc
}

func schemaRef(name string) *Schema {
	return &Schema{Ref: refSchemas + name}
}

// decode round-trips the JSON so assertions do not depend on Go types.
func decode(t *testing.T, s string) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(s), &out))
	return out
}

func orderedKeys(t *testing.T, v any) []string {
	t.Helper()
	obj, ok := v.(*Ordered[any])
	require.True(t, ok, "expected *Ordered[any], got %T", v)
	return obj.Keys
}

func TestExampleValueObjectPropertiesAndModes(t *testing.T) {
	doc := parseSpec(t, exampleSpec)

	request := doc.ExampleValue(schemaRef("Pet"), ForRequest)
	require.Equal(t, []string{
		"name", "password", "born_at", "born_on", "nickname", "status", "weight",
		"age", "vaccinated", "tags", "labels", "meta", "owner",
	}, orderedKeys(t, request))

	response := doc.ExampleValue(schemaRef("Pet"), ForResponse)
	keys := orderedKeys(t, response)
	require.Equal(t, "id", keys[0])
	require.NotContains(t, keys, "password")

	pet := decode(t, doc.ExampleJSON(schemaRef("Pet"), ForResponse))
	require.Equal(t, "123e4567-e89b-12d3-a456-426614174000", pet["id"])
	require.Equal(t, "string", pet["name"])
	require.Equal(t, "2024-01-15T09:30:00Z", pet["born_at"])
	require.Equal(t, "2024-01-15", pet["born_on"])
	require.Equal(t, "string", pet["nickname"], "nullable schemas still produce a value")
	require.Equal(t, "available", pet["status"], "enum picks the first value")
	require.Equal(t, 1.5, pet["weight"])
	require.Equal(t, float64(0), pet["age"])
	require.Equal(t, true, pet["vaccinated"])
	require.Equal(t, []any{"string"}, pet["tags"])
	require.Equal(t, map[string]any{"key": float64(0)}, pet["labels"])
	require.Equal(t, map[string]any{}, pet["meta"])

	owner := pet["owner"].(map[string]any)
	require.Equal(t, "user@example.com", owner["email"])
}

func TestExampleValueRecursionTerminates(t *testing.T) {
	doc := parseSpec(t, exampleSpec)

	owner := decode(t, doc.ExampleJSON(schemaRef("Owner"), ForResponse))
	require.Equal(t, map[string]any{}, owner["best_friend"])
	require.Equal(t, []any{map[string]any{}}, owner["pets"])

	// Node recurses through an inline allOf, which only the depth cap stops.
	out := doc.ExampleJSON(schemaRef("Node"), ForResponse)
	require.Contains(t, out, `"children"`)
}

func TestExampleValueOneOfAndDiscriminator(t *testing.T) {
	doc := parseSpec(t, exampleSpec)

	animal := doc.ExampleValue(schemaRef("Animal"), ForRequest)
	require.Equal(t, []string{"kind", "meows"}, orderedKeys(t, animal))
	require.Equal(t, `{
  "kind": "cat",
  "meows": true
}`, doc.ExampleJSON(schemaRef("Animal"), ForRequest))

	implicit := decode(t, doc.ExampleJSON(schemaRef("Implicit"), ForRequest))
	require.Equal(t, "Dog", implicit["kind"], "without mapping the component name is the tag")

	require.Equal(t, int64(0), doc.ExampleValue(schemaRef("Choice"), ForRequest))

	// anyOf of bare required lists keeps the parent's properties.
	contact := doc.ExampleValue(schemaRef("Contact"), ForRequest)
	require.Equal(t, []string{"email", "phone"}, orderedKeys(t, contact))
}

func TestExampleValuePrecedenceAndConstraints(t *testing.T) {
	doc := parseSpec(t, exampleSpec)

	require.Equal(t, "from-example", doc.ExampleValue(schemaRef("Explicit"), ForRequest))
	require.Equal(t, "from-default", doc.ExampleValue(schemaRef("Defaulted"), ForRequest))
	require.Equal(t, "stringstri", doc.ExampleValue(schemaRef("Padded"), ForRequest))
	require.Equal(t, []any{int64(0), int64(0), int64(0)}, doc.ExampleValue(schemaRef("Many"), ForRequest))

	composed := decode(t, doc.ExampleJSON(schemaRef("Composed"), ForRequest))
	require.Equal(t, map[string]any{"kind": "string", "meows": true, "lives": float64(9)}, composed)

	require.Equal(t, `"<b>&</b>"`, doc.ExampleJSON(schemaRef("Html"), ForRequest), "HTML is not escaped")
	require.Equal(t, "null", doc.ExampleJSON(nil, ForRequest))
	require.Nil(t, doc.ExampleValue(&Schema{}, ForRequest), "untyped schemas have no example")

	formats := map[string]any{
		"time":     "09:30:00Z",
		"uri":      "https://example.com",
		"hostname": "example.com",
		"ipv4":     "192.168.0.1",
		"ipv6":     "2001:db8::1",
		"byte":     "U3dhZ2dlciByb2Nrcw==",
		"binary":   "<binary>",
		"int64":    "string",
	}
	for format, want := range formats {
		require.Equal(t, want, doc.ExampleValue(&Schema{Type: TypeList{"string"}, Format: format}, ForRequest), format)
	}
}

func TestMediaExamplePrecedence(t *testing.T) {
	doc := parseSpec(t, exampleSpec)

	_, ok := doc.MediaExample(nil, ForRequest)
	require.False(t, ok)
	_, ok = doc.MediaExample(&MediaType{}, ForRequest)
	require.False(t, ok)
	require.Equal(t, "", doc.MediaExampleJSON(&MediaType{}, ForRequest))

	mt := &MediaType{Schema: schemaRef("Cat")}
	mt.Examples.Set("rex", &Example{Ref: refExamples + "Rex"})
	v, ok := doc.MediaExample(mt, ForRequest)
	require.True(t, ok)
	require.Equal(t, map[string]any{"name": "Rex"}, v)

	mt.Example = "inline"
	v, _ = doc.MediaExample(mt, ForRequest)
	require.Equal(t, "inline", v)

	require.Equal(t, `{
  "kind": "string",
  "meows": true
}`, doc.MediaExampleJSON(&MediaType{Schema: schemaRef("Cat")}, ForRequest))
}

func TestToJSONKeepsOrderAndHandlesYAMLValues(t *testing.T) {
	obj := &Ordered[any]{}
	obj.Set("z", 1)
	obj.Set("a", map[any]any{"k": []any{"<x>", nil}})
	require.Equal(t, `{
  "z": 1,
  "a": {
    "k": [
      "<x>",
      null
    ]
  }
}`, ToJSON(obj))
	require.Equal(t, "null", ToJSON(nil))
	require.Equal(t, "{}", ToJSON(&Ordered[any]{}))
}

func TestExampleJSONEmma(t *testing.T) {
	doc := loadEmma(t, "emma.yaml")

	require.Equal(t, `{
  "code": "string",
  "message": "string",
  "request_id": "string"
}`, doc.ExampleJSON(schemaRef("StandardError"), ForResponse))

	// oneOf of variants whose members use anyOf of bare required lists.
	assign := doc.ExampleValue(schemaRef("application_modules_property_complexes_staff_AssignStaffRoleRequest"), ForRequest)
	require.Contains(t, orderedKeys(t, assign), "complex_id")

	// Every component schema produces an example without looping.
	for _, name := range doc.Components.Schemas.Keys {
		for _, mode := range []ExampleMode{ForRequest, ForResponse} {
			out := doc.ExampleJSON(schemaRef(name), mode)
			require.NotEmpty(t, out, name)
			require.True(t, json.Valid([]byte(out)), name)
		}
	}
}
