package spectest_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/vanclief/compose/components/rest/spectest"
)

// spec30 is an OpenAPI 3.0 document in the shapes the generator writes.
const spec30 = `{
  "openapi": "3.0.3",
  "info": {"title": "t", "version": "1"},
  "paths": {},
  "components": {"schemas": {
    "Tags": {"type": "object", "properties": {"tags": {"type": "array", "items": {"type": "string"}}}},
    "NullableTags": {"type": "object", "properties": {"tags": {"type": "array", "items": {"type": "string"}, "nullable": true}}},
    "Account": {
      "type": "object",
      "additionalProperties": false,
      "required": ["id", "owner", "password", "status"],
      "properties": {
        "id": {"type": "integer", "format": "int64", "readOnly": true},
        "owner": {"$ref": "#/components/schemas/Owner"},
        "manager": {"allOf": [{"$ref": "#/components/schemas/Owner"}], "type": "object", "nullable": true},
        "password": {"type": "string", "writeOnly": true},
        "status": {"type": "string", "enum": ["active", "closed"]},
        "created_at": {"type": "string", "format": "date-time"},
        "score": {"type": "number", "minimum": 0, "exclusiveMinimum": true}
      }
    },
    "Owner": {"type": "object", "required": ["name", "address"], "properties": {
      "name": {"type": "string", "minLength": 1},
      "address": {"$ref": "#/components/schemas/Address"}
    }},
    "Address": {"type": "object", "required": ["city"], "properties": {"city": {"type": "string"}}},
    "Node": {"type": "object", "required": ["name", "children"], "properties": {
      "name": {"type": "string"},
      "children": {"type": "array", "items": {"$ref": "#/components/schemas/Node"}}
    }}
  }}
}`

type tags struct {
	Tags []string `json:"tags"`
}

type address struct {
	City string `json:"city"`
}

type owner struct {
	Name    string   `json:"name"`
	Address *address `json:"address,omitempty"`
}

type account struct {
	ID        int64   `json:"id"`
	Owner     owner   `json:"owner"`
	Manager   *owner  `json:"manager"`
	Password  string  `json:"password,omitempty"`
	Status    string  `json:"status"`
	CreatedAt string  `json:"created_at,omitempty"`
	Score     float64 `json:"score,omitempty"`
	Secret    string  `json:"secret,omitempty"`
}

type node struct {
	Name     string `json:"name"`
	Children []node `json:"children"`
}

func validAccount() account {
	return account{ID: 1, Owner: owner{Name: "ana", Address: &address{City: "Monterrey"}}, Status: "active"}
}

// schemaErrors runs Schema with a fake TB and returns what it reported.
func schemaErrors(t *testing.T, spec string, component string, value any) []string {
	t.Helper()

	fake := &fakeTB{TB: t}
	spectest.Schema(fake, []byte(spec), component, value)
	return fake.errors
}

func TestSchemaNilSlice(t *testing.T) {
	// encoding/json writes a nil slice as null.
	errs := schemaErrors(t, spec30, "Tags", tags{})
	require.Len(t, errs, 1)
	require.Equal(t, `spectest: the value does not match component Tags:
  at "/tags": got null, want array`, errs[0])

	spectest.Schema(t, []byte(spec30), "Tags", tags{Tags: []string{}})
	spectest.Schema(t, []byte(spec30), "NullableTags", tags{})
	spectest.Schema(t, []byte(spec30), "NullableTags", tags{Tags: []string{"a"}})

	errs = schemaErrors(t, spec30, "NullableTags", map[string]any{"tags": []int{1}})
	require.Len(t, errs, 1)
	require.Contains(t, errs[0], `at "/tags/0": got number, want string`)
}

func TestSchemaResponseDirection(t *testing.T) {
	// readOnly id is allowed, writeOnly password is required yet absent,
	// and a nullable wrapper of a $ref admits null.
	spectest.Schema(t, []byte(spec30), "Account", validAccount())

	withManager := validAccount()
	withManager.Manager = &owner{Name: "bo", Address: &address{City: "Puebla"}}
	spectest.Schema(t, []byte(spec30), "Account", withManager)

	leaked := validAccount()
	leaked.Password = "hunter2"
	errs := schemaErrors(t, spec30, "Account", leaked)
	require.Len(t, errs, 1)
	require.Contains(t, errs[0], `at "/password": must be absent`)
}

func TestSchemaFailures(t *testing.T) {
	value := validAccount()
	value.Owner.Address = nil
	value.Status = "frozen"
	value.Secret = "x"
	value.CreatedAt = "yesterday"
	value.Manager = &owner{Address: &address{}}

	errs := schemaErrors(t, spec30, "Account", value)
	require.Len(t, errs, 1)
	require.Contains(t, errs[0], "spectest: the value does not match component Account:")
	// additionalProperties: false
	require.Contains(t, errs[0], `at "/secret": the document does not declare this key`)
	// an omitted required field through a $ref
	require.Contains(t, errs[0], `at "/owner": missing property 'address'`)
	// enum and format
	require.Contains(t, errs[0], `at "/status": value must be one of 'active', 'closed'`)
	require.Contains(t, errs[0], `at "/created_at": 'yesterday' is not valid date-time`)
	// a nullable wrapper still checks a value that is not null, through a $ref chain
	require.Contains(t, errs[0], `at "/manager/name": minLength: got 0, want 1`)
	require.NotContains(t, errs[0], "want null")
}

