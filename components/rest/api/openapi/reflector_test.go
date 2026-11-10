package openapi

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/vanclief/ez"

	"github.com/vanclief/compose/components/rest/api/internal/huma"
	"github.com/vanclief/compose/components/rest/api/openapi/internal/fixture"
)

// toMap marshals v through huma's MarshalJSON and decodes it generically.
func toMap(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	var out map[string]any
	err = json.Unmarshal(raw, &out)
	require.NoError(t, err)
	return out
}

func component(t *testing.T, res *Result, name string) map[string]any {
	t.Helper()
	s, ok := res.Components[name]
	require.True(t, ok, "missing component %s", name)
	return toMap(t, s)
}

func prop(t *testing.T, schema map[string]any, name string) map[string]any {
	t.Helper()
	props, ok := schema["properties"].(map[string]any)
	require.True(t, ok)
	p, ok := props[name].(map[string]any)
	require.True(t, ok, "missing property %s", name)
	return p
}

func ref(name string) string {
	return RefPrefix + name
}

func TestComponentNames(t *testing.T) {
	res, _ := buildFixture(t)
	require.ElementsMatch(t, []string{
		"fixture.Config", "fixture.ConfigInput", "fixture.CreateRequest", "fixture.Flags",
		"fixture.ListResponse", "fixture.Meta", "fixture.Mode", "fixture.Node", "fixture.NodeInput", "fixture.Page_fixture.Run",
		"fixture.Page_other.Run", "fixture.Run", "fixture.RunStatus", "fixture.Tree",
		"fixture.UpdateRequest", "other.Run",
	}, sortedKeys(res.Components))
}

func TestSchemaName(t *testing.T) {
	tests := []struct {
		typ  reflect.Type
		want string
	}{
		{reflect.TypeFor[fixture.Run](), "fixture.Run"},
		{reflect.TypeFor[*fixture.Run](), "fixture.Run"},
		{reflect.TypeFor[fixture.Page[*fixture.Run]](), "fixture.Page_Ptrfixture.Run"},
		{reflect.TypeFor[fixture.Page[[]fixture.Run]](), "fixture.Page_Listfixture.Run"},
		{reflect.TypeFor[fixture.Page[fixture.Page[string]]](), "fixture.Page_fixture.Page_string"},
		{reflect.TypeFor[fixture.Page[map[string]int]](), "fixture.Page_map_string_int"},
	}
	for _, tt := range tests {
		require.Equal(t, tt.want, DefaultNamer(tt.typ), tt.typ.String())
	}
}

func TestEnums(t *testing.T) {
	res, _ := buildFixture(t)

	status := component(t, res, "fixture.RunStatus")
	require.Equal(t, "string", status["type"])
	require.Equal(t, []any{"pending", "running", "completed", "failed", "cancelled"}, status["enum"])

	run := component(t, res, "fixture.Run")
	require.Equal(t, map[string]any{"$ref": ref("fixture.RunStatus")}, prop(t, run, "status"))

	update := component(t, res, "fixture.UpdateRequest")
	require.Equal(t, map[string]any{"anyOf": []any{
		map[string]any{"$ref": ref("fixture.RunStatus"), "enum": []any{"failed", "cancelled"}},
		map[string]any{"type": "null"},
	}}, prop(t, update, "status"))
}

func TestListRequestParams(t *testing.T) {
	res, params := buildFixture(t)
	typ := reflect.TypeFor[fixture.ListRequest]()

	byName := map[string]Param{}
	for _, p := range params[typ] {
		require.Equal(t, "query", p.In)
		require.False(t, p.Required)
		byName[p.Name] = p
	}
	require.Len(t, byName, 6)

	limit := byName["limit"]
	require.Equal(t, []int{0, 0}, limit.Index)
	require.Equal(t, "Page size, defaults to 50", limit.Doc)
	require.Equal(t, map[string]any{"type": "integer", "format": "int64", "minimum": 0.0, "maximum": 1000.0}, toMap(t, limit.Schema))

	require.Equal(t, map[string]any{"type": "string"}, toMap(t, byName["cursor"].Schema))
	require.Equal(t, map[string]any{"type": "string"}, toMap(t, byName["sort"].Schema))
	require.Equal(t, map[string]any{"type": "boolean"}, toMap(t, byName["include_count"].Schema))
	require.Equal(t, map[string]any{"type": "string", "format": "uuid"}, toMap(t, byName["project_id"].Schema))
	require.Equal(t, map[string]any{"$ref": ref("fixture.RunStatus")}, toMap(t, byName["status"].Schema))

	_, _, ok := res.BodyRef(typ)
	require.False(t, ok)
	require.NotContains(t, res.Components, "fixture.ListRequest")
}

