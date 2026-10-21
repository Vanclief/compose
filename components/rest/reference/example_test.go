package reference

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

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

// emmaExampleSizes renders the request and response examples of every emma
// operation and returns their total bytes and the longest one in lines.
func emmaExampleSizes(t *testing.T) (int, int) {
	t.Helper()
	doc := loadEmma(t, "emma.yaml")
	total := 0
	longest := 0
	measure := func(mt *MediaType, mode ExampleMode) {
		out := doc.MediaExampleJSON(mt, mode)
		total += len(out)
		longest = max(longest, strings.Count(out, "\n")+1)
	}
	for _, path := range doc.Paths.Keys {
		item := doc.Paths.Map[path]
		if item == nil {
			continue
		}
		for _, m := range item.Methods() {
			body := doc.RequestBody(m.Operation.RequestBody)
			if body != nil {
				for _, ct := range body.Content.Keys {
					measure(body.Content.Map[ct], ForRequest)
				}
			}
			for _, code := range m.Operation.Responses.Keys {
				r := doc.Response(m.Operation.Responses.Map[code])
				if r == nil {
					continue
				}
				for _, ct := range r.Content.Keys {
					measure(r.Content.Map[ct], ForResponse)
				}
			}
		}
	}
	return total, longest
}

func TestExampleSizesEmma(t *testing.T) {
	total, longest := emmaExampleSizes(t)
	t.Logf("emma examples: %d bytes, longest %d lines", total, longest)

	// Without the model nesting cap these were 2.3 MB and 1,102 lines.
	require.Less(t, total, 1_000_000)
	require.Less(t, longest, 500)
}

const chainSpec = `
openapi: 3.1.0
info:
  title: Chain
  version: "1"
components:
  schemas:
    Level1:
      type: object
      properties:
        name:
          type: string
        next:
          $ref: '#/components/schemas/Level2'
    Level2:
      type: object
      properties:
        name:
          type: string
        next:
          $ref: '#/components/schemas/Level3'
    Level3:
      type: object
      properties:
        name:
          type: string
        code:
          $ref: '#/components/schemas/Code'
        inline:
          type: object
          properties:
            deep:
              type: object
              properties:
                flag:
                  type: boolean
        next:
          $ref: '#/components/schemas/Level4'
        list:
          type: array
          items:
            $ref: '#/components/schemas/Level4'
        pinned:
          $ref: '#/components/schemas/Level4'
          example:
            name: pinned
        documented:
          $ref: '#/components/schemas/Documented'
    Level4:
      type: object
      properties:
        name:
          type: string
        next:
          $ref: '#/components/schemas/Level5'
    Level5:
      type: object
      properties:
        name:
          type: string
    Code:
      type: string
      format: uuid
    Documented:
      type: object
      properties:
        name:
          type: string
      example:
        name: documented
paths: {}
`

func TestExampleValueCapsNamedModelNesting(t *testing.T) {
	doc := parseSpec(t, chainSpec)

	root := decode(t, doc.ExampleJSON(schemaRef("Level1"), ForResponse))
	level2 := root["next"].(map[string]any)
	level3 := level2["next"].(map[string]any)
	require.Equal(t, "string", level3["name"], "the grandchild model still expands")
	require.Equal(t, "123e4567-e89b-12d3-a456-426614174000", level3["code"], "named scalars are not cut")
	require.Equal(t, map[string]any{"deep": map[string]any{"flag": true}}, level3["inline"], "inline objects do not count")

	require.Equal(t, map[string]any{}, level3["next"], "the great-grandchild model is cut")
	require.Equal(t, []any{}, level3["list"], "arrays of cut models are empty")

	// Explicit examples beyond the cap are used as written.
	require.Equal(t, map[string]any{"name": "pinned"}, level3["pinned"])
	require.Equal(t, map[string]any{"name": "documented"}, level3["documented"])

	// The cap counts from wherever the example starts.
	level3Root := decode(t, doc.ExampleJSON(schemaRef("Level3"), ForResponse))
	level4 := level3Root["next"].(map[string]any)
	require.Equal(t, map[string]any{"name": "string"}, level4["next"])
	require.Equal(t, []any{map[string]any{"name": "string", "next": map[string]any{"name": "string"}}}, level3Root["list"])
}

