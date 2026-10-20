package openapi

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/require"
	"github.com/vanclief/ez"

	"github.com/vanclief/compose/components/rest/api/openapi/internal/fixture"
	aunits "github.com/vanclief/compose/components/rest/api/openapi/internal/fixture/a/units"
	bunits "github.com/vanclief/compose/components/rest/api/openapi/internal/fixture/b/units"
)

type jkPlain struct {
	A int
	b int
	C int `json:"-"`
	D int `json:"-,"`
	E int `json:"e"`
	F int `json:"a\\b"`
}

type jkInner struct {
	X int `json:"x"`
	Y int
}

type JKExported struct {
	Q int
}

type (
	JKTags []string
	jkTags []string
)

type jkTagged struct {
	Bar int `json:"Name"`
}

type jkUntagged struct {
	Name int
}

type jkU1 struct {
	Z int
}

type jkU2 struct {
	Z int
}

type JKT1 struct {
	A int `json:"k"`
}

type JKT2 struct {
	B int `json:"k"`
}

type jkP1 struct {
	JKExported
}

type jkP2 struct {
	JKExported
}

type jkRec struct {
	*jkRec
	V int
}

func TestJSONFields(t *testing.T) {
	tests := []struct {
		name      string
		typ       reflect.Type
		want      map[string]string // JSON key -> Go path
		noMarshal bool              // nil embedded pointers hide keys from Marshal
	}{
		{"exported, dash, invalid tag", reflect.TypeFor[jkPlain](), map[string]string{"-": "D", "A": "A", "F": "F", "e": "E"}, false},
		{"unexported embedded struct promotes", reflect.TypeFor[struct {
			jkInner
			N int
		}](), map[string]string{"x": "jkInner.X", "Y": "jkInner.Y", "N": "N"}, false},
		{"embedded pointer struct promotes", reflect.TypeFor[struct{ *JKExported }](), map[string]string{"Q": "JKExported.Q"}, true},
		{"embedded struct with json name", reflect.TypeFor[struct {
			JKExported `json:"ex"`
		}](), map[string]string{"ex": "JKExported"}, false},
		{"embedded non-struct", reflect.TypeFor[struct {
			jkTags
			JKTags
		}](), map[string]string{"JKTags": "JKTags"}, false},
		{"shallower wins", reflect.TypeFor[struct {
			X int `json:"x"`
			jkInner
		}](), map[string]string{"x": "X", "Y": "jkInner.Y"}, false},
		{"tagged beats untagged at same depth", reflect.TypeFor[struct {
			jkUntagged
			jkTagged
		}](), map[string]string{"Name": "jkTagged.Bar"}, false},
		{"same depth untagged both dropped", reflect.TypeFor[struct {
			jkU1
			jkU2
		}](), map[string]string{}, false},
		// Built at runtime: vet's structtag check rejects the literal.
		{"same depth tagged both dropped", reflect.StructOf([]reflect.StructField{
			{Name: "JKT1", Type: reflect.TypeFor[JKT1](), Anonymous: true},
			{Name: "JKT2", Type: reflect.TypeFor[JKT2](), Anonymous: true},
		}), map[string]string{}, false},
		{"same type twice at same depth dropped", reflect.TypeFor[struct {
			jkP1
			jkP2
		}](), map[string]string{}, false},
		{"recursive embedding", reflect.TypeFor[jkRec](), map[string]string{"V": "V"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fields, _ := jsonFields(tt.typ)
			got := map[string]string{}
			for _, f := range fields {
				got[f.name] = f.path
			}
			require.Equal(t, tt.want, got)

			if tt.noMarshal {
				return
			}
			// encoding/json encodes with the same field rules.
			raw, err := json.Marshal(reflect.New(tt.typ).Elem().Interface())
			require.NoError(t, err)
			var decoded map[string]any
			err = json.Unmarshal(raw, &decoded)
			require.NoError(t, err)
			require.ElementsMatch(t, sortedKeys(tt.want), sortedKeys(decoded))
		})
	}
}