func TestUpdateRequest(t *testing.T) {
	res, params := buildFixture(t)
	typ := reflect.TypeFor[fixture.UpdateRequest]()

	require.Len(t, params[typ], 1)
	id := params[typ][0]
	require.Equal(t, "id", id.Name)
	require.Equal(t, "path", id.In)
	require.True(t, id.Required)
	require.Equal(t, []int{0}, id.Index)
	require.Equal(t, map[string]any{"type": "string", "format": "uuid"}, toMap(t, id.Schema))

	bodyRef, required, ok := res.BodyRef(typ)
	require.True(t, ok)
	require.False(t, required)
	require.Equal(t, ref("fixture.UpdateRequest"), bodyRef)

	update := component(t, res, "fixture.UpdateRequest")
	require.NotContains(t, update, "required")
	require.Equal(t, []any{"integer", "null"}, prop(t, update, "max_total_runs")["type"])
	require.Equal(t, []any{"string", "null"}, prop(t, update, "code_snapshot_id")["type"])
	require.Equal(t, []any{"string", "null"}, prop(t, update, "failure_reason")["type"])
}

func TestCreateRequest(t *testing.T) {
	res, params := buildFixture(t)
	typ := reflect.TypeFor[fixture.CreateRequest]()
	require.Empty(t, params[typ])

	bodyRef, required, ok := res.BodyRef(reflect.TypeFor[*fixture.CreateRequest]())
	require.True(t, ok)
	require.True(t, required)
	require.Equal(t, ref("fixture.CreateRequest"), bodyRef)

	create := component(t, res, "fixture.CreateRequest")
	require.Equal(t, []any{"name"}, create["required"])
	require.Equal(t, ref("fixture.ConfigInput"), prop(t, create, "config")["$ref"])
}

func TestBothDirectionsVariant(t *testing.T) {
	res, _ := buildFixture(t)

	base := component(t, res, "fixture.Config")
	input := component(t, res, "fixture.ConfigInput")
	require.Equal(t, []any{"retries"}, base["required"])
	require.NotContains(t, input, "required")
	require.Equal(t, "integer", prop(t, base, "timeout")["type"])
	require.Equal(t, []any{"integer", "null"}, prop(t, input, "timeout")["type"])

	// Apart from required and nullability they are identical.
	delete(base, "required")
	prop(t, input, "timeout")["type"] = "integer"
	require.Equal(t, base, input)

	run := component(t, res, "fixture.Run")
	require.Equal(t, ref("fixture.Config"), prop(t, run, "config")["$ref"])
}

func TestRecursiveVariant(t *testing.T) {
	res, _ := buildFixture(t)

	node := component(t, res, "fixture.Node")
	input := component(t, res, "fixture.NodeInput")
	require.Equal(t, ref("fixture.Node"), prop(t, node, "children")["items"].(map[string]any)["$ref"])
	require.Equal(t, ref("fixture.NodeInput"), prop(t, input, "children")["items"].(map[string]any)["$ref"])
	require.Equal(t, []any{"name"}, input["required"])
	require.Equal(t, []any{"string", "null"}, prop(t, input, "label")["type"])

	tree := component(t, res, "fixture.Tree")
	require.Equal(t, ref("fixture.Node"), prop(t, tree, "root")["$ref"])

	bodyRef, required, ok := res.BodyRef(reflect.TypeFor[fixture.Node]())
	require.True(t, ok)
	require.True(t, required)
	require.Equal(t, ref("fixture.NodeInput"), bodyRef)
}

