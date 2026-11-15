package reference

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/vanclief/ez"
	"gopkg.in/yaml.v3"
)

func loadEmma(t *testing.T, name string) *Document {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	require.NoError(t, err)
	doc, err := Parse(data)
	require.NoError(t, err)
	return doc
}

func TestParseEmmaYAMLAndJSONAgree(t *testing.T) {
	yamlDoc := loadEmma(t, "emma.yaml")
	jsonDoc := loadEmma(t, "emma.json")

	require.Equal(t, "3.0.3", yamlDoc.OpenAPI)
	require.Equal(t, "Emma API", yamlDoc.Info.Title)
	require.ElementsMatch(t, yamlDoc.Paths.Keys, jsonDoc.Paths.Keys)
	require.ElementsMatch(t, yamlDoc.Components.Schemas.Keys, jsonDoc.Components.Schemas.Keys)
	require.Len(t, yamlDoc.Paths.Keys, 180)
	require.Len(t, yamlDoc.Components.Schemas.Keys, 633)
	require.Len(t, yamlDoc.Tags, 16)

	item, ok := yamlDoc.Paths.Get("/auth/login")
	require.True(t, ok)
	require.NotNil(t, item.Post)
	require.Equal(t, "auth.login", item.Post.OperationID)
	require.NotNil(t, item.Post.Security)
	require.Empty(t, *item.Post.Security)

	body := yamlDoc.RequestBody(item.Post.RequestBody)
	mt, ok := body.Content.Get("application/json")
	require.True(t, ok)
	schema, name := yamlDoc.Schema(mt.Schema)
	require.Equal(t, "application_services_authentication_login_LoginRequest", name)
	require.Equal(t, "object", schema.Type.Primary())

	require.Equal(t, []string{"200", "default"}, item.Post.Responses.Keys)
}

// parseSchemaBothWays parses a document holding one component schema S,
// written as JSON, once through the JSON decoder and once through the YAML
// decoder (as a YAML flow mapping), and returns S from each.
func parseSchemaBothWays(t *testing.T, version, schemaJSON string) map[string]*Schema {
	t.Helper()

	inputs := map[string]string{
		"json": `{"openapi": "` + version + `", "info": {"title": "t", "version": "1"}, "components": {"schemas": {"S": ` + schemaJSON + `}}}`,
		"yaml": "openapi: " + version + "\ninfo:\n  title: t\n  version: \"1\"\ncomponents:\n  schemas:\n    S: " + schemaJSON + "\n",
	}

	out := map[string]*Schema{}
	for format, input := range inputs {
		doc, err := Parse([]byte(input))
		require.NoError(t, err, format)
		s, ok := doc.Components.Schemas.Get("S")
		require.True(t, ok, format)
		require.NotNil(t, s, format)
		out[format] = s
	}
	return out
}

func TestSchemaTypeArrayWithNull(t *testing.T) {
	for format, s := range parseSchemaBothWays(t, "3.1.0", `{"type": ["string", "null"]}`) {
		require.Equal(t, TypeList{"string"}, s.Type, format)
		require.True(t, s.Nullable, format)
		require.False(t, s.NullMember, format)
		require.Equal(t, "string", s.Type.Primary(), format)
	}
}

func TestSchemaTypeOnlyNull(t *testing.T) {
	for format, s := range parseSchemaBothWays(t, "3.1.0", `{"type": "null"}`) {
		require.Empty(t, s.Type, format)
		require.True(t, s.Nullable, format)
	}
}

// TestSchemaConstNull checks that Parse tells const: null from no const,
// in JSON and in YAML, where null may also be written ~, at any depth.
func TestSchemaConstNull(t *testing.T) {
	for _, c := range []struct {
		schema string
		null   bool
	}{
		{`{"const": null}`, true},
		{`{"type": ["string", "null"], "const": null, "x-a": 1}`, true},
		{`{"const": 1}`, false},
		{`{"const": "null"}`, false},
		{`{"type": "string"}`, false},
		{`{"properties": {"const": {"type": "string"}}}`, false},
	} {
		for format, s := range parseSchemaBothWays(t, "3.1.0", c.schema) {
			require.Equal(t, c.null, s.ConstNull, "%s %s", format, c.schema)
			if c.null {
				require.Nil(t, s.Const, "%s %s", format, c.schema)
			}
		}
	}
	for format, s := range parseSchemaBothWays(t, "3.1.0", `{"properties": {"p": {"items": {"const": null}}}}`) {
		require.True(t, s.Properties.Map["p"].Items.ConstNull, format)
	}
	doc := parseYAML(t, "openapi: 3.1.0\ninfo: {title: t, version: \"1\"}\ncomponents:\n  schemas:\n    S: {const: ~}\n")
	require.True(t, doc.Components.Schemas.Map["S"].ConstNull)

	// A {type: null} member that also says const: null is a null member.
	for format, s := range parseSchemaBothWays(t, "3.1.0", `{"anyOf": [{"type": "string"}, {"type": "null", "const": null}]}`) {
		require.True(t, s.NullMember, format)
		require.Empty(t, s.AnyOf, format)
		require.Len(t, s.AllOf, 1, format)
	}
}

func TestSchemaTypeNullValue(t *testing.T) {
	for format, s := range parseSchemaBothWays(t, "3.1.0", `{"type": null}`) {
		require.Empty(t, s.Type, format)
		require.False(t, s.Nullable, format)
	}
}

func TestSchemaNullable30(t *testing.T) {
	for format, s := range parseSchemaBothWays(t, "3.0.3", `{"type": "integer", "nullable": true}`) {
		require.Equal(t, TypeList{"integer"}, s.Type, format)
		require.True(t, s.Nullable, format)
	}
}

func TestSchemaExclusiveBoundBoolean30(t *testing.T) {
	input := `{"type": "number", "minimum": 5, "exclusiveMinimum": true, "maximum": 10, "exclusiveMaximum": true}`
	for format, s := range parseSchemaBothWays(t, "3.0.3", input) {
		require.Nil(t, s.Minimum, format)
		require.NotNil(t, s.ExclusiveMinimum, format)
		require.NotNil(t, s.ExclusiveMinimum.Value, format)
		require.Equal(t, 5.0, *s.ExclusiveMinimum.Value, format)

		require.Nil(t, s.Maximum, format)
		require.NotNil(t, s.ExclusiveMaximum, format)
		require.Equal(t, 10.0, *s.ExclusiveMaximum.Value, format)
	}
}