func TestRequestKeyErrors(t *testing.T) {
	tests := []struct {
		name string
		typ  reflect.Type
		want string
	}{
		{"unexported embedded struct", reflect.TypeFor[fixture.UnexportedEmbed](), `openapi: fixture.UnexportedEmbed: field base.Level (json "level") is bound by encoding/json but missing from the schema`},
		{"hidden field", reflect.TypeFor[fixture.HiddenField](), `openapi: fixture.HiddenField: field Secret has hidden:"true", which is not allowed on request types (encoding/json still binds it)`},
		{"hidden param", reflect.TypeFor[fixture.HiddenParam](), `openapi: fixture.HiddenParam: field Q has hidden:"true", which is not allowed on request types (encoding/json still binds it)`},
		{"embedded non-struct", reflect.TypeFor[fixture.EmbeddedSlice](), `openapi: fixture.EmbeddedSlice: field Tags (json "Tags") is bound by encoding/json but missing from the schema`},
		{"shadowed Go name", reflect.TypeFor[fixture.ShadowedGoName](), `openapi: fixture.ShadowedGoName: field Inner.ID (json "a_id") is bound by encoding/json but missing from the schema`},
		{"same depth conflict", reflect.TypeFor[fixture.SameDepthConflict](), `openapi: fixture.SameDepthConflict: schema property "x" is not bound by encoding/json (conflicting fields at the same depth)`},
		{"shadow with different Go name", reflect.TypeFor[fixture.ShadowOuter](), `openapi: fixture.ShadowOuter: json "x" is declared by Count and ConflictB.Y; rename one (only a direct field may shadow embedded fields of the same Go name)`},
		{"nested through slice", reflect.TypeFor[fixture.NestedBad](), `openapi: fixture.UnexportedEmbed: field base.Level (json "level") is bound by encoding/json but missing from the schema`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewReflector().AddRequest(tt.typ)
			require.Error(t, err)
			require.Equal(t, ez.EINVALID, ez.ErrorCode(err))
			require.Equal(t, tt.want, ez.ErrorMessage(err))

			// The same types are fine as responses.
			err = NewReflector().AddResponse(tt.typ)
			require.NoError(t, err)
		})
	}
}

func TestHiddenResponseAllowed(t *testing.T) {
	r := NewReflector()
	err := r.AddResponse(reflect.TypeFor[fixture.HiddenResponse]())
	require.NoError(t, err)
	res, err := r.Finalize()
	require.NoError(t, err)

	// The hidden pointer enum keeps hidden after the anyOf-null rewrite.
	s := component(t, res, "fixture.HiddenResponse")
	require.Equal(t, map[string]any{"name": map[string]any{"type": "string"}}, s["properties"])
	require.Equal(t, []any{"name"}, s["required"])
}

func nullOf(name string) map[string]any {
	return map[string]any{"anyOf": []any{map[string]any{"$ref": ref(name)}, map[string]any{"type": "null"}}}
}

func TestNullability(t *testing.T) {
	res, _ := buildFixture(t)
	flags := component(t, res, "fixture.Flags")

	require.Equal(t, map[string]any{"$ref": ref("fixture.Mode")}, prop(t, flags, "mode"))
	require.Equal(t, nullOf("fixture.Mode"), prop(t, flags, "pmode"))
	require.Equal(t, map[string]any{"$ref": ref("fixture.Mode")}, prop(t, flags, "omode"))
	require.Equal(t, nullOf("fixture.Mode"), prop(t, flags, "nmode"))
	require.Equal(t, map[string]any{"type": []any{"array", "null"}, "items": nullOf("fixture.Mode")}, prop(t, flags, "pmodes"))
	require.Equal(t, map[string]any{"type": []any{"string", "null"}, "enum": []any{"x", "y", nil}}, prop(t, flags, "choice"))

	r := NewReflector()
	params, err := r.AddRequest(reflect.TypeFor[fixture.FlagsRequest]())
	require.NoError(t, err)
	res, err = r.Finalize()
	require.NoError(t, err)

	req := component(t, res, "fixture.FlagsRequest")
	require.Equal(t, []any{"must", "must_mode"}, req["required"])
	require.Equal(t, map[string]any{"type": []any{"string", "null"}, "enum": []any{"a", "b", nil}}, prop(t, req, "choice"))
	require.Equal(t, map[string]any{"type": "integer", "format": "int64"}, prop(t, req, "must"))
	require.Equal(t, map[string]any{"$ref": ref("fixture.Mode")}, prop(t, req, "must_mode"))
	require.Equal(t, nullOf("fixture.Mode"), prop(t, req, "pmode"))

	require.Len(t, params, 1)
	require.Equal(t, map[string]any{"type": "integer", "format": "int32", "minimum": -2147483648.0, "maximum": 2147483647.0}, toMap(t, params[0].Schema))
	require.Equal(t, map[string]any{
		"type":  []any{"array", "null"},
		"items": map[string]any{"type": "integer", "format": "int32", "minimum": -32768.0, "maximum": 32767.0},
	}, prop(t, req, "levels"))
}