func TestAdditionalProperties(t *testing.T) {
	res, _ := buildFixture(t)

	raw, err := json.Marshal(res.Components)
	require.NoError(t, err)
	require.NotContains(t, string(raw), `"additionalProperties":false`)
	// The only additionalProperties left is the map value schema of Meta.labels.
	require.Equal(t, 1, strings.Count(string(raw), `"additionalProperties"`))

	strict := toMap(t, res.StrictComponents())
	closed := []string{
		"fixture.Config", "fixture.Flags", "fixture.ListResponse", "fixture.Meta", "fixture.Node",
		"fixture.Page_fixture.Run", "fixture.Page_other.Run", "fixture.Run", "fixture.Tree", "other.Run",
	}
	for name, s := range strict {
		_, has := s.(map[string]any)["additionalProperties"]
		if contains(closed, name) {
			require.Equal(t, false, s.(map[string]any)["additionalProperties"], name)
			continue
		}
		require.False(t, has, name)
	}

	// StrictComponents is a copy.
	raw2, err := json.Marshal(res.Components)
	require.NoError(t, err)
	require.Equal(t, string(raw), string(raw2))
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func TestFormats(t *testing.T) {
	res, _ := buildFixture(t)
	run := component(t, res, "fixture.Run")
	require.Equal(t, map[string]any{"type": "string", "format": "uuid"}, prop(t, run, "id"))
	require.Equal(t, map[string]any{"type": "string", "format": "date-time"}, prop(t, run, "created_at"))
	require.Equal(t, map[string]any{"type": "string", "format": "email"}, prop(t, component(t, res, "fixture.Config"), "notify"))
}

func TestResponseSchema(t *testing.T) {
	res, _ := buildFixture(t)
	require.Equal(t, map[string]any{"$ref": ref("fixture.Run")}, toMap(t, res.ResponseSchema(reflect.TypeFor[*fixture.Run]())))
	require.Equal(t, map[string]any{
		"type":  []any{"array", "null"},
		"items": map[string]any{"$ref": ref("fixture.Run")},
	}, toMap(t, res.ResponseSchema(reflect.TypeFor[[]fixture.Run]())))
	require.Nil(t, res.ResponseSchema(reflect.TypeFor[fixture.Meta]()))
}

func TestIdempotent(t *testing.T) {
	r := NewReflector()
	typ := reflect.TypeFor[fixture.UpdateRequest]()
	first, err := r.AddRequest(typ)
	require.NoError(t, err)
	second, err := r.AddRequest(reflect.TypeFor[*fixture.UpdateRequest]())
	require.NoError(t, err)
	require.Equal(t, first, second)

	err = r.AddResponse(reflect.TypeFor[fixture.Run]())
	require.NoError(t, err)
	err = r.AddResponse(reflect.TypeFor[fixture.Run]())
	require.NoError(t, err)

	res, err := r.Finalize()
	require.NoError(t, err)
	again, err := r.Finalize()
	require.NoError(t, err)
	require.Same(t, res, again)

	err = r.AddResponse(reflect.TypeFor[fixture.Tree]())
	require.Error(t, err)
}

func TestErrors(t *testing.T) {
	tests := []struct {
		name    string
		typ     reflect.Type
		request bool
		want    string
	}{
		{"unknown tag", reflect.TypeFor[fixture.BadTag](), false, `unknown struct tag "minimun"`},
		{"echo param tag", reflect.TypeFor[fixture.EchoParam](), true, "param is Echo's binder tag"},
		{"missing json dash", reflect.TypeFor[fixture.MissingDash](), true, `fixture.MissingDash.Status: query parameter fields must carry json:"-"`},
		{"contentEncoding", reflect.TypeFor[fixture.ContentEncodingProp](), false, `property name "contentEncoding"`},
		{"struct param", reflect.TypeFor[fixture.StructParam](), true, "struct query parameters are not supported"},
		{"duplicate param", reflect.TypeFor[fixture.DuplicateParam](), true, `duplicate header parameter "x-trace"`},
		{"huma panic", reflect.TypeFor[fixture.BadValue](), false, "invalid"},
		{"non-struct request", reflect.TypeFor[[]fixture.Run](), true, "must be a plain struct"},
		{"nullable struct ref", reflect.TypeFor[fixture.NullableStruct](), false, "fixture.NullableStruct.Meta: nullable tag is not supported on object references; use a pointer"},
		{"bound beyond int8", reflect.TypeFor[fixture.WideBound](), false, "fixture.WideBound.N: maximum 1000 is above the int8 range"},
		{"json string option in a request", reflect.TypeFor[fixture.StringOption](), true, "fixture.StringOption.N: the json string option is not supported"},
		{"json string option in a response", reflect.TypeFor[fixture.StringOption](), false, "fixture.StringOption.N: the json string option is not supported"},
		{"default tag in a request body", reflect.TypeFor[fixture.DefaultTag](), true, "fixture.DefaultTag.Limit: the default tag is not supported on a request type"},
		{"default tag on a parameter", reflect.TypeFor[fixture.DefaultParam](), true, "fixture.DefaultParam.Limit: the default tag is not supported on a request type"},
		{"query tag in a named field", reflect.TypeFor[fixture.NestedParam](), true, "fixture.NestedQuery.Q: the query tag is ignored on a nested struct; parameters and files are read only from the request type and the structs it embeds"},
		{"query tag in a slice element", reflect.TypeFor[fixture.NestedParamSlice](), true, "fixture.NestedQuery.Q: the query tag is ignored on a nested struct"},
		{"query tag in an Optional", reflect.TypeFor[fixture.NestedParamOptional](), true, "fixture.NestedQuery.Q: the query tag is ignored on a nested struct"},
		{"query tag embedded and nested", reflect.TypeFor[fixture.EmbeddedAndNested](), true, "fixture.NestedQuery.Q: the query tag is ignored on a nested struct"},
		{"form tag in a named field", reflect.TypeFor[fixture.NestedFile](), true, "fixture.NestedFileHolder.File: the form tag is ignored on a nested struct"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewReflector()
			var err error
			if tt.request {
				_, err = r.AddRequest(tt.typ)
			} else {
				err = r.AddResponse(tt.typ)
			}
			require.Error(t, err)
			require.Equal(t, ez.EINVALID, ez.ErrorCode(err))
			require.Contains(t, err.Error(), tt.want)
		})
	}
}