func TestSchemaExclusiveBoundFalseKeepsInclusive(t *testing.T) {
	input := `{"minimum": 5, "exclusiveMinimum": false, "exclusiveMaximum": true}`
	for format, s := range parseSchemaBothWays(t, "3.0.3", input) {
		require.NotNil(t, s.Minimum, format)
		require.Equal(t, 5.0, *s.Minimum, format)
		require.Nil(t, s.ExclusiveMinimum, format)
		// A true flag without maximum has nothing to qualify.
		require.Nil(t, s.ExclusiveMaximum, format)
	}
}

func TestSchemaExclusiveBoundNumeric31(t *testing.T) {
	input := `{"type": "integer", "exclusiveMinimum": 5, "minimum": 1, "exclusiveMaximum": 7.5}`
	for format, s := range parseSchemaBothWays(t, "3.1.0", input) {
		require.NotNil(t, s.ExclusiveMinimum, format)
		require.Equal(t, 5.0, *s.ExclusiveMinimum.Value, format)
		require.NotNil(t, s.Minimum, format)
		require.Equal(t, 1.0, *s.Minimum, format)
		require.Equal(t, 7.5, *s.ExclusiveMaximum.Value, format)
	}
}

func TestSchemaAdditionalProperties(t *testing.T) {
	cases := map[string]struct {
		input      string
		allowed    bool
		schemaType string
	}{
		"unset":  {`{"type": "object"}`, true, ""},
		"false":  {`{"type": "object", "additionalProperties": false}`, false, ""},
		"true":   {`{"type": "object", "additionalProperties": true}`, true, ""},
		"schema": {`{"type": "object", "additionalProperties": {"type": ["string", "null"]}}`, true, "string"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			for format, s := range parseSchemaBothWays(t, "3.1.0", tc.input) {
				allowed := s.AdditionalProperties == nil || s.AdditionalProperties.Allowed
				require.Equal(t, tc.allowed, allowed, format)
				if tc.schemaType == "" {
					require.Nil(t, s.AdditionalSchema(), format)
					continue
				}
				require.NotNil(t, s.AdditionalSchema(), format)
				require.Equal(t, tc.schemaType, s.AdditionalSchema().Type.Primary(), format)
				// Normalization reaches additionalProperties schemas too.
				require.True(t, s.AdditionalSchema().Nullable, format)
			}
		})
	}
}

func TestSchemaKeywordLists(t *testing.T) {
	input := `{
		"type": "object",
		"required": ["id", "kind"],
		"properties": {
			"kind": {"type": "string", "enum": ["dog", "cat", null]},
			"tag": {"const": "fixed"},
			"id": {"type": "integer", "examples": [1, 2]}
		}
	}`

	for format, s := range parseSchemaBothWays(t, "3.1.0", input) {
		require.Equal(t, []string{"id", "kind"}, s.Required, format)
		require.True(t, s.IsRequired("kind"), format)
		require.False(t, s.IsRequired("tag"), format)
		require.Equal(t, []string{"kind", "tag", "id"}, s.Properties.Keys, format)

		kind := s.Properties.Map["kind"]
		require.Len(t, kind.Enum, 3, format)
		require.Equal(t, "dog", kind.Enum[0], format)
		require.Nil(t, kind.Enum[2], format)

		require.Equal(t, "fixed", s.Properties.Map["tag"].Const, format)

		id := s.Properties.Map["id"]
		require.Len(t, id.Examples, 2, format)
		require.EqualValues(t, 1, id.Examples[0], format)
	}
}

func TestSchemaBooleanSchemas31(t *testing.T) {
	input := `{"type": "object", "properties": {"anything": true, "nothing": false}, "items": true}`
	for format, s := range parseSchemaBothWays(t, "3.1.0", input) {
		anything := s.Properties.Map["anything"]
		require.NotNil(t, anything, format)
		require.Empty(t, anything.Type, format)
		require.Nil(t, anything.Not, format)

		nothing := s.Properties.Map["nothing"]
		require.NotNil(t, nothing, format)
		require.NotNil(t, nothing.Not, format)

		require.NotNil(t, s.Items, format)
	}
}

func TestSchemaNormalizationIsDeep(t *testing.T) {
	input := `{
		"allOf": [{"properties": {"a": {"type": ["integer", "null"]}}}],
		"oneOf": [{"type": ["string", "null"]}],
		"anyOf": [{"type": ["boolean", "null"]}],
		"not": {"type": ["number", "null"]},
		"properties": {"list": {"type": "array", "items": {"type": ["string", "null"]}}}
	}`
	for format, s := range parseSchemaBothWays(t, "3.1.0", input) {
		require.True(t, s.AllOf[0].Properties.Map["a"].Nullable, format)
		require.Equal(t, TypeList{"integer"}, s.AllOf[0].Properties.Map["a"].Type, format)
		require.True(t, s.OneOf[0].Nullable, format)
		require.True(t, s.AnyOf[0].Nullable, format)
		require.True(t, s.Not.Nullable, format)
		require.True(t, s.Properties.Map["list"].Items.Nullable, format)
	}
}