func TestSchemaExclusiveBound30(t *testing.T) {
	value := validAccount()
	spectest.Schema(t, []byte(spec30), "Account", map[string]any{"id": 1, "owner": value.Owner, "status": "active", "score": 0.5})

	errs := schemaErrors(t, spec30, "Account", map[string]any{"id": 1, "owner": value.Owner, "status": "active", "score": 0})
	require.Len(t, errs, 1)
	require.Contains(t, errs[0], `at "/score": exclusiveMinimum: got 0, want 0`)
}

func TestSchemaRecursion(t *testing.T) {
	tree := node{Name: "root", Children: []node{{Name: "a", Children: []node{}}, {Name: "b", Children: []node{{Name: "c", Children: []node{}}}}}}
	spectest.Schema(t, []byte(spec30), "Node", tree)

	tree.Children[1].Children[0].Children = nil
	errs := schemaErrors(t, spec30, "Node", tree)
	require.Len(t, errs, 1)
	require.Contains(t, errs[0], `at "/children/1/children/0/children": got null, want array`)
}

func TestSchemaRawJSON(t *testing.T) {
	body := `{"tags": ["a", "b"]}`
	spectest.Schema(t, []byte(spec30), "Tags", json.RawMessage(body))
	spectest.Schema(t, []byte(spec30), "Tags", []byte(body))

	errs := schemaErrors(t, spec30, "Tags", json.RawMessage(`{"tags": ["a"], "tags": null}`))
	require.Len(t, errs, 1)
	require.Contains(t, errs[0], "spectest: Tags: body repeats an object key")

	errs = schemaErrors(t, spec30, "Tags", []byte(`{"tags": [`))
	require.Len(t, errs, 1)
	require.Contains(t, errs[0], "spectest: Tags: body is not valid JSON")

	errs = schemaErrors(t, spec30, "Tags", json.RawMessage(" "))
	require.Len(t, errs, 1)
	require.Contains(t, errs[0], "spectest: Tags: the value is empty, not JSON")
}

func TestSchemaUnknownComponent(t *testing.T) {
	errs := schemaErrors(t, spec30, "Widget", tags{})
	require.Equal(t, []string{"spectest: Widget: no such component in #/components/schemas, which holds 6 schemas"}, errs)
}

func TestSchemaBadDocument(t *testing.T) {
	for _, spec := range []string{`{"swagger": "2.0"}`, `{"openapi": null}`} {
		errs := schemaErrors(t, spec, "Tags", tags{})
		require.Equal(t, []string{"spectest: Tags: the document is not OpenAPI 3.x: it has no openapi field, or the field is null"}, errs, spec)
	}
	errs := schemaErrors(t, `{"openapi": "2.0"}`, "Tags", tags{})
	require.Equal(t, []string{`spectest: Tags: the document is not OpenAPI 3.x: its openapi field is "2.0"`}, errs)

	errs = schemaErrors(t, "openapi: [", "Tags", tags{})
	require.Len(t, errs, 1)
	require.Contains(t, errs[0], "the document is neither JSON nor valid YAML")

	dangling := `{"openapi": "3.1.0", "components": {"schemas": {"A": {"$ref": "#/components/schemas/B"}}}}`
	errs = schemaErrors(t, dangling, "A", tags{})
	require.Len(t, errs, 1)
	require.Contains(t, errs[0], "spectest: A: could not compile the component")
}

const spec31 = `openapi: 3.1.0
info:
  title: t
  version: "1"
paths:
  /tags:
    get:
      responses:
        200:
          description: ok
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/Tags'
components:
  schemas:
    Tags:
      type: object
      properties:
        tags:
          type: array
          items:
            type: string
    NullableTags:
      type: object
      properties:
        tags:
          type: [array, "null"]
          items:
            type: string
    Holder:
      type: object
      required: [owner]
      properties:
        owner:
          anyOf:
            - $ref: '#/components/schemas/Owner'
            - type: "null"
        password:
          type: string
          writeOnly: true
    Owner:
      type: object
      required: [name]
      properties:
        name:
          type: string
`

func TestSchemaYAML31(t *testing.T) {
	errs := schemaErrors(t, spec31, "Tags", tags{})
	require.Len(t, errs, 1)
	require.Contains(t, errs[0], `at "/tags": got null, want array`)

	spectest.Schema(t, []byte(spec31), "NullableTags", tags{})
	spectest.Schema(t, []byte(spec31), "Holder", map[string]any{"owner": nil})
	spectest.Schema(t, []byte(spec31), "Holder", map[string]any{"owner": map[string]any{"name": "ana"}})

	errs = schemaErrors(t, spec31, "Holder", map[string]any{"owner": map[string]any{}, "password": "x"})
	require.Len(t, errs, 1)
	require.Contains(t, errs[0], `at "/owner": missing property 'name'`)
	require.Contains(t, errs[0], `at "/password": must be absent`)
}