// Parameters of an embedded struct are read; a response type may nest
// parameter tags, which mean nothing there.
func TestParamTagsDepth(t *testing.T) {
	r := NewReflector()
	params, err := r.AddRequest(reflect.TypeFor[struct{ fixture.NestedQuery }]())
	require.NoError(t, err)
	require.Len(t, params, 1)
	require.Equal(t, "q", params[0].Name)
	require.Equal(t, "query", params[0].In)

	err = r.AddResponse(reflect.TypeFor[fixture.NestedParam]())
	require.NoError(t, err)
}

func TestVariantNameCollision(t *testing.T) {
	r := NewReflector()
	_, err := r.AddRequest(reflect.TypeFor[fixture.Pair]())
	require.NoError(t, err)
	err = r.AddResponse(reflect.TypeFor[fixture.Pair]())
	require.NoError(t, err)
	err = r.AddResponse(reflect.TypeFor[fixture.PairInput]())
	require.NoError(t, err)

	_, err = r.Finalize()
	require.Error(t, err)
	require.Contains(t, err.Error(), "duplicate component name fixture.PairInput")
}

func TestDowngrade(t *testing.T) {
	res, _ := buildFixture(t)

	schemas := huma.NewMapRegistry(RefPrefix, huma.DefaultSchemaNamer)
	var refs []any
	for name, s := range res.Components {
		schemas.Map()[name] = s
		refs = append(refs, map[string]any{"$ref": ref(name)})
	}
	doc := &huma.OpenAPI{
		OpenAPI:    "3.1.0",
		Info:       &huma.Info{Title: "fixture", Version: "1"},
		Components: &huma.Components{Schemas: schemas},
		// OpenAPI.MarshalJSON prunes schemas no $ref points at.
		Extensions: map[string]any{"x-refs": refs},
	}

	raw, err := doc.Downgrade()
	require.NoError(t, err)

	var down struct {
		OpenAPI    string `json:"openapi"`
		Components struct {
			Schemas map[string]map[string]any `json:"schemas"`
		} `json:"components"`
	}
	err = json.Unmarshal(raw, &down)
	require.NoError(t, err)
	require.Equal(t, "3.0.3", down.OpenAPI)
	require.Len(t, down.Components.Schemas, len(res.Components))

	update := down.Components.Schemas["fixture.UpdateRequest"]
	require.Equal(t, map[string]any{"type": "string", "nullable": true}, prop(t, update, "failure_reason"))
	require.Equal(t, map[string]any{"type": "integer", "format": "int64", "minimum": 0.0, "nullable": true}, prop(t, update, "max_total_runs"))
	runs := prop(t, down.Components.Schemas["fixture.ListResponse"], "runs")
	require.Equal(t, "array", runs["type"])
	require.Equal(t, true, runs["nullable"])

	// Nullable refs keep the 3.1 anyOf form; huma's Downgrade does not rewrite
	// {type: null}. The api package owns any 3.0-specific fix-up.
	require.Equal(t, map[string]any{"type": "null"}, prop(t, update, "status")["anyOf"].([]any)[1])
}