func TestParseNormalizesOutsideComponentSchemas(t *testing.T) {
	input := `
openapi: 3.1.0
info: {title: t, version: "1"}
paths:
  /things/{id}:
    parameters:
      - {name: id, in: path, required: true, schema: {type: [string, "null"]}}
    post:
      parameters:
        - {name: q, in: query, schema: {type: [string, "null"]}}
      requestBody:
        content:
          application/json:
            schema: {type: [object, "null"]}
      responses:
        "200":
          description: ok
          headers:
            X-Rate: {schema: {type: [integer, "null"]}}
          content:
            application/json:
              schema: {type: [array, "null"], items: {type: string}}
components:
  parameters:
    P: {name: p, in: query, schema: {type: [string, "null"]}}
  headers:
    H: {schema: {type: [string, "null"]}}
  requestBodies:
    B:
      content:
        application/json:
          schema: {type: [string, "null"]}
  responses:
    R:
      description: r
      content:
        application/json:
          schema: {type: [string, "null"]}
`
	doc, err := Parse([]byte(input))
	require.NoError(t, err)

	item := doc.Paths.Map["/things/{id}"]
	require.True(t, item.Parameters[0].Schema.Nullable)
	require.True(t, item.Post.Parameters[0].Schema.Nullable)
	require.True(t, item.Post.RequestBody.Content.Map["application/json"].Schema.Nullable)
	res := item.Post.Responses.Map["200"]
	require.True(t, res.Headers.Map["X-Rate"].Schema.Nullable)
	require.True(t, res.Content.Map["application/json"].Schema.Nullable)

	require.True(t, doc.Components.Parameters.Map["P"].Schema.Nullable)
	require.True(t, doc.Components.Headers.Map["H"].Schema.Nullable)
	require.True(t, doc.Components.RequestBodies.Map["B"].Content.Map["application/json"].Schema.Nullable)
	require.True(t, doc.Components.Responses.Map["R"].Content.Map["application/json"].Schema.Nullable)
}

func TestParseToleratesNullEntries(t *testing.T) {
	inputs := map[string]string{
		"yaml": `
openapi: 3.0.3
info: {title: t, version: "1"}
paths:
  /a: ~
  /b:
    get:
      parameters: [~]
      responses:
        "200": ~
        "201":
          description: x
          headers: {X: ~}
          content: {application/json: ~}
components:
  schemas: {S: ~}
  parameters: {P: ~}
  headers: {H: ~}
  requestBodies: {B: ~}
  responses: {R: ~}
  examples: {E: ~}
`,
		"json": `{"openapi": "3.0.3", "info": {"title": "t", "version": "1"},
			"paths": {"/a": null, "/b": {"get": {"parameters": [null], "responses": {"200": null}}}},
			"components": {"schemas": {"S": null}, "parameters": {"P": null}, "headers": null,
				"requestBodies": {"B": null}, "responses": {"R": null}, "examples": {"E": null}}}`,
	}

	for format, input := range inputs {
		doc, err := Parse([]byte(input))
		require.NoError(t, err, format)
		require.Equal(t, []string{"/a", "/b"}, doc.Paths.Keys, format)
		require.Len(t, doc.Operations(), 1, format)
	}
}

func TestParseEmptyOrMissingPaths(t *testing.T) {
	inputs := map[string]string{
		"yaml empty":   "openapi: 3.0.3\ninfo: {title: t, version: \"1\"}\npaths: {}\n",
		"yaml missing": "openapi: 3.0.3\ninfo: {title: t, version: \"1\"}\n",
		"yaml null":    "openapi: 3.0.3\ninfo: {title: t, version: \"1\"}\npaths: ~\n",
		"json empty":   `{"openapi": "3.1.0", "info": {"title": "t", "version": "1"}, "paths": {}}`,
		"json missing": `{"openapi": "3.1.0", "info": {"title": "t", "version": "1"}}`,
		"json null":    `{"openapi": "3.1.0", "info": {"title": "t", "version": "1"}, "paths": null}`,
	}

	for name, input := range inputs {
		doc, err := Parse([]byte(input))
		require.NoError(t, err, name)
		require.Equal(t, 0, doc.Paths.Len(), name)
		require.Empty(t, doc.Operations(), name)
		_, ok := doc.Paths.Get("/x")
		require.False(t, ok, name)
	}
}

func TestParseUnquotedVersionAndBOM(t *testing.T) {
	doc, err := Parse([]byte("\xef\xbb\xbfopenapi: 3.0.3\ninfo: {title: t, version: 1.0}\n"))
	require.NoError(t, err)
	require.Equal(t, "3.0.3", doc.OpenAPI)
	require.Equal(t, "1.0", doc.Info.Version)

	doc, err = Parse([]byte("\xef\xbb\xbf  {\"openapi\": \"3.1.0\"}"))
	require.NoError(t, err)
	require.Equal(t, "3.1.0", doc.OpenAPI)
}

func TestParseErrors(t *testing.T) {
	cases := map[string]struct {
		input   string
		message string
	}{
		"empty":            {"", "empty OpenAPI document"},
		"whitespace":       {"  \n\t\r\n", "empty OpenAPI document"},
		"invalid yaml":     {"openapi: 3.0.3\ninfo: [unclosed\n", "invalid OpenAPI document"},
		"yaml wrong shape": {"openapi: 3.0.3\npaths: [a, b]\n", "expected a mapping"},
		"yaml scalar":      {"hello", "invalid OpenAPI document"},
		"yaml list":        {"- openapi: 3.0.3\n", "invalid OpenAPI document"},
		"invalid json":     {`{"openapi": "3.0.3",`, "invalid OpenAPI document"},
		"json wrong type":  {`{"openapi": "3.0.3", "paths": []}`, "expected a JSON object"},
		"swagger yaml":     {"swagger: \"2.0\"\ninfo: {title: t, version: \"1\"}\npaths: {}\n", "Swagger 2.0 documents are not supported"},
		"swagger json":     {`{"swagger": "2.0", "info": {"title": "t", "version": "1"}}`, "Swagger 2.0 documents are not supported"},
		"missing yaml":     {"info: {title: t, version: \"1\"}\npaths: {}\n", "missing openapi version field"},
		"missing json":     {`{"info": {"title": "t", "version": "1"}}`, "missing openapi version field"},
		"version 4":        {"openapi: 4.0.0\n", `unsupported OpenAPI version "4.0.0", expected 3.x`},
		"version 2":        {`{"openapi": "2.0"}`, `unsupported OpenAPI version "2.0", expected 3.x`},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			doc, err := Parse([]byte(tc.input))
			require.Error(t, err)
			require.Nil(t, doc)
			require.Equal(t, ez.EINVALID, ez.ErrorCode(err))
			require.Contains(t, ez.ErrorMessage(err), tc.message)
		})
	}
}