// closedSpec has struct components as the generator writes them: open, with
// few fields required.
const closedSpec = `{
  "openapi": "3.0.3",
  "info": {"title": "t", "version": "1"},
  "paths": {},
  "components": {"schemas": {
    "Note": {"type": "object", "required": ["id"], "properties": {
      "id": {"type": "integer"},
      "note": {"type": "string"},
      "owner": {"$ref": "#/components/schemas/Cat"},
      "labels": {"type": "object", "additionalProperties": {"type": "string"}},
      "pets": {"type": "object", "additionalProperties": {"$ref": "#/components/schemas/Cat"}},
      "raw": {},
      "data": {"type": "object"},
      "parent": {"allOf": [{"$ref": "#/components/schemas/Note"}], "type": "object", "nullable": true},
      "children": {"type": "array", "items": {"$ref": "#/components/schemas/Note"}},
      "pet": {"anyOf": [{"$ref": "#/components/schemas/Cat"}, {"$ref": "#/components/schemas/Dog"}]}
    }},
    "AdminNote": {"allOf": [{"$ref": "#/components/schemas/Note"}, {"type": "object", "properties": {"role": {"type": "string"}}}]},
    "Cat": {"type": "object", "properties": {"meow": {"type": "string"}}},
    "Dog": {"type": "object", "properties": {"bark": {"type": "string"}}},
    "Secret": {"type": "object", "properties": {"name": {"type": "string"}, "password": {"type": "string", "writeOnly": true}}},
    "Login": {"allOf": [{"$ref": "#/components/schemas/Secret"}], "required": ["password"]},
    "LoginMember": {"allOf": [{"$ref": "#/components/schemas/Secret"}, {"type": "object", "required": ["password"]}]},
    "Password": {"type": "string", "writeOnly": true},
    "Token": {"type": "object", "required": ["token"], "properties": {"name": {"type": "string"}, "token": {"$ref": "#/components/schemas/Password"}}},
    "WrappedToken": {"type": "object", "required": ["token"], "properties": {"name": {"type": "string"}, "token": {"allOf": [{"$ref": "#/components/schemas/Password"}], "nullable": true}}},
    "Limits": {"type": "object", "properties": {
      "count": {"type": "integer", "maximum": 1000000},
      "big": {"type": "integer", "format": "int64", "maximum": 9223372036854775806}
    }}
  }}
}`

func TestSchemaClosesDeclaredObjects(t *testing.T) {
	// A renamed optional key and an undocumented key fail, at the top and
	// nested, through a $ref, a nullable wrapper, items and a map value.
	cases := []struct {
		value map[string]any
		at    string
	}{
		{map[string]any{"id": 1, "note_text": "x"}, "/note_text"},
		{map[string]any{"id": 1, "secret": "x"}, "/secret"},
		{map[string]any{"id": 1, "owner": map[string]any{"meow": "x", "purr": true}}, "/owner/purr"},
		{map[string]any{"id": 1, "parent": map[string]any{"id": 2, "extra": 1}}, "/parent/extra"},
		{map[string]any{"id": 1, "children": []any{map[string]any{"id": 2, "children": []any{map[string]any{"id": 3, "deep": 1}}}}}, "/children/0/children/0/deep"},
		{map[string]any{"id": 1, "pets": map[string]any{"tom": map[string]any{"wag": 1}}}, "/pets/tom/wag"},
	}
	for _, tc := range cases {
		errs := schemaErrors(t, closedSpec, "Note", tc.value)
		require.Len(t, errs, 1, tc.at)
		require.Contains(t, errs[0], `at "`+tc.at+`": the document does not declare this key: the field's json name differs from the document's, or the document lacks the field`)
	}

	// An embedded struct composes, maps and free-form objects take any key,
	// a nullable wrapper takes null, and the branches of an anyOf are open.
	spectest.Schema(t, []byte(closedSpec), "Note", map[string]any{
		"id":       1,
		"note":     "x",
		"owner":    map[string]any{"meow": "x"},
		"labels":   map[string]any{"any": "a", "key": "b"},
		"pets":     map[string]any{"tom": map[string]any{"meow": "x"}},
		"raw":      map[string]any{"whatever": []int{1}},
		"data":     map[string]any{"whatever": true},
		"parent":   nil,
		"children": []any{map[string]any{"id": 2, "parent": map[string]any{"id": 3}}},
		"pet":      map[string]any{"meow": "x"},
	})
	spectest.Schema(t, []byte(closedSpec), "Note", map[string]any{"id": 1, "pet": map[string]any{"bark": "x"}})
	spectest.Schema(t, []byte(closedSpec), "Note", map[string]any{"id": 1, "pet": map[string]any{"meow": "x", "wag": 1}})
	spectest.Schema(t, []byte(closedSpec), "AdminNote", map[string]any{"id": 1, "note": "x", "role": "ops"})

	errs := schemaErrors(t, closedSpec, "AdminNote", map[string]any{"id": 1, "rol": "ops"})
	require.Len(t, errs, 1)
	require.Contains(t, errs[0], `at "/rol": the document does not declare this key`)

	// A failure beneath does not make the keys around it read as undeclared.
	errs = schemaErrors(t, closedSpec, "Note", map[string]any{"id": "1", "note": "x", "owner": map[string]any{"meow": 1}})
	require.Len(t, errs, 1)
	require.Equal(t, `spectest: the value does not match component Note:
  at "/id": got string, want integer
  at "/owner/meow": got number, want string`, errs[0])
}