const wrappedRecursionSpec = `
openapi: 3.0.3
info: {title: t, version: "1"}
paths: {}
components:
  schemas:
    Node:
      type: object
      properties:
        a: {allOf: [{$ref: '#/components/schemas/Node'}], description: first}
        b: {allOf: [{$ref: '#/components/schemas/Node'}], description: second}
        c: {allOf: [{$ref: '#/components/schemas/Node'}], description: third}
    Level1:
      type: object
      properties:
        next: {allOf: [{$ref: '#/components/schemas/Level2'}], description: wrapped}
    Level2:
      type: object
      properties:
        next: {allOf: [{$ref: '#/components/schemas/Level3'}], description: wrapped}
    Level3:
      type: object
      properties:
        next: {allOf: [{$ref: '#/components/schemas/Level4'}], description: wrapped}
    Level4:
      type: object
      properties:
        name: {type: string}
`

// A model that recurs through the 3.0 {allOf: [$ref], description} wrapper
// used to be stopped only by the depth cap, which with three such properties
// produced about 100 MB of JSON.
func TestExampleValueRecursionThroughAllOfWrappers(t *testing.T) {
	doc := parseSpec(t, wrappedRecursionSpec)

	start := time.Now()
	out := doc.ExampleJSON(schemaRef("Node"), ForResponse)
	require.Less(t, time.Since(start), time.Second)
	require.Less(t, len(out), 2048)
	require.Equal(t, map[string]any{"a": map[string]any{}, "b": map[string]any{}, "c": map[string]any{}}, decode(t, out))

	// Wrapped models count toward the nesting cap too.
	level1 := decode(t, doc.ExampleJSON(schemaRef("Level1"), ForResponse))
	level3 := level1["next"].(map[string]any)["next"].(map[string]any)
	require.Equal(t, map[string]any{}, level3["next"])
}

func TestExampleValueAllOfPolymorphism(t *testing.T) {
	doc := parseSpec(t, polymorphicSpec)

	cat := decode(t, doc.ExampleJSON(schemaRef("Cat"), ForRequest))
	require.Equal(t, map[string]any{"petType": "string", "name": "string", "lives": float64(0)}, cat)

	pet := decode(t, doc.ExampleJSON(schemaRef("Pet"), ForRequest))
	require.Equal(t, map[string]any{"petType": "cat", "name": "string", "lives": float64(0)}, pet)
}

// nestedOneOfSpec chains named oneOf schemas: each of L0..L(levels-1) has
// width object members holding width fields of the next level. None of them
// is a model, so the model cap does not apply and an uncapped example grows
// by width^2 per level.
func nestedOneOfSpec(levels, width int) string {
	spec := `
openapi: 3.1.0
info: {title: Nested, version: "1"}
paths:
  /x:
    post:
      requestBody:
        content:
          application/json:
            schema:
              $ref: '#/components/schemas/L0'
      responses:
        "200":
          description: ok
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/L0'
components:
  schemas:
`
	for l := 0; l < levels; l++ {
		spec += fmt.Sprintf("    L%d:\n      oneOf:\n", l)
		for m := 0; m < width; m++ {
			spec += "        - type: object\n          properties:\n"
			for p := 0; p < width; p++ {
				spec += fmt.Sprintf("            p%d:\n              $ref: '#/components/schemas/L%d'\n", p, l+1)
			}
		}
	}
	spec += fmt.Sprintf("    L%d:\n      type: string\n", levels)
	return spec
}

func TestExampleValueBoundsNestedOneOf(t *testing.T) {
	doc := parseSpec(t, nestedOneOfSpec(8, 8))

	example := doc.ExampleJSON(schemaRef("L0"), ForResponse)
	require.LessOrEqual(t, strings.Count(example, `"string"`), maxExampleValues)
	require.Less(t, len(example), 500_000)

	start := time.Now()
	html := renderPage(t, doc)
	require.Less(t, len(html), 10_000_000)
	require.Less(t, time.Since(start), 10*time.Second)

	// Every copy of a repeated array item counts: twelve levels of arrays
	// with minItems 3 would otherwise write 3^12 strings.
	deep := &Schema{Type: TypeList{"string"}}
	three := 3
	for range 12 {
		deep = &Schema{Type: TypeList{"array"}, MinItems: &three, Items: deep}
	}
	example = doc.ExampleJSON(deep, ForResponse)
	require.LessOrEqual(t, strings.Count(example, `"string"`), maxExampleValues)
}