func TestParseErrorMessagesLocateTheProblem(t *testing.T) {
	_, err := Parse([]byte("{\n  \"openapi\": \"3.0.3\",\n  \"info\": {\n    \"title\": \"t\",,\n  }\n}"))
	require.Error(t, err)
	require.Contains(t, ez.ErrorMessage(err), "line 4:")

	_, err = Parse([]byte("\n\n  {\"openapi\": \"3.0.3\",, }"))
	require.Error(t, err)
	require.Contains(t, ez.ErrorMessage(err), "line 3:")

	// Leading blank lines and comments count toward YAML line numbers.
	_, err = Parse([]byte("\n\n# spec\nopenapi: 3.0.3\ninfo:\n  title: t\npaths:\n  /a:\n    get:\n      deprecated: maybe\n"))
	require.Error(t, err)
	require.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	require.Contains(t, ez.ErrorMessage(err), "line 10")
	require.NotContains(t, ez.ErrorMessage(err), "reference.")
}

func TestParsePetstore31(t *testing.T) {
	doc := loadEmma(t, "petstore-3.1.yaml")
	require.Equal(t, "3.1.0", doc.OpenAPI)
	require.Empty(t, unresolvedRefs(doc))

	pet := doc.Components.Schemas.Map["Pet"]
	require.Equal(t, []string{"id", "name", "kind"}, pet.Properties.Keys)
	require.Equal(t, []string{"id", "kind"}, pet.Required)

	id := pet.Properties.Map["id"]
	require.Equal(t, 0.0, *id.ExclusiveMinimum.Value)
	require.Equal(t, 1000000.0, *id.ExclusiveMaximum.Value)
	require.Nil(t, id.Minimum)

	name := pet.Properties.Map["name"]
	require.Equal(t, TypeList{"string"}, name.Type)
	require.True(t, name.Nullable)
	require.Equal(t, []any{"Rex", "Fido"}, name.Examples)

	require.NotNil(t, pet.Discriminator)
	require.Equal(t, "kind", pet.Discriminator.PropertyName)
	require.Equal(t, "#/components/schemas/Dog", pet.Discriminator.Mapping["dog"])
	require.Len(t, pet.OneOf, 2)

	dog := doc.Components.Schemas.Map["Dog"]
	require.Equal(t, "dog", dog.Properties.Map["kind"].Const)

	lives := doc.Components.Schemas.Map["Cat"].Properties.Map["lives"]
	require.True(t, lives.Nullable)
	require.Equal(t, 1.0, *lives.Minimum)
	require.Equal(t, 9.0, *lives.Maximum)

	item := doc.Paths.Map["/pets/{petId}"]
	require.Len(t, item.Parameters, 2)
	require.Equal(t, "#/components/parameters/PetId", item.Parameters[0].Ref)

	petID := doc.Parameter(item.Parameters[0])
	require.Equal(t, "petId", petID.Name)
	require.Equal(t, 0.0, *petID.Schema.ExclusiveMinimum.Value)

	fields := item.Get.Parameters[0]
	require.NotNil(t, fields.Explode)
	require.False(t, *fields.Explode)
	require.Nil(t, item.Parameters[1].Explode)
	require.True(t, fields.Schema.Nullable)
	require.Equal(t, TypeList{"array"}, fields.Schema.Type)

	// 200 is an unquoted YAML integer key, "404" a quoted one.
	require.Equal(t, []string{"200", "404"}, item.Get.Responses.Keys)
	notFound := doc.Response(item.Get.Responses.Map["404"])
	require.Equal(t, "Not found", notFound.Description)
}

func TestParseCompose31Golden(t *testing.T) {
	doc := loadEmma(t, "compose-3.1.json")
	require.Equal(t, "3.1.0", doc.OpenAPI)
	require.Empty(t, unresolvedRefs(doc))
	require.Equal(t, []string{"/health", "/legacy/{id}", "/runs", "/runs/{id}"}, doc.Paths.Keys)

	create := doc.Components.Schemas.Map["fixture.CreateRequest"]
	require.Equal(t, []string{"config", "labels", "name", "steps"}, create.Properties.Keys)

	steps := create.Properties.Map["steps"]
	require.Equal(t, TypeList{"array"}, steps.Type)
	require.True(t, steps.Nullable)

	// anyOf: [{$ref: Config}, {type: "null"}] reads as a nullable Config.
	config := create.Properties.Map["config"]
	require.Empty(t, config.AnyOf)
	require.True(t, config.Nullable)
	require.True(t, config.NullMember)
	require.Equal(t, "#/components/schemas/fixture.Config", config.Ref)
	_, configName := doc.Schema(config)
	require.Equal(t, "fixture.Config", configName)

	// A member with siblings next to its $ref moves into allOf instead.
	status := doc.Components.Schemas.Map["fixture.UpdateRequest"].Properties.Map["status"]
	require.Empty(t, status.AnyOf)
	require.True(t, status.Nullable)
	require.Len(t, status.AllOf, 1)
	require.Equal(t, []any{"failed", "cancelled"}, status.AllOf[0].Enum)
	require.Equal(t, "fixture.RunStatus", doc.SchemaName(status))

	labels := create.Properties.Map["labels"]
	resolved, name := doc.Schema(labels.AdditionalSchema())
	require.Equal(t, "fixture.Config", name)
	require.Equal(t, "object", resolved.Type.Primary())
}