func TestSchemaWriteOnlyRequiredByComposer(t *testing.T) {
	// required beside the allOf, in a member of it, or naming a property
	// whose $ref leads to a writeOnly schema does not require it.
	for _, component := range []string{"Login", "LoginMember"} {
		spectest.Schema(t, []byte(closedSpec), component, map[string]any{"name": "ana"})

		errs := schemaErrors(t, closedSpec, component, map[string]any{"password": "x"})
		require.Len(t, errs, 1, component)
		require.Contains(t, errs[0], `at "/password": must be absent`, component)
	}

	// The same through a $ref, and through the allOf of a nullable wrapper.
	for _, component := range []string{"Token", "WrappedToken"} {
		spectest.Schema(t, []byte(closedSpec), component, map[string]any{"name": "ana"})

		errs := schemaErrors(t, closedSpec, component, map[string]any{"token": "x"})
		require.Len(t, errs, 1, component)
		require.Contains(t, errs[0], `at "/token": must be absent`, component)
	}
}

func TestSchemaBoundsExact(t *testing.T) {
	errs := schemaErrors(t, closedSpec, "Limits", map[string]any{"count": 1234567})
	require.Len(t, errs, 1)
	require.Contains(t, errs[0], `at "/count": maximum: got 1234567, want 1000000`)

	errs = schemaErrors(t, closedSpec, "Limits", json.RawMessage(`{"big": 9223372036854775807}`))
	require.Len(t, errs, 1)
	require.Contains(t, errs[0], `at "/big": maximum: got 9223372036854775807, want 9223372036854775806`)
}

func TestSchemaDropsSchemaKeyword(t *testing.T) {
	spec := `{"openapi": "3.1.0", "components": {"schemas": {
    "Tags": {"$schema": "https://spec.openapis.org/oas/3.1/dialect/base", "type": "object", "properties": {"tags": {"type": "array", "items": {"type": "string"}}}}
  }}}`
	spectest.Schema(t, []byte(spec), "Tags", tags{Tags: []string{"a"}})
}

func TestSchemaYAMLReadAsOtherTypes(t *testing.T) {
	errs := schemaErrors(t, "openapi: 3.1\ncomponents: {}\n", "Tags", tags{})
	require.Equal(t, []string{"spectest: Tags: the document is not OpenAPI 3.x: its openapi field is 3.1, not a string; quote it"}, errs)

	spec := `openapi: 3.1.0
components:
  schemas:
    Tags:
      type: object
      properties:
        %s:
          type: string
`
	errs = schemaErrors(t, fmt.Sprintf(spec, "null"), "Tags", tags{})
	require.Equal(t, []string{`spectest: Tags: the YAML document has a key at "/components/schemas/Tags/properties" that is not a string: it is read as null; quote the key`}, errs)

	errs = schemaErrors(t, fmt.Sprintf(spec, "1.10"), "Tags", tags{})
	require.Equal(t, []string{`spectest: Tags: the YAML document has a key at "/components/schemas/Tags/properties" that is not a string: it is read as the float64 1.1; quote the key`}, errs)
}

// openSpec holds the shapes an object stays open in.
const openSpec = `{
  "openapi": "3.0.3",
  "info": {"title": "t", "version": "1"},
  "paths": {},
  "components": {"schemas": {
    "Cat": {"type": "object", "properties": {"meow": {"type": "string"}}},
    "Dog": {"type": "object", "properties": {"bark": {"type": "string"}}},
    "Pet": {"oneOf": [{"$ref": "#/components/schemas/Cat"}, {"$ref": "#/components/schemas/Dog"}]},
    "AnyPet": {"anyOf": [{"$ref": "#/components/schemas/Cat"}, {"$ref": "#/components/schemas/Dog"}]},
    "Unions": {"type": "object", "properties": {
      "pet": {"$ref": "#/components/schemas/Pet"},
      "inline": {"oneOf": [{"$ref": "#/components/schemas/Cat"}, {"$ref": "#/components/schemas/Dog"}]},
      "any_pet": {"$ref": "#/components/schemas/AnyPet"},
      "any_inline": {"anyOf": [{"$ref": "#/components/schemas/Cat"}, {"$ref": "#/components/schemas/Dog"}]},
      "pets": {"type": "array", "items": {"anyOf": [{"$ref": "#/components/schemas/Cat"}, {"$ref": "#/components/schemas/Dog"}]}}
    }},
    "Composed": {"type": "object", "properties": {
      "one": {"type": "object", "properties": {"a": {"type": "string"}}, "oneOf": [
        {"required": ["b"], "properties": {"b": {"type": "string"}}},
        {"required": ["c"], "properties": {"c": {"type": "string"}}}
      ]},
      "any": {"type": "object", "properties": {"a": {"type": "string"}}, "anyOf": [{"properties": {"b": {"type": "string"}}}]},
      "cond": {"type": "object", "properties": {"a": {"type": "string"}}, "if": {"required": ["a"]}, "then": {"properties": {"b": {"type": "string"}}}}
    }},
    "Frozen": {"type": "object", "properties": {
      "not": {"not": {"type": "object", "properties": {"a": {}}}},
      "not_nested": {"not": {"type": "object", "required": ["a"], "properties": {"a": {"type": "object", "properties": {"c": {}}}}}},
      "contains": {"type": "array", "contains": {"type": "object", "properties": {"a": {}}}}
    }},
    "Meta": {"type": "object", "description": "third-party metadata"},
    "Base": {"type": "object", "properties": {"own": {"type": "string"}}},
    "Described": {"allOf": [{"$ref": "#/components/schemas/Meta"}, {"type": "object", "properties": {"own": {"type": "string"}}}]},
    "Bare": {"allOf": [{"type": "object"}, {"$ref": "#/components/schemas/Base"}]},
    "Constrained": {"allOf": [{"$ref": "#/components/schemas/Base"}, {"required": ["own"]}, {"description": "constrained"}, {"type": "object", "required": ["own"]}]},
    "Named": {"type": "object", "properties": {"a": {"type": "string"}}, "required": ["a", "b"]},
    "Holder": {"type": "object", "properties": {"x": {"type": "object", "properties": {"a": {"type": "string"}}}}},
    "Extended": {"allOf": [{"$ref": "#/components/schemas/Holder/properties/x"}, {"type": "object", "properties": {"b": {"type": "string"}}}]},
    "Bounds": {"type": "object", "properties": {
      "low": {"type": "number", "minimum": 0, "exclusiveMinimum": false},
      "high": {"type": "number", "maximum": 10, "exclusiveMaximum": false}
    }}
  }}
}`