func TestSizedInts(t *testing.T) {
	res, _ := buildFixture(t)
	flags := component(t, res, "fixture.Flags")
	require.Equal(t, map[string]any{"type": "integer", "format": "int32", "minimum": -2147483648.0, "maximum": 2147483647.0}, prop(t, flags, "small"))
	require.Equal(t, map[string]any{"type": "integer", "format": "int32", "minimum": 0.0, "maximum": 100.0}, prop(t, flags, "tiny"))
	require.Equal(t, map[string]any{"type": "integer", "format": "int64", "minimum": 0.0, "maximum": 4294967295.0}, prop(t, flags, "big"))
	require.Equal(t, map[string]any{"type": "integer", "format": "int64", "minimum": 0.0}, prop(t, flags, "count"))
	require.Equal(t, map[string]any{"type": "integer", "format": "int64"}, prop(t, flags, "wide"))
}

// TestValidatorProbe compiles the published components with santhosh and
// checks null handling end to end.
func TestValidatorProbe(t *testing.T) {
	r := NewReflector()
	_, err := r.AddRequest(reflect.TypeFor[fixture.FlagsRequest]())
	require.NoError(t, err)
	_, err = r.AddRequest(reflect.TypeFor[fixture.UpdateRequest]())
	require.NoError(t, err)
	res, err := r.Finalize()
	require.NoError(t, err)

	doc := map[string]any{"components": map[string]any{"schemas": toMap(t, res.Components)}}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	err = c.AddResource("spec.json", doc)
	require.NoError(t, err)

	tests := []struct {
		schema string
		body   string
		ok     bool
	}{
		{"fixture.FlagsRequest", `{"must":1,"must_mode":"a"}`, true},
		{"fixture.FlagsRequest", `{"must":1,"must_mode":"a","choice":null,"pmode":null}`, true},
		{"fixture.FlagsRequest", `{"must":1,"must_mode":"a","choice":"z"}`, false},
		{"fixture.FlagsRequest", `{"must":1,"must_mode":"a","pmode":"z"}`, false},
		{"fixture.FlagsRequest", `{"must":null,"must_mode":"a"}`, false},
		{"fixture.FlagsRequest", `{"must":1,"must_mode":null}`, false},
		{"fixture.FlagsRequest", `{"must":1,"must_mode":"a","levels":[40000]}`, false},
		{"fixture.UpdateRequest", `{"status":null}`, true},
		{"fixture.UpdateRequest", `{"status":"failed"}`, true},
		{"fixture.UpdateRequest", `{"status":"pending"}`, false},
	}
	for _, tt := range tests {
		schema, err := c.Compile("spec.json#/components/schemas/" + tt.schema)
		require.NoError(t, err)
		inst, err := jsonschema.UnmarshalJSON(strings.NewReader(tt.body))
		require.NoError(t, err)
		err = schema.Validate(inst)
		if tt.ok {
			require.NoError(t, err, tt.body)
			continue
		}
		require.Error(t, err, tt.body)
	}
}

func TestNamer(t *testing.T) {
	r := NewReflector()
	_, err := r.AddRequest(reflect.TypeFor[aunits.ListRequest]())
	require.NoError(t, err)
	_, err = r.AddRequest(reflect.TypeFor[bunits.ListRequest]())
	require.Error(t, err)
	require.Contains(t, err.Error(), "duplicate component name units.ListRequest")
	require.Contains(t, err.Error(), "openapi.WithNamer")

	r = NewReflector(WithNamer(SegmentsNamer(2)))
	_, err = r.AddRequest(reflect.TypeFor[aunits.ListRequest]())
	require.NoError(t, err)
	_, err = r.AddRequest(reflect.TypeFor[bunits.ListRequest]())
	require.NoError(t, err)
	res, err := r.Finalize()
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"a.units.ListRequest", "b.units.ListRequest"}, sortedKeys(res.Components))

	r = NewReflector(WithNamer(func(t reflect.Type) string {
		return "x." + t.Name()
	}))
	err = r.AddResponse(reflect.TypeFor[fixture.Run]())
	require.NoError(t, err)
	res, err = r.Finalize()
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"x.Config", "x.Meta", "x.Run", "x.RunStatus"}, sortedKeys(res.Components))

	require.Equal(t, "internal.fixture.Page_a.units.ListRequest", SegmentsNamer(2)(reflect.TypeFor[fixture.Page[aunits.ListRequest]]()))
	require.Equal(t, "fixture.Page_units.ListRequest", SegmentsNamer(0)(reflect.TypeFor[fixture.Page[aunits.ListRequest]]()))
	require.Equal(t, "string", DefaultNamer(reflect.TypeFor[string]()))
}