func TestSchemaNullMembersOfOneOfAndAnyOf(t *testing.T) {
	doc := parseSpec(t, `
openapi: 3.1.0
info: {title: t, version: "1"}
paths: {}
components:
  schemas:
    Pet: {type: object}
    Holder:
      type: object
      properties:
        ref:
          anyOf: [{$ref: '#/components/schemas/Pet'}, {type: "null"}]
        oneRef:
          oneOf: [{type: "null"}, {$ref: '#/components/schemas/Pet'}]
          description: kept on the wrapper
        inline:
          oneOf: [{type: string, format: uuid}, {type: "null"}]
        many:
          anyOf: [{type: string}, {type: integer}, {type: "null"}]
        typed:
          type: object
          anyOf: [{required: [a]}, {type: "null"}]
        described:
          anyOf: [{$ref: '#/components/schemas/Pet'}, {type: "null", description: not bare}]
        onlyNull:
          oneOf: [{type: "null"}]
        nested:
          anyOf: [{type: string}, {anyOf: [{type: "null"}]}]
`)
	props := doc.Components.Schemas.Map["Holder"].Properties.Map

	ref := props["ref"]
	require.True(t, ref.Nullable)
	require.Empty(t, ref.AnyOf)
	require.Equal(t, "#/components/schemas/Pet", ref.Ref)

	oneRef := props["oneRef"]
	require.True(t, oneRef.Nullable)
	require.Empty(t, oneRef.OneOf)
	require.Equal(t, "#/components/schemas/Pet", oneRef.Ref)
	require.Equal(t, "kept on the wrapper", oneRef.Description)

	inline := props["inline"]
	require.True(t, inline.Nullable)
	require.Empty(t, inline.OneOf)
	require.Empty(t, inline.Ref)
	require.Len(t, inline.AllOf, 1)
	require.Equal(t, "uuid", doc.Flatten(inline).Format)

	many := props["many"]
	require.True(t, many.Nullable)
	require.Len(t, many.AnyOf, 2)

	// A parent with its own type keeps the remaining member where it was.
	typed := props["typed"]
	require.True(t, typed.Nullable)
	require.Len(t, typed.AnyOf, 1)
	require.Empty(t, typed.AllOf)

	// A null member with other keywords is not exactly null.
	require.Len(t, props["described"].AnyOf, 2)
	require.False(t, props["described"].Nullable)

	onlyNull := props["onlyNull"]
	require.True(t, onlyNull.Nullable)
	require.Empty(t, onlyNull.OneOf)

	// A member that is itself an anyOf of null is still only null.
	nested := props["nested"]
	require.True(t, nested.Nullable)
	require.Empty(t, nested.AnyOf)
	require.Len(t, nested.AllOf, 1)
	require.Equal(t, "string", doc.Flatten(nested).Type.Primary())
}

// aliasBombSpec is a billion-laughs document made of schemas: A0 has nine
// string properties and every next level holds nine aliases of the one
// before, so An expands to 9^(n+1) schemas. Ordered and Schema decode each
// property with its own Decode call, which never expands more than one
// level, so yaml.v3's per-call ratio check lets it through.
func aliasBombSpec(levels int) string {
	spec := "openapi: 3.1.0\ninfo: {title: Bomb, version: \"1\"}\npaths: {}\ncomponents:\n  schemas:\n"
	spec += "    A0: &a0 {type: object, properties: {p0: {type: string}, p1: {type: string}, p2: {type: string}, p3: {type: string}, p4: {type: string}, p5: {type: string}, p6: {type: string}, p7: {type: string}, p8: {type: string}}}\n"
	for i := 1; i < levels; i++ {
		var props []string
		for p := 0; p < 9; p++ {
			props = append(props, fmt.Sprintf("p%d: *a%d", p, i-1))
		}
		spec += fmt.Sprintf("    A%d: &a%d {type: object, properties: {%s}}\n", i, i, strings.Join(props, ", "))
	}
	return spec
}

// TestParseRejectsYAMLAliasBomb covers a document whose aliases expand to
// 9^9 schemas: it fails fast with EINVALID instead of decoding them all.
func TestParseRejectsYAMLAliasBomb(t *testing.T) {
	spec := aliasBombSpec(9)
	require.Less(t, len(spec), 2000)

	start := time.Now()
	_, err := Parse([]byte(spec))
	require.Error(t, err)
	require.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	require.Contains(t, err.Error(), "YAML alias expansion too large")
	require.Less(t, time.Since(start), 5*time.Second)

	// An alias inside its own anchor never ends either.
	_, err = Parse([]byte("openapi: 3.1.0\ninfo: {title: Loop, version: \"1\"}\npaths: {}\nx-loop: &loop [*loop]\n"))
	require.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	require.Contains(t, err.Error(), "YAML alias expansion too large")

	// Ordinary anchors and merge keys still decode.
	doc, err := Parse([]byte(`
openapi: 3.1.0
info: {title: Anchors, version: "1"}
paths: {}
components:
  schemas:
    Base: &base
      type: object
      properties:
        id: {type: string}
    Copy: *base
    Extended:
      <<: *base
      description: extended
`))
	require.NoError(t, err)
	require.Equal(t, []string{"id"}, doc.Components.Schemas.Map["Copy"].Properties.Keys)
	require.Equal(t, "extended", doc.Components.Schemas.Map["Extended"].Description)
}

// TestParseDropsNilMembers covers null list items under allOf, oneOf and
// anyOf, in YAML and JSON: they are dropped, so a nullable idiom with a
// null member next to the real one still reads as that member.
func TestParseDropsNilMembers(t *testing.T) {
	yamlSpec := `
openapi: 3.1.0
info: {title: Nil, version: "1"}
paths: {}
components:
  schemas:
    Pet:
      type: object
      properties:
        name: {type: string}
    Holder:
      type: object
      properties:
        one:
          oneOf: [null, {$ref: '#/components/schemas/Pet'}, {type: "null"}]
        any:
          anyOf: [~, {type: string}, {type: integer}]
        all:
          allOf: [null]
        onlyNull:
          oneOf: [null, {type: "null"}]
`
	jsonSpec := `{
  "openapi": "3.1.0",
  "info": {"title": "Nil", "version": "1"},
  "paths": {},
  "components": {"schemas": {
    "Pet": {"type": "object", "properties": {"name": {"type": "string"}}},
    "Holder": {"type": "object", "properties": {
      "one": {"oneOf": [null, {"$ref": "#/components/schemas/Pet"}, {"type": "null"}]},
      "any": {"anyOf": [null, {"type": "string"}, {"type": "integer"}]},
      "all": {"allOf": [null]},
      "onlyNull": {"oneOf": [null, {"type": "null"}]}
    }}
  }}
}`
	for name, spec := range map[string]string{"yaml": yamlSpec, "json": jsonSpec} {
		t.Run(name, func(t *testing.T) {
			doc, err := Parse([]byte(spec))
			require.NoError(t, err)

			props := doc.Components.Schemas.Map["Holder"].Properties.Map
			require.Equal(t, "#/components/schemas/Pet", props["one"].Ref)
			require.True(t, props["one"].Nullable)
			require.Empty(t, props["one"].OneOf)
			require.Len(t, props["any"].AnyOf, 2)
			require.NotContains(t, props["any"].AnyOf, (*Schema)(nil))
			require.Empty(t, props["all"].AllOf)
			require.True(t, props["onlyNull"].Nullable)
			require.Empty(t, props["onlyNull"].OneOf)
			require.Empty(t, props["onlyNull"].AllOf)

			example := decode(t, doc.ExampleJSON(schemaRef("Holder"), ForResponse))
			require.Equal(t, map[string]any{"name": "string"}, example["one"])
			require.Equal(t, "string", example["any"])

			html := renderPage(t, doc)
			require.Contains(t, html, `id="model/Holder"`)
		})
	}
}