func TestSchemaLeavesBranchesOpen(t *testing.T) {
	// An anyOf whose branches each declare part of the keys takes a value
	// holding keys of both, inline, by reference and as an item.
	both := map[string]any{"meow": "x", "bark": "y"}
	spectest.Schema(t, []byte(openSpec), "Unions", map[string]any{"any_inline": both})
	spectest.Schema(t, []byte(openSpec), "Unions", map[string]any{"any_pet": both})
	spectest.Schema(t, []byte(openSpec), "Unions", map[string]any{"pets": []any{both}})

	// A value two open branches of a oneOf match fails, as the document says,
	// inline and by reference alike, and no key is called undeclared.
	for _, key := range []string{"inline", "pet"} {
		errs := schemaErrors(t, openSpec, "Unions", map[string]any{key: map[string]any{"meow": "x"}})
		require.Len(t, errs, 1, key)
		require.Contains(t, errs[0], `at "/`+key+`": 'oneOf' failed, subschemas 0, 1 matched`, key)
		require.NotContains(t, errs[0], "does not declare", key)
	}
}

func TestSchemaOpenKeywordsKeepTheObjectOpen(t *testing.T) {
	// Properties beside a oneOf, an anyOf or an if whose branches declare
	// more keys take those keys.
	spectest.Schema(t, []byte(openSpec), "Composed", map[string]any{"one": map[string]any{"a": "x", "b": "y"}})
	spectest.Schema(t, []byte(openSpec), "Composed", map[string]any{"any": map[string]any{"a": "x", "b": "y"}})
	spectest.Schema(t, []byte(openSpec), "Composed", map[string]any{"cond": map[string]any{"a": "x", "b": "y"}})
}

func TestSchemaNeverClosesUnderNotOrContains(t *testing.T) {
	// Closed, the schema under not would fail these values and not would
	// pass them; open, as written, they match it and not refuses them.
	errs := schemaErrors(t, openSpec, "Frozen", map[string]any{"not": map[string]any{"b": 1}})
	require.Len(t, errs, 1)
	require.Contains(t, errs[0], `at "/not": 'not' failed`)

	errs = schemaErrors(t, openSpec, "Frozen", map[string]any{"not_nested": map[string]any{"a": map[string]any{"d": 1}}})
	require.Len(t, errs, 1)
	require.Contains(t, errs[0], `at "/not_nested": 'not' failed`)

	spectest.Schema(t, []byte(openSpec), "Frozen", map[string]any{"contains": []any{map[string]any{"b": 1}}})
}

func TestSchemaFreeFormMemberOpensTheChain(t *testing.T) {
	// An allOf member that is an object schema saying nothing of its keys
	// lets the object take any key, reached by $ref or written inline.
	spectest.Schema(t, []byte(openSpec), "Described", map[string]any{"own": "x", "rank": 1})
	spectest.Schema(t, []byte(openSpec), "Bare", map[string]any{"own": "x", "rank": 1})

	// Members that only constrain or describe do not.
	errs := schemaErrors(t, openSpec, "Constrained", map[string]any{"own": "x", "rank": 1})
	require.Len(t, errs, 1)
	require.Contains(t, errs[0], `at "/rank": the document does not declare this key`)
}

func TestSchemaRequiredNamesAreDeclared(t *testing.T) {
	spectest.Schema(t, []byte(openSpec), "Named", map[string]any{"a": "x", "b": 1})

	errs := schemaErrors(t, openSpec, "Named", map[string]any{"a": "x", "b": 1, "c": 2})
	require.Len(t, errs, 1)
	require.Contains(t, errs[0], `at "/c": the document does not declare this key`)
}

func TestSchemaRefIntoAComponentIsReadAsWritten(t *testing.T) {
	// A $ref into a property, used as an allOf member, composes with the
	// keys beside it, and the object is checked as the document writes it.
	spectest.Schema(t, []byte(openSpec), "Extended", map[string]any{"a": "x", "b": "y"})
	spectest.Schema(t, []byte(openSpec), "Extended", map[string]any{"a": "x", "b": "y", "c": 1})

	// The property it points into is still closed where it is written.
	errs := schemaErrors(t, openSpec, "Holder", map[string]any{"x": map[string]any{"a": "x", "b": "y"}})
	require.Len(t, errs, 1)
	require.Contains(t, errs[0], `at "/x/b": the document does not declare this key`)
}