func TestStringExampleClampsMinLength(t *testing.T) {
	huge := 1 << 40
	v := stringExample(&Schema{Type: TypeList{"string"}, MinLength: &huge})
	require.Len(t, v, maxExampleString)

	doc := parseSpec(t, `
openapi: 3.1.0
info: {title: Long, version: "1"}
paths: {}
components:
  schemas:
    Long:
      type: string
      format: uuid
      minLength: 1099511627776
`)
	require.Len(t, doc.ExampleValue(schemaRef("Long"), ForResponse), maxExampleString)
	renderPage(t, doc)
}

// TestExampleValueNilMember covers a schema built in Go, which Parse has
// not cleaned, whose first oneOf member is nil.
func TestExampleValueNilMember(t *testing.T) {
	doc := parseSpec(t, exampleSpec)
	s := &Schema{
		Type:          TypeList{"object"},
		Properties:    Ordered[*Schema]{Keys: []string{"kind"}, Map: map[string]*Schema{"kind": {Type: TypeList{"string"}}}},
		OneOf:         []*Schema{nil, schemaRef("Owner")},
		Discriminator: &Discriminator{PropertyName: "kind"},
	}
	require.Equal(t, map[string]any{"kind": "string"}, decode(t, doc.ExampleJSON(s, ForResponse)))
}

// explicitExamplesSpec builds a spec of three models, A with width
// properties of B, B with width properties of C, and C with width string
// properties each carrying an example of size bytes, directly or, with oneOf,
// on their only oneOf member.
func explicitExamplesSpec(width, size int, oneOf bool) string {
	var b strings.Builder
	b.WriteString(`{"openapi":"3.1.0","info":{"title":"Explicit","version":"1"},"paths":{},"components":{"schemas":{`)
	example := strings.Repeat("x", size)
	models := []string{"A", "B", "C"}
	for i, name := range models {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `%q:{"type":"object","properties":{`, name)
		for p := range width {
			if p > 0 {
				b.WriteString(",")
			}
			if name == "C" && oneOf {
				fmt.Fprintf(&b, `"p%d":{"oneOf":[{"type":"string","example":%q}]}`, p, example)
				continue
			}
			if name == "C" {
				fmt.Fprintf(&b, `"p%d":{"type":"string","example":%q}`, p, example)
				continue
			}
			fmt.Fprintf(&b, `"p%d":{"$ref":"#/components/schemas/%s"}`, p, models[i+1])
		}
		b.WriteString("}}")
	}
	b.WriteString("}}}")
	return b.String()
}

// TestExampleValueBoundsExplicitExamples covers explicit examples, which the
// value cap counts per KiB: 100 x 100 x 100 leaves of 10 KB would otherwise
// write about 5 GB.
func TestExampleValueBoundsExplicitExamples(t *testing.T) {
	for _, oneOf := range []bool{false, true} {
		doc := parseSpec(t, explicitExamplesSpec(100, 10_000, oneOf))

		start := time.Now()
		example := doc.ExampleJSON(schemaRef("A"), ForResponse)
		elapsed := time.Since(start)
		t.Logf("oneOf %t: %d bytes in %s", oneOf, len(example), elapsed)

		require.Less(t, len(example), 10_000_000)
		require.Less(t, elapsed, 5*time.Second)
		require.True(t, strings.HasPrefix(example, "{\n  \"p0\": {\n    \"p0\": {\n      \"p0\": \"xxx"), example[:min(len(example), 200)])
	}
}

func TestExampleValueKeepsExplicitExamplesUnderCap(t *testing.T) {
	doc := parseSpec(t, `
openapi: 3.1.0
info: {title: Small, version: "1"}
paths: {}
components:
  schemas:
    Small:
      type: object
      properties:
        name: {type: string, example: Ada}
        tags: {type: array, items: {type: string}, example: [a, b]}
        meta: {type: object, example: {plan: pro, seats: 3}}
        note: {type: string, example: `+strings.Repeat("n", 4096)+`}
`)
	require.Equal(t, map[string]any{
		"name": "Ada",
		"tags": []any{"a", "b"},
		"meta": map[string]any{"plan": "pro", "seats": float64(3)},
		"note": strings.Repeat("n", 4096),
	}, decode(t, doc.ExampleJSON(schemaRef("Small"), ForResponse)))
}