// TestSchemaUnmodelled checks that Parse keeps in Unmodelled, sorted, the
// schema keywords no field holds, in JSON and YAML alike, as compact JSON
// with object keys sorted and numbers written one way, and leaves out the
// keywords a field holds, the annotations and the extensions.
func TestSchemaUnmodelled(t *testing.T) {
	const schema = `{"type": "object", "title": "t", "x-a": 1, "$comment": "c", "$id": "i", "$schema": "s", "$anchor": "a", "externalDocs": {"url": "u"}, "xml": {"name": "n"},` +
		` "patternProperties": {"^b": {"type": "string", "minLength": 1.0}, "^a": {"maxLength": 1e1}}, "if": {"$ref": "#/components/schemas/S"}, "$defs": {}, "dependentRequired": {"a": ["b"]}}`
	want := []RawKeyword{
		{Name: "$defs", Value: `{}`},
		{Name: "dependentRequired", Value: `{"a":["b"]}`},
		{Name: "if", Value: `{"$ref":"#/components/schemas/S"}`},
		{Name: "patternProperties", Value: `{"^a":{"maxLength":10},"^b":{"minLength":1,"type":"string"}}`},
	}
	for format, s := range parseSchemaBothWays(t, "3.1.0", schema) {
		require.Equal(t, want, s.Unmodelled, format)
	}
	for format, s := range parseSchemaBothWays(t, "3.0.3", `{"type": "string", "properties": {"p": {"items": {"contentEncoding": "base64"}}}}`) {
		require.Nil(t, s.Unmodelled, format)
		require.Equal(t, []RawKeyword{{Name: "contentEncoding", Value: `"base64"`}}, s.Properties.Map["p"].Items.Unmodelled, format)
	}
	for format, s := range parseSchemaBothWays(t, "3.1.0", `{"type": "string", "maxLength": 3, "x-b": {"if": 1}}`) {
		require.Nil(t, s.Unmodelled, format)
	}
	// encoding/json matches a key to a field regardless of case, YAML does
	// not.
	schemas := parseSchemaBothWays(t, "3.1.0", `{"maxlength": 3}`)
	require.Equal(t, 3, *schemas["json"].MaxLength)
	require.Nil(t, schemas["json"].Unmodelled)
	require.Nil(t, schemas["yaml"].MaxLength)
	require.Equal(t, []RawKeyword{{Name: "maxlength", Value: "3"}}, schemas["yaml"].Unmodelled)

	// A YAML merge key brings keywords in, a keyword written in the mapping
	// wins over a merged one, and a value JSON cannot hold is a string.
	doc := parseYAML(t, `
openapi: 3.1.0
info: {title: t, version: "1"}
components:
  schemas:
    S:
      <<: &base {if: {const: 1}, then: {const: 2}}
      then: {const: 3}
      else: .nan
    T: *base
    U:
      items: *base
`)
	require.Equal(t, []RawKeyword{{Name: "else", Value: `"NaN"`}, {Name: "if", Value: `{"const":1}`}, {Name: "then", Value: `{"const":3}`}}, doc.Components.Schemas.Map["S"].Unmodelled)
	require.Equal(t, []RawKeyword{{Name: "if", Value: `{"const":1}`}, {Name: "then", Value: `{"const":2}`}}, doc.Components.Schemas.Map["T"].Unmodelled)
	require.Equal(t, []RawKeyword{{Name: "if", Value: `{"const":1}`}, {Name: "then", Value: `{"const":2}`}}, doc.Components.Schemas.Map["U"].Items.Unmodelled)
}

// TestSchemaUnmodelledYAMLAliases checks that Parse reads the keywords
// Unmodelled keeps from their own value nodes, never decoding a schema
// mapping whole: a large anchor merged into a schema or used under
// properties beside such a keyword, which yaml.v3 refuses to decode in one
// mapping as excessive aliasing, parses, as do a deeply nested schema and a
// long chain of anchors with such a keyword at every level.
func TestSchemaUnmodelledYAMLAliases(t *testing.T) {
	const head = "openapi: 3.1.0\ninfo: {title: t, version: \"1\"}\ncomponents:\n  schemas:\n"
	var pet strings.Builder
	pet.WriteString("    Pet: &pet\n      type: object\n      properties:\n")
	for i := range 300 {
		fmt.Fprintf(&pet, "        p%d: {type: string, maxLength: 10}\n", i)
	}
	for _, c := range []struct {
		schema string
		want   []RawKeyword
	}{
		{"    S: {<<: *pet, description: a cat}\n", nil},
		{"    S: {type: object, unevaluatedProperties: false, properties: {data: *pet}}\n", []RawKeyword{{Name: "unevaluatedProperties", Value: "false"}}},
		{"    S: {type: object, additionalProperty: false, properties: {data: *pet}}\n", []RawKeyword{{Name: "additionalProperty", Value: "false"}}},
	} {
		doc, err := Parse([]byte(head + pet.String() + c.schema))
		require.NoError(t, err, c.schema)
		require.Equal(t, c.want, doc.Components.Schemas.Map["S"].Unmodelled, c.schema)
	}

	const depth = 2000
	deep := head + "    Deep: " + strings.Repeat("{if: {const: 1}, properties: {a: ", depth) + "{}" + strings.Repeat("}}", depth) + "\n"
	doc, err := Parse([]byte(deep))
	require.NoError(t, err)
	s := doc.Components.Schemas.Map["Deep"]
	for range depth {
		require.Equal(t, []RawKeyword{{Name: "if", Value: `{"const":1}`}}, s.Unmodelled)
		s = s.Properties.Map["a"]
	}

	var chain strings.Builder
	chain.WriteString(head + "    S0: &a0 {type: object, if: {const: 1}}\n")
	for i := 1; i < 300; i++ {
		fmt.Fprintf(&chain, "    S%d: &a%d {type: object, if: {const: 1}, properties: {a: *a%d}}\n", i, i, i-1)
	}
	doc, err = Parse([]byte(chain.String()))
	require.NoError(t, err)
	require.Equal(t, []RawKeyword{{Name: "if", Value: `{"const":1}`}}, doc.Components.Schemas.Map["S299"].Properties.Map["a"].Unmodelled)
}