func TestSchemaExclusiveBoundFalse30(t *testing.T) {
	// In OpenAPI 3.0, exclusiveMinimum: false leaves the bound inclusive.
	spectest.Schema(t, []byte(openSpec), "Bounds", map[string]any{"low": 0, "high": 10})

	errs := schemaErrors(t, openSpec, "Bounds", map[string]any{"low": -1, "high": 11})
	require.Len(t, errs, 1)
	require.Contains(t, errs[0], `at "/low": minimum: got -1, want 0`)
	require.Contains(t, errs[0], `at "/high": maximum: got 11, want 10`)
}

// writtenSpec holds schemas that are checked as the document writes them,
// beside closed objects, in OpenAPI 3.1.
const writtenSpec = `{
  "openapi": "3.1.0",
  "info": {"title": "t", "version": "1"},
  "paths": {},
  "components": {"schemas": {
    "AnyNested": {"anyOf": [
      {"type": "object", "properties": {"a": {"type": "object", "properties": {"x": {}}}}},
      {"type": "object", "properties": {"a": {"type": "object", "properties": {"y": {}}}}}
    ]},
    "OneNested": {"oneOf": [
      {"type": "object", "properties": {"a": {"type": "object", "properties": {"x": {}}}}},
      {"type": "object", "properties": {"a": {"type": "object", "properties": {"y": {}}}}}
    ]},
    "X": {"type": "object", "properties": {"a": {"type": "object", "properties": {"x": {}}}}},
    "Y": {"type": "object", "properties": {"a": {"type": "object", "properties": {"y": {}}}}},
    "AnyRef": {"anyOf": [{"$ref": "#/components/schemas/X"}, {"$ref": "#/components/schemas/Y"}]},
    "OneRef": {"oneOf": [{"$ref": "#/components/schemas/X"}, {"$ref": "#/components/schemas/Y"}]},
    "AnyItems": {"type": "array", "items": {"$ref": "#/components/schemas/AnyRef"}},
    "B": {"type": "object", "required": ["c"], "properties": {"c": {"type": "object", "properties": {"x": {}}}}},
    "IfB": {"if": {"$ref": "#/components/schemas/B"}, "then": {"type": "object"}, "else": {"type": "string"}},
    "NotB": {"type": "object", "not": {"$ref": "#/components/schemas/B"}},
    "WriteOnlyInNot": {"type": "object", "properties": {"name": {"type": "string"}}, "not": {"properties": {"pw": {"type": "string", "writeOnly": true}}, "required": ["pw"]}},
    "Pet": {"type": "object", "required": ["pet_type"], "properties": {"pet_type": {"type": "string"}}, "discriminator": {"propertyName": "pet_type"}},
    "Cat": {"allOf": [{"$ref": "#/components/schemas/Pet"}, {"type": "object", "properties": {"meow": {"type": "boolean"}}}]},
    "Owner": {"type": "object", "properties": {"pet": {"$ref": "#/components/schemas/Pet"}}},
    "Base": {"type": "object", "properties": {"own": {"type": "string"}}},
    "TypedRef": {"type": "object", "$ref": "#/components/schemas/Base"},
    "NullableFreeForm": {"allOf": [{"type": ["object", "null"]}, {"$ref": "#/components/schemas/Base"}]},
    "Nested": {"type": "object", "properties": {"n": {"type": "object", "properties": {"a": {"type": "string"}}}}},
    "Tuple": {"type": "array", "prefixItems": [{"type": "object", "properties": {"a": {}}}]},
    "Big": {"type": "object", "properties": {
      "min": {"type": "integer", "minimum": 9223372036854775806},
      "xmin": {"type": "integer", "exclusiveMinimum": 9223372036854775806},
      "xmax": {"type": "integer", "exclusiveMaximum": 9223372036854775806},
      "mult": {"type": "integer", "multipleOf": 9007199254740993}
    }},
    "A B": {"type": "object", "properties": {"x": {"type": "string"}}},
    "a/b": {"type": "object", "properties": {"x": {"type": "string"}}},
    "a~b": {"type": "object", "properties": {"x": {"type": "string"}}},
    "a%b": {"type": "object", "properties": {"x": {"type": "string"}}},
    "Names": {"type": "object", "properties": {
      "space": {"$ref": "#/components/schemas/A%20B"},
      "slash": {"$ref": "#/components/schemas/a~1b"},
      "tilde": {"$ref": "#/components/schemas/a~0b"}
    }}
  }}
}`

func TestSchemaClosesNothingInBranches(t *testing.T) {
	// An object nested in the branches of an anyOf takes the keys either
	// branch declares, inline, by reference and in items.
	both := map[string]any{"a": map[string]any{"x": 1, "y": 2}}
	spectest.Schema(t, []byte(writtenSpec), "AnyNested", both)
	spectest.Schema(t, []byte(writtenSpec), "AnyRef", both)
	spectest.Schema(t, []byte(writtenSpec), "AnyItems", []any{both})

	// In a oneOf, both open branches match, which the document refuses.
	for _, component := range []string{"OneNested", "OneRef"} {
		errs := schemaErrors(t, writtenSpec, component, map[string]any{"a": map[string]any{"x": 1}})
		require.Len(t, errs, 1, component)
		require.Contains(t, errs[0], `at "": 'oneOf' failed, subschemas 0, 1 matched`, component)
	}
}