func TestOptional(t *testing.T) {
	r := NewReflector()
	_, err := r.AddRequest(reflect.TypeFor[fixture.OptionalRequest]())
	require.NoError(t, err)
	err = r.AddResponse(reflect.TypeFor[fixture.OptionalResponse]())
	require.NoError(t, err)
	res, err := r.Finalize()
	require.NoError(t, err)

	bodyRef, required, ok := res.BodyRef(reflect.TypeFor[fixture.OptionalRequest]())
	require.True(t, ok)
	require.True(t, required)
	require.Equal(t, ref("fixture.OptionalRequest"), bodyRef)

	// Request: the schema of T made nullable, with the field tags applied to
	// T's schema; only required:"true" makes the key required.
	req := component(t, res, "fixture.OptionalRequest")
	require.Equal(t, []any{"must"}, req["required"])
	require.Equal(t, map[string]any{"type": []any{"string", "null"}, "minLength": 1.0, "description": "Display name"}, prop(t, req, "name"))
	require.Equal(t, map[string]any{"anyOf": []any{
		map[string]any{"$ref": ref("fixture.Mode"), "enum": []any{"a"}},
		map[string]any{"type": "null"},
	}}, prop(t, req, "mode"))
	require.Equal(t, map[string]any{"anyOf": []any{
		map[string]any{"$ref": ref("fixture.MetaInput")},
		map[string]any{"type": "null"},
	}}, prop(t, req, "meta"))
	require.Equal(t, map[string]any{"type": []any{"array", "null"}, "items": map[string]any{"type": "string"}}, prop(t, req, "tags"))
	require.Equal(t, map[string]any{
		"type": []any{"integer", "null"}, "format": "int32", "minimum": float64(math.MinInt32), "maximum": float64(math.MaxInt32),
	}, prop(t, req, "small"))
	require.Equal(t, map[string]any{"type": []any{"string", "null"}}, prop(t, req, "must"))

	// Response: a nullable leaf, required unless omitempty or omitzero.
	resp := component(t, res, "fixture.OptionalResponse")
	require.Equal(t, []any{"name"}, resp["required"])
	require.Equal(t, map[string]any{"type": []any{"string", "null"}}, prop(t, resp, "name"))
	require.Equal(t, map[string]any{"anyOf": []any{
		map[string]any{"$ref": ref("fixture.Mode")},
		map[string]any{"type": "null"},
	}}, prop(t, resp, "mode"))
	require.Equal(t, map[string]any{"anyOf": []any{
		map[string]any{"$ref": ref("fixture.Meta")},
		map[string]any{"type": "null"},
	}}, prop(t, resp, "meta"))

	// Optional is a leaf: no component of its own.
	for name := range res.Components {
		require.NotContains(t, name, "Optional_", name)
	}
}

func TestOptionalErrors(t *testing.T) {
	r := NewReflector()
	_, err := r.AddRequest(reflect.TypeFor[fixture.OptionalParam]())
	require.Error(t, err)
	require.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	require.Contains(t, err.Error(), `fixture.OptionalParam.Q: query parameter "q" is types.Optional[string], but types.Optional is a body field; use a pointer for an optional parameter`)

	r = NewReflector()
	_, err = r.AddRequest(reflect.TypeFor[fixture.OptionalPointer]())
	require.Error(t, err)
	require.Contains(t, err.Error(), `fixture.OptionalPointer.Name: *types.Optional[string] is a pointer to types.Optional, which already records an absent key; use it by value`)

	// The key check and the lint look through Optional into T.
	r = NewReflector()
	_, err = r.AddRequest(reflect.TypeFor[fixture.OptionalBadKeys]())
	require.Error(t, err)
	require.Contains(t, err.Error(), `fixture.UnexportedEmbed: field base.Level (json "level") is bound by encoding/json but missing from the schema`)

	r = NewReflector()
	err = r.AddResponse(reflect.TypeFor[fixture.OptionalWideBound]())
	require.Error(t, err)
	require.Contains(t, err.Error(), "fixture.OptionalWideBound.N: maximum 1000 is above the int8 range")
}