// TestSchemaUnmodelledYAMLMerge checks that keywords a YAML merge key
// brings in are kept where the schema has none of its own, and that, as
// YAML merge defines, the mapping's own keys win over merged ones, an
// earlier merged mapping over a later one, and a merged mapping's own keys
// over those of its merge keys.
func TestSchemaUnmodelledYAMLMerge(t *testing.T) {
	doc := parseYAML(t, `
openapi: 3.1.0
info: {title: t, version: "1"}
components:
  schemas:
    A: &a {if: {const: 1}, then: {const: 1}}
    B: &b {if: {const: 2}, else: {const: 2}, <<: {contains: {const: 2}, else: {const: 4}}}
    S: {type: object, <<: *a}
    T: {<<: [*a, *b]}
    U: {<<: [*b, *a], else: {const: 3}}
`)
	schemas := doc.Components.Schemas.Map
	require.Equal(t, []RawKeyword{{Name: "if", Value: `{"const":1}`}, {Name: "then", Value: `{"const":1}`}}, schemas["S"].Unmodelled)
	require.Equal(t, []RawKeyword{{Name: "contains", Value: `{"const":2}`}, {Name: "else", Value: `{"const":2}`}, {Name: "if", Value: `{"const":1}`}, {Name: "then", Value: `{"const":1}`}}, schemas["T"].Unmodelled)
	require.Equal(t, []RawKeyword{{Name: "contains", Value: `{"const":2}`}, {Name: "else", Value: `{"const":3}`}, {Name: "if", Value: `{"const":2}`}, {Name: "then", Value: `{"const":1}`}}, schemas["U"].Unmodelled)
}

// TestSchemaUnmodelledNumbers checks how Parse writes a number inside a
// value Unmodelled keeps: as its float64 where it has at most 15
// significant digits in the normal float64 range, and as written
// otherwise, never failing on one. yaml.v3 reads 1e400, which overflows a
// float64, as a string.
func TestSchemaUnmodelledNumbers(t *testing.T) {
	for _, c := range []struct{ number, want string }{
		{"1", "1"},
		{"1.0", "1"},
		{"-0", "0"},
		{"-0.0", "0"},
		{"1e2", "100"},
		{"-0.5", "-0.5"},
		{"1e21", "1e+21"},
		{"0e999", "0"},
		{"123456789012345", "123456789012345"},
		{"9007199254740993", "9007199254740993"},
		{"9007199254740992", "9007199254740992"},
		{"0.30000000000000001", "0.30000000000000001"},
		{"1e400", "1e400"},
		{"1e-400", "1e-400"},
	} {
		for format, s := range parseSchemaBothWays(t, "3.1.0", `{"if": {"maximum": `+c.number+`}}`) {
			want := c.want
			if format == "yaml" && c.number == "1e400" {
				want = `"1e400"`
			}
			require.Equal(t, []RawKeyword{{Name: "if", Value: `{"maximum":` + want + `}`}}, s.Unmodelled, "%s %s", format, c.number)
		}
	}
}

// TestSchemaUnmodelledYAMLKeys checks that a YAML value whose mapping keys
// yaml.v3 would decode to the same key, or refuses as given twice, parses
// and is written the same on every parse.
func TestSchemaUnmodelledYAMLKeys(t *testing.T) {
	for _, c := range []struct{ value, want string }{
		{`{1: a, 1.0: b}`, `{"1":"a","1.0":"b"}`},
		{`{1: a, "1": b}`, `{"1":"a","1":"b"}`},
		{`{"1": b, 1: a}`, `{"1":"b","1":"a"}`},
	} {
		for range 20 {
			doc := parseYAML(t, "openapi: 3.1.0\ninfo: {title: t, version: \"1\"}\ncomponents:\n  schemas:\n    S: {if: "+c.value+"}\n")
			require.Equal(t, []RawKeyword{{Name: "if", Value: c.want}}, doc.Components.Schemas.Map["S"].Unmodelled, c.value)
		}
	}
}

// TestParseRejectsYAMLAliasText checks that Parse refuses a YAML document
// whose aliases repeat more scalar text than the budget allows, though it
// has few nodes, and still parses one whose aliases repeat modest text.
func TestParseRejectsYAMLAliasText(t *testing.T) {
	doc := func(size, count int) string {
		return "openapi: 3.1.0\ninfo: {title: t, version: \"1\"}\ncomponents:\n  schemas:\n" +
			"    Big: {type: string, description: &s \"" + strings.Repeat("a", size) + "\"}\n" +
			"    S: {type: object, if: [" + strings.TrimSuffix(strings.Repeat("*s,", count), ",") + "]}\n"
	}
	_, err := Parse([]byte(doc(1<<20, 100)))
	require.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	require.Contains(t, err.Error(), "YAML alias expansion too large")

	parsed, err := Parse([]byte(doc(1<<10, 1000)))
	require.NoError(t, err)
	require.Len(t, parsed.Components.Schemas.Map["S"].Unmodelled[0].Value, 1000*(1<<10+3)+1)

	// A control character is written as six bytes, \u0001, and counted so.
	spec := "openapi: 3.1.0\ninfo: {title: t, version: \"1\"}\ncomponents:\n  schemas:\n" +
		"    Big: {description: &s \"" + strings.Repeat(`\x01`, 1000) + "\"}\n" +
		"    S: {type: object, if: [" + strings.TrimSuffix(strings.Repeat("*s,", 66000), ",") + "]}\n"
	require.Less(t, len(spec), 210_000)
	_, err = Parse([]byte(spec))
	require.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	require.Contains(t, err.Error(), "YAML alias expansion too large")
}