func TestSchemaReadsIfAndNotAsWritten(t *testing.T) {
	// B as written matches, so if takes then, and not refuses the value.
	value := map[string]any{"c": map[string]any{"x": 1, "y": 2}}
	spectest.Schema(t, []byte(writtenSpec), "IfB", value)

	errs := schemaErrors(t, writtenSpec, "NotB", value)
	require.Len(t, errs, 1)
	require.Contains(t, errs[0], `at "": 'not' failed`)

	// A writeOnly property under not stays as written: the value lacks pw,
	// so the schema under not fails and not passes.
	spectest.Schema(t, []byte(writtenSpec), "WriteOnlyInNot", map[string]any{"name": "a"})
}

func TestSchemaDiscriminatorBaseStaysOpen(t *testing.T) {
	// The base of an allOf hierarchy takes the keys of the schemas that
	// extend it.
	spectest.Schema(t, []byte(writtenSpec), "Owner", map[string]any{"pet": map[string]any{"pet_type": "Cat", "meow": true}})
	spectest.Schema(t, []byte(writtenSpec), "Pet", map[string]any{"pet_type": "Cat", "meow": true})
}

func TestSchemaOpenKeywordsBesideProperties(t *testing.T) {
	cases := map[string]string{
		"patternProperties":     `{"^b$": {}}`,
		"additionalProperties":  `{"type": "integer"}`,
		"unevaluatedProperties": `{"type": "integer"}`,
		"propertyNames":         `{"maxLength": 1}`,
		"dependentSchemas":      `{"a": {"properties": {"b": {}}}}`,
		"dependentRequired":     `{"a": ["b"]}`,
		"$dynamicRef":           `"#/components/schemas/Holder"`,
	}
	for keyword, schema := range cases {
		spec := `{"openapi": "3.1.0", "components": {"schemas": {
      "Holder": {"type": "object", "properties": {"o": {"$ref": "#/components/schemas/O"}}},
      "O": {"type": "object", "properties": {"a": {}}, "` + keyword + `": ` + schema + `}
    }}}`
		value := map[string]any{"a": 1, "b": 1}
		errs := schemaErrors(t, spec, "O", value)
		require.Empty(t, errs, keyword)
		errs = schemaErrors(t, spec, "Holder", map[string]any{"o": value})
		require.Empty(t, errs, keyword)
	}
}

func TestSchemaFreeFormRules(t *testing.T) {
	// type object beside a $ref is no free-form object: the $ref declares.
	errs := schemaErrors(t, writtenSpec, "TypedRef", map[string]any{"own": "x", "rank": 1})
	require.Len(t, errs, 1)
	require.Contains(t, errs[0], `at "/rank": the document does not declare this key`)

	// A type list naming object is free-form.
	spectest.Schema(t, []byte(writtenSpec), "NullableFreeForm", map[string]any{"own": "x", "rank": 1})
}

func TestSchemaClosingKeepsNestedTypes(t *testing.T) {
	errs := schemaErrors(t, writtenSpec, "Nested", map[string]any{"n": map[string]any{"a": 1}})
	require.Len(t, errs, 1)
	require.Contains(t, errs[0], `at "/n/a": got number, want string`)

	errs = schemaErrors(t, writtenSpec, "Tuple", []any{map[string]any{"a": 1, "b": 2}})
	require.Len(t, errs, 1)
	require.Contains(t, errs[0], `at "/0/b": the document does not declare this key`)
}

func TestSchemaBigBoundsExact(t *testing.T) {
	errs := schemaErrors(t, writtenSpec, "Big", json.RawMessage(`{"min": 9223372036854775805, "xmin": 9223372036854775806, "xmax": 9223372036854775806, "mult": 9007199254740994}`))
	require.Equal(t, []string{`spectest: the value does not match component Big:
  at "/min": minimum: got 9223372036854775805, want 9223372036854775806
  at "/mult": multipleOf: got 9007199254740994, want 9007199254740993
  at "/xmax": exclusiveMaximum: got 9223372036854775806, want 9223372036854775806
  at "/xmin": exclusiveMinimum: got 9223372036854775806, want 9223372036854775806`}, errs)
}

func TestSchemaComponentNamesNeedEscaping(t *testing.T) {
	for _, name := range []string{"A B", "a/b", "a~b", "a%b"} {
		spectest.Schema(t, []byte(writtenSpec), name, map[string]any{"x": "y"})

		errs := schemaErrors(t, writtenSpec, name, map[string]any{"x": 1})
		require.Len(t, errs, 1, name)
		require.Contains(t, errs[0], `at "/x": got number, want string`, name)
	}

	// A $ref with an escaped name leads to the closed copy.
	for _, key := range []string{"space", "slash", "tilde"} {
		errs := schemaErrors(t, writtenSpec, "Names", map[string]any{key: map[string]any{"x": "y", "z": 1}})
		require.Len(t, errs, 1, key)
		require.Contains(t, errs[0], `at "/`+key+`/z": the document does not declare this key`, key)
	}
}

func TestSchemaCyclesEnd(t *testing.T) {
	spec := `{"openapi": "3.1.0", "components": {"schemas": {
    "A": {"allOf": [{"$ref": "#/components/schemas/B"}], "properties": {"a": {}}},
    "B": {"allOf": [{"$ref": "#/components/schemas/A"}], "properties": {"b": {}}},
    "H": {"type": "object", "required": ["p"], "properties": {"p": {"$ref": "#/components/schemas/A"}}}
  }}}`
	done := make(chan []string)
	go func() {
		fake := &fakeTB{TB: t}
		spectest.Schema(fake, []byte(spec), "H", map[string]any{"p": map[string]any{"a": 1}})
		done <- fake.errors
	}()
	select {
	case errs := <-done:
		// The JSON Schema library refuses the cycle itself.
		require.Len(t, errs, 1)
		require.Contains(t, errs[0], "causing reference cycle")
	case <-time.After(30 * time.Second):
		t.Fatal("Schema did not return on a cyclic allOf")
	}
}

func TestSchemaYAMLIntegerKeys(t *testing.T) {
	spec := `openapi: 3.1.0
components:
  schemas:
    Codes:
      type: object
      properties:
        200:
          type: string
        18446744073709551615:
          type: string
`
	errs := schemaErrors(t, spec, "Codes", map[string]any{"200": 1, "18446744073709551615": 2})
	require.Equal(t, []string{`spectest: the value does not match component Codes:
  at "/18446744073709551615": got number, want string
  at "/200": got number, want string`}, errs)
}

func TestSchemaRefFragmentIsPercentDecoded(t *testing.T) {
	// #/components/schemas/a%2Fb is the pointer /components/schemas/a/b, the
	// schema under the key b of component a, not the component named a/b.
	spec := `{"openapi": "3.1.0", "components": {"schemas": {
    "a": {"type": "object", "b": {"type": "object", "properties": {"y": {"type": "string"}}}},
    "a/b": {"type": "object", "properties": {"x": {"type": "string"}}},
    "Holder": {"type": "object", "properties": {"p": {"$ref": "#/components/schemas/a%2Fb"}}}
  }}}`
	errs := schemaErrors(t, spec, "Holder", map[string]any{"p": map[string]any{"y": 1}})
	require.Equal(t, []string{`spectest: the value does not match component Holder:
  at "/p/y": got number, want string`}, errs)
}

func TestSchemaJSONWithByteOrderMark(t *testing.T) {
	// Read as YAML, the bound would be rounded to a float64 and the value
	// would pass.
	spec := "\xef\xbb\xbf" + `{"openapi": "3.1.0", "components": {"schemas": {
    "Big": {"type": "integer", "maximum": 123456789012345678901234567890}
  }}}`
	errs := schemaErrors(t, spec, "Big", json.RawMessage(`123456789012345678901234567891`))
	require.Equal(t, []string{`spectest: the value does not match component Big:
  at "": maximum: got 123456789012345678901234567891, want 123456789012345678901234567890`}, errs)
}

func TestSchemaRepeatedFailureListedOnce(t *testing.T) {
	spec := `{"openapi": "3.1.0", "components": {"schemas": {
    "Twice": {"allOf": [{"type": "string"}, {"type": "string"}]}
  }}}`
	errs := schemaErrors(t, spec, "Twice", 1)
	require.Equal(t, []string{`spectest: the value does not match component Twice:
  at "": got number, want string`}, errs)
}

func TestSchemaFailuresCapped(t *testing.T) {
	spec := `{"openapi": "3.1.0", "components": {"schemas": {
    "Numbers": {"type": "array", "items": {"type": "integer"}}
  }}}`
	value := make([]string, 105)
	errs := schemaErrors(t, spec, "Numbers", value)
	require.Len(t, errs, 1)
	lines := strings.Split(errs[0], "\n  ")
	// The heading, 100 failures and the count of the rest.
	require.Len(t, lines, 102)
	require.Equal(t, "and 5 more", lines[101])
}

func TestSchemaDefinitionsRead30(t *testing.T) {
	// A schema under definitions takes the 3.0 reading of nullable too.
	spec := `{"openapi": "3.0.3", "components": {"schemas": {
    "Holder": {"type": "object", "definitions": {"Name": {"type": "string", "nullable": true}}, "properties": {"name": {"$ref": "#/components/schemas/Holder/definitions/Name"}}}
  }}}`
	spectest.Schema(t, []byte(spec), "Holder", map[string]any{"name": nil})
}

func TestSchemaClosedNamesMatchWhole(t *testing.T) {
	// A declared name reached through a $ref admits that key only.
	spec := `{"openapi": "3.1.0", "components": {"schemas": {
    "O": {"type": "object", "properties": {"a.b": {}}},
    "Holder": {"type": "object", "properties": {"o": {"$ref": "#/components/schemas/O"}}}
  }}}`
	errs := schemaErrors(t, spec, "Holder", map[string]any{"o": map[string]any{"a.b": 1, "axb": 1, "a.bc": 1}})
	require.Equal(t, []string{`spectest: the value does not match component Holder:
  at "/o/a.bc": the document does not declare this key: the field's json name differs from the document's, or the document lacks the field
  at "/o/axb": the document does not declare this key: the field's json name differs from the document's, or the document lacks the field`}, errs)
}