// TestSchemaUnmodelledYAMLBinary checks that a !!binary value is kept as
// its text, so two values whose bytes are not UTF-8 differ.
func TestSchemaUnmodelledYAMLBinary(t *testing.T) {
	doc := parseYAML(t, "openapi: 3.1.0\ninfo: {title: t, version: \"1\"}\ncomponents:\n  schemas:\n    A: {if: !!binary gIGC}\n    B: {if: !!binary g4SF}\n")
	require.Equal(t, []RawKeyword{{Name: "if", Value: `"gIGC"`}}, doc.Components.Schemas.Map["A"].Unmodelled)
	require.Equal(t, []RawKeyword{{Name: "if", Value: `"g4SF"`}}, doc.Components.Schemas.Map["B"].Unmodelled)
}

// TestSchemaUnmodelledYAMLKeyNotScalar checks that a mapping key that is a
// mapping, which JSON cannot hold, inside a value Unmodelled keeps is an
// error naming the keyword, nested or as an alias inside its own anchor,
// while a key that is an alias of a scalar is read as that scalar.
func TestSchemaUnmodelledYAMLKeyNotScalar(t *testing.T) {
	key := "{a: 1}"
	for range 20 {
		key = "{? " + key + " : 1}"
	}
	_, err := Parse([]byte("openapi: 3.1.0\ninfo: {title: t, version: \"1\"}\ncomponents:\n  schemas:\n    S: {type: object, if: " + key + "}\n"))
	require.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	require.Contains(t, err.Error(), "schema keyword if has a key that is not a scalar")

	var s Schema
	err = yaml.Unmarshal([]byte("type: object\nif: &a {? *a : 1}\n"), &s)
	require.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	require.Contains(t, err.Error(), "schema keyword if has a key that is not a scalar")

	doc := parseYAML(t, "openapi: 3.1.0\ninfo: {title: t, version: \"1\"}\nx-key: &k b\ncomponents:\n  schemas:\n    S: {if: {*k : 1, a: *k}}\n")
	require.Equal(t, []RawKeyword{{Name: "if", Value: `{"a":"b","b":1}`}}, doc.Components.Schemas.Map["S"].Unmodelled)
}

// TestSchemaUnmodelledYAMLAliasCycle checks that a value holding an alias
// inside its own anchor is an error where a Schema is decoded without Parse,
// which refuses it first.
func TestSchemaUnmodelledYAMLAliasCycle(t *testing.T) {
	var s Schema
	err := yaml.Unmarshal([]byte("type: object\nif: &a [*a]\n"), &s)
	require.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	require.Contains(t, err.Error(), `anchor "a" contains itself`)
}

// TestSchemaUnmodelledYAMLAliasText checks that a value whose aliases fan
// out, ten times per level, is an error once it is written longer than
// maxYAMLAliasText where a Schema is decoded without Parse, which yaml.v3's
// own alias check never sees, stopping the expansion before it allocates
// more than a small multiple of the limit, while a value under the limit is
// read as before.
func TestSchemaUnmodelledYAMLAliasText(t *testing.T) {
	fanOut := func(levels int) []byte {
		doc := "type: object\n$comment:\n  - &l0 [1, 1, 1, 1, 1, 1, 1, 1, 1, 1]\n"
		for i := 1; i <= levels; i++ {
			doc += fmt.Sprintf("  - &l%d [%s]\n", i, strings.TrimSuffix(strings.Repeat(fmt.Sprintf("*l%d, ", i-1), 10), ", "))
		}
		return []byte(doc + fmt.Sprintf("if: *l%d\n", levels))
	}
	var want Schema
	err := yaml.Unmarshal(fanOut(2), &want)
	require.NoError(t, err)
	require.Len(t, want.Unmodelled[0].Value, 2221)

	previous := maxYAMLAliasText
	maxYAMLAliasText = 4 << 10
	t.Cleanup(func() {
		maxYAMLAliasText = previous
	})

	var got Schema
	err = yaml.Unmarshal(fanOut(2), &got)
	require.NoError(t, err)
	require.Equal(t, want, got)

	bomb := fanOut(7)
	require.Less(t, len(bomb), 500)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	var s Schema
	err = yaml.Unmarshal(bomb, &s)
	runtime.ReadMemStats(&after)
	require.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	require.Contains(t, err.Error(), "schema keyword if is longer than 4096 bytes written as JSON with its YAML aliases expanded")
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(64*maxYAMLAliasText))
}

// TestSchemaUnmodelledYAMLAliasKeys checks that a schema key written as an
// alias is read as the key its anchor holds, as yaml.v3 reads it into a
// field, in the schema and in a merged mapping.
func TestSchemaUnmodelledYAMLAliasKeys(t *testing.T) {
	doc := parseYAML(t, `
openapi: 3.1.0
info: {title: t, version: "1"}
x-keys: [&t type, &m maxLength, &p patternProperties]
components:
  schemas:
    S: {*t : string, *m : 3}
    T: {type: object, *p : {a: 2}}
    U: {type: object, <<: {*p : {a: 3}}}
`)
	schemas := doc.Components.Schemas.Map
	require.Nil(t, schemas["S"].Unmodelled)
	require.Equal(t, 3, *schemas["S"].MaxLength)
	require.Equal(t, []RawKeyword{{Name: "patternProperties", Value: `{"a":2}`}}, schemas["T"].Unmodelled)
	require.Equal(t, []RawKeyword{{Name: "patternProperties", Value: `{"a":3}`}}, schemas["U"].Unmodelled)
}
