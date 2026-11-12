package openapi

import (
	"encoding/json"
	"math/big"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
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

// TestRequestKeyErrors: each type fails as a request. As a response, a type
// fails on the same comparison with the keys encoding/json sends (response
// is the expected message), except hidden fields, which a response may
// carry, and a hidden parameter, whose json:"-" field is never sent.
func TestRequestKeyErrors(t *testing.T) {
	tests := []struct {
		name     string
		typ      reflect.Type
		want     string
		response string // "" when the type is fine as a response
	}{
		{"unexported embedded struct", reflect.TypeFor[fixture.UnexportedEmbed](), `openapi: fixture.UnexportedEmbed: field base.Level (json "level") is bound by encoding/json but missing from the schema`,
			`openapi: fixture.UnexportedEmbed: field base.Level (json "level") is sent by encoding/json but missing from the schema; export the embedded struct, or declare the field on the type`},
		{"hidden field", reflect.TypeFor[fixture.HiddenField](), `openapi: fixture.HiddenField: field Secret has hidden:"true", which is not allowed on request types (encoding/json still binds it)`, ""},
		{"hidden param", reflect.TypeFor[fixture.HiddenParam](), `openapi: fixture.HiddenParam: field Q has hidden:"true", which is not supported on a parameter (the document would hide a parameter the server still reads)`, ""},
		{"embedded non-struct", reflect.TypeFor[fixture.EmbeddedSlice](), `openapi: fixture.EmbeddedSlice: field Tags (json "Tags") is bound by encoding/json but missing from the schema`,
			`openapi: fixture.EmbeddedSlice: field Tags (json "Tags") is sent by encoding/json but missing from the schema; export the embedded struct, or declare the field on the type`},
		{"shadowed Go name", reflect.TypeFor[fixture.ShadowedGoName](), `openapi: fixture.ShadowedGoName: field Inner.ID (json "a_id") is bound by encoding/json but missing from the schema`,
			`openapi: fixture.ShadowedGoName: field Inner.ID (json "a_id") is sent by encoding/json but missing from the schema; export the embedded struct, or declare the field on the type`},
		// Built at runtime: vet's structtag check rejects the literals. As a
		// response, the first documents a key never sent and the second
		// describes ConflictB.Y where Count is sent.
		{"same depth conflict", reflect.StructOf([]reflect.StructField{
			{Name: "ConflictA", Type: reflect.TypeFor[fixture.ConflictA](), Anonymous: true},
			{Name: "ConflictB", Type: reflect.TypeFor[fixture.ConflictB](), Anonymous: true},
		}), `openapi: struct { fixture.ConflictA; fixture.ConflictB }: schema property "x" is not bound by encoding/json (conflicting fields at the same depth)`,
			`openapi: struct { fixture.ConflictA; fixture.ConflictB }: schema property "x" is not sent by encoding/json (conflicting fields at the same depth)`},
		{"shadow with different Go name", reflect.StructOf([]reflect.StructField{
			{Name: "Count", Type: reflect.TypeFor[int](), Tag: `json:"x"`},
			{Name: "ConflictB", Type: reflect.TypeFor[fixture.ConflictB](), Anonymous: true},
		}), `openapi: struct { Count int "json:\"x\""; fixture.ConflictB }: json "x" is declared by Count and ConflictB.Y; rename one (only a direct field may shadow embedded fields of the same Go name)`,
			`openapi: struct { Count int "json:\"x\""; fixture.ConflictB }: json "x" is declared by Count and ConflictB.Y; rename one (only a direct field may shadow embedded fields of the same Go name)`},
		{"nested through slice", reflect.TypeFor[fixture.NestedBad](), `openapi: fixture.UnexportedEmbed: field base.Level (json "level") is bound by encoding/json but missing from the schema`,
			`openapi: fixture.UnexportedEmbed: field base.Level (json "level") is sent by encoding/json but missing from the schema; export the embedded struct, or declare the field on the type`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewReflector().AddRequest(tt.typ)
			require.Error(t, err)
			require.Equal(t, ez.EINVALID, ez.ErrorCode(err))
			require.Equal(t, tt.want, ez.ErrorMessage(err))

			err = NewReflector().AddResponse(tt.typ)
			if tt.response == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Equal(t, ez.EINVALID, ez.ErrorCode(err))
			require.Equal(t, tt.response, ez.ErrorMessage(err))
		})
	}
}

// TestResponseKeyErrors: a response type that sends a key its schema does
// not list fails at any depth; one that encodes itself is not looked into.
func TestResponseKeyErrors(t *testing.T) {
	tests := []struct {
		name string
		typ  reflect.Type
		want string
	}{
		{"unexported embedded struct", reflect.TypeFor[fixture.StampedRun](), `openapi: fixture.StampedRun: field stamps.CreatedAt (json "created_at") is sent by encoding/json but missing from the schema; export the embedded struct, or declare the field on the type`},
		{"embedded map", reflect.TypeFor[fixture.EmbeddedMap](), `openapi: fixture.EmbeddedMap: field Extras (json "Extras") is sent by encoding/json but missing from the schema; export the embedded struct, or declare the field on the type`},
		{"slice root", reflect.TypeFor[[]fixture.StampedRun](), `openapi: fixture.StampedRun: field stamps.CreatedAt (json "created_at") is sent by encoding/json but missing from the schema; export the embedded struct, or declare the field on the type`},
		{"map value of a field", reflect.TypeFor[fixture.StampedRuns](), `openapi: fixture.StampedRun: field stamps.CreatedAt (json "created_at") is sent by encoding/json but missing from the schema; export the embedded struct, or declare the field on the type`},
		{"pointer sent as null", reflect.TypeFor[nullPointer](), `openapi: openapi.nullPointer: field Owner (json "owner") is sent as null when nil, which its schema does not allow; add omitempty to its json tag (the key is then absent rather than null), or declare the field by value instead of as a pointer`},
		{"pointer sent as null through a slice", reflect.TypeFor[nullPointerList](), `openapi: openapi.nullPointer: field Owner (json "owner") is sent as null when nil, which its schema does not allow; add omitempty to its json tag (the key is then absent rather than null), or declare the field by value instead of as a pointer`},
		{"pointer sent as null through a map value", reflect.TypeFor[nullPointerMap](), `openapi: openapi.nullPointer: field Owner (json "owner") is sent as null when nil, which its schema does not allow; add omitempty to its json tag (the key is then absent rather than null), or declare the field by value instead of as a pointer`},
		{"hidden pointer sent as null", reflect.TypeFor[nullHidden](), `openapi: openapi.nullHidden: field Owner (json "owner") is sent as null when nil, which its schema does not allow; add omitempty to its json tag (the key is then absent rather than null), or declare the field by value instead of as a pointer`},
		{"non-null scalar pointer sent as null", reflect.TypeFor[nullScalar](), `openapi: openapi.nullScalar: field Note (json "note") is sent as null when nil, which its schema does not allow; add omitempty to its json tag (the key is then absent rather than null), or declare the field by value instead of as a pointer`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := NewReflector().AddResponse(tt.typ)
			require.Error(t, err)
			require.Equal(t, ez.EINVALID, ez.ErrorCode(err))
			require.Equal(t, tt.want, ez.ErrorMessage(err))
		})
	}

	for _, typ := range []reflect.Type{reflect.TypeFor[fixture.JSONStampedRun](), reflect.TypeFor[fixture.TextStampedRun]()} {
		err := NewReflector().AddResponse(typ)
		require.NoError(t, err, typ.String())
	}
}

// pcBase decodes itself.
type pcBase struct {
	A string `json:"a"`
}

func (b *pcBase) UnmarshalJSON(raw []byte) error {
	return nil
}

// pcRequest gets pcBase's decoder by embedding it.
type pcRequest struct {
	pcBase
	Name string `json:"name" maxLength:"3"`
}

// pcNested reaches pcRequest through a slice.
type pcNested struct {
	Items []pcRequest `json:"items"`
}

// pcStamp encodes itself.
type pcStamp struct{}

func (pcStamp) MarshalJSON() ([]byte, error) {
	return []byte(`"stamp"`), nil
}

// pcJSONResponse gets pcStamp's encoder by embedding it.
type pcJSONResponse struct {
	pcStamp
	Name string `json:"name"`
}

// pcLabel encodes itself as text and has no text decoder, which would make
// it a string.
type pcLabel struct {
	V string
}

func (l pcLabel) MarshalText() ([]byte, error) {
	return []byte(l.V), nil
}

// pcTextResponse gets pcLabel's encoder by embedding it.
type pcTextResponse struct {
	*pcLabel
	Name string `json:"name"`
}

// pcNamed holds the self-coding types as named fields.
type pcNamed struct {
	Base  pcBase  `json:"base"`
	Stamp pcStamp `json:"stamp"`
	Name  string  `json:"name"`
}

// pcOwn declares its own decoder.
type pcOwn struct {
	Name string `json:"name"`
}

func (o *pcOwn) UnmarshalJSON(raw []byte) error {
	return nil
}

// TestPromotedCoder: a struct documented by its fields that gets a JSON
// decoder (request) or encoder (response) from an embedded field is refused
// at any depth: encoding/json would hand it the whole value. One that holds
// such a type as a named field, or declares its own, is not looked into.
func TestPromotedCoder(t *testing.T) {
	tests := []struct {
		name    string
		typ     reflect.Type
		request bool
		want    string
	}{
		{"request", reflect.TypeFor[pcRequest](), true, `openapi: openapi.pcRequest: the JSON decoder of embedded field pcBase would take over the whole value, not the fields the schema lists; make it a named field`},
		{"nested request", reflect.TypeFor[pcNested](), true, `openapi: openapi.pcRequest: the JSON decoder of embedded field pcBase would take over the whole value, not the fields the schema lists; make it a named field`},
		{"json response", reflect.TypeFor[pcJSONResponse](), false, `openapi: openapi.pcJSONResponse: the JSON encoder of embedded field pcStamp would take over the whole value, not the fields the schema lists; make it a named field`},
		{"text response", reflect.TypeFor[pcTextResponse](), false, `openapi: openapi.pcTextResponse: the JSON encoder of embedded field pcLabel would take over the whole value, not the fields the schema lists; make it a named field`},
		{"nested response", reflect.TypeFor[map[string][]pcJSONResponse](), false, `openapi: openapi.pcJSONResponse: the JSON encoder of embedded field pcStamp would take over the whole value, not the fields the schema lists; make it a named field`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var err error
			if tt.request {
				_, err = NewReflector().AddRequest(tt.typ)
			} else {
				err = NewReflector().AddResponse(tt.typ)
			}
			require.Error(t, err)
			require.Equal(t, ez.EINVALID, ez.ErrorCode(err))
			require.Equal(t, tt.want, ez.ErrorMessage(err))
		})
	}

	for _, typ := range []reflect.Type{reflect.TypeFor[pcNamed](), reflect.TypeFor[pcOwn]()} {
		_, err := NewReflector().AddRequest(typ)
		require.NoError(t, err, typ.String())
		err = NewReflector().AddResponse(typ)
		require.NoError(t, err, typ.String())
	}
}

type nullOwner struct {
	Name string `json:"name"`
}

// nullPointer sends null for a nil Owner, which the schema of a required
// object property does not allow.
type nullPointer struct {
	Owner *nullOwner `json:"owner"`
}

type nullPointerList struct {
	Items []nullPointer `json:"items"`
}

type nullPointerMap struct {
	ByID map[string]nullPointer `json:"by_id"`
}

type nullHidden struct {
	Owner *nullOwner `json:"owner" hidden:"true"`
}

// nullScalar documents a pointer to a scalar as non-null.
type nullScalar struct {
	Note *string `json:"note" nullable:"false"`
}

// nullSelfCoded encodes itself, so its fields are not looked into.
type nullSelfCoded struct {
	Owner *nullOwner `json:"owner"`
}

func (nullSelfCoded) MarshalJSON() ([]byte, error) {
	return []byte(`{}`), nil
}

// nullAllowed holds pointers that send no null their schema refuses.
type nullAllowed struct {
	OmitEmpty *nullOwner       `json:"omit_empty,omitempty"`
	OmitZero  *nullOwner       `json:"omit_zero,omitzero"`
	Name      *string          `json:"name"`
	At        *time.Time       `json:"at"`
	Mode      *fixture.Mode    `json:"mode"`
	Raw       *json.RawMessage `json:"raw"`
	ID        *uuid.UUID       `json:"id"`
	IP        *net.IP          `json:"ip"`
	Value     nullOwner        `json:"value"`
	Coded     nullSelfCoded    `json:"coded"`
}

// TestSentNullAllowed: a response pointer field passes when it is absent
// rather than null when nil, when its schema allows null, or when it is held
// by value; a request pointer field accepts null and is never refused.
func TestSentNullAllowed(t *testing.T) {
	r := NewReflector()
	err := r.AddResponse(reflect.TypeFor[nullAllowed]())
	require.NoError(t, err)
	_, err = r.AddRequest(reflect.TypeFor[nullPointer]())
	require.NoError(t, err)

	// A builtin behind a pointer is nullable whatever its kind: uuid.UUID
	// is an array and net.IP a slice.
	res, err := r.Finalize()
	require.NoError(t, err)
	s := component(t, res, "openapi.nullAllowed")
	require.Equal(t, map[string]any{"type": []any{"string", "null"}, "format": "uuid"}, prop(t, s, "id"))
	require.Equal(t, map[string]any{"type": []any{"string", "null"}, "format": "ip"}, prop(t, s, "ip"))
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

type stdlibShapes struct {
	Number json.Number   `json:"number"`
	IP     net.IP        `json:"ip"`
	Big    *big.Int      `json:"big"`
	NullID uuid.NullUUID `json:"null_id"`
	Bytes  []byte        `json:"bytes"`
	Hash   [2]byte       `json:"hash"`
	Err    error         `json:"err"`
}

// TestStdlibShapes: standard library types are documented in the shape
// encoding/json reads and writes. A response may hold an error, which the
// schema leaves open.
func TestStdlibShapes(t *testing.T) {
	r := NewReflector()
	err := r.AddResponse(reflect.TypeFor[stdlibShapes]())
	require.NoError(t, err)
	res, err := r.Finalize()
	require.NoError(t, err)

	s := component(t, res, "openapi.stdlibShapes")
	require.Equal(t, map[string]any{"type": "number"}, prop(t, s, "number"))
	require.Equal(t, map[string]any{"type": "string", "format": "ip"}, prop(t, s, "ip"))
	require.Equal(t, map[string]any{"type": []any{"integer", "null"}}, prop(t, s, "big"))
	require.Equal(t, map[string]any{"type": []any{"string", "null"}, "format": "uuid"}, prop(t, s, "null_id"))
	require.Equal(t, map[string]any{"type": []any{"string", "null"}, "contentEncoding": "base64"}, prop(t, s, "bytes"))
	require.Equal(t, map[string]any{
		"type":     "array",
		"items":    map[string]any{"type": "integer", "format": "int32", "minimum": 0.0, "maximum": 255.0},
		"minItems": 2.0,
		"maxItems": 2.0,
	}, prop(t, s, "hash"))
	require.Equal(t, map[string]any{}, prop(t, s, "err"))

	// What encoding/json writes for the zero value matches each schema's
	// type. The nil IP is written as "", which the ip format refuses, so an
	// apitest contract fails a response that sends it.
	raw, err := json.Marshal(&stdlibShapes{})
	require.NoError(t, err)
	require.JSONEq(t, `{"number":0,"ip":"","big":null,"null_id":null,"bytes":null,"hash":[0,0],"err":null}`, string(raw))
}

// A response documents a default; only a request type is refused the tag.
func TestDefaultResponseAllowed(t *testing.T) {
	r := NewReflector()
	err := r.AddResponse(reflect.TypeFor[fixture.DefaultTag]())
	require.NoError(t, err)
	res, err := r.Finalize()
	require.NoError(t, err)

	s := component(t, res, "fixture.DefaultTag")
	limit := s["properties"].(map[string]any)["limit"].(map[string]any)
	require.EqualValues(t, 20, limit["default"])
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

// TestMapNullability: a nil map is sent as null like a nil slice, so each
// map field is exactly as nullable as the slice field declared under the
// same rule, in a response and in a request.
func TestMapNullability(t *testing.T) {
	r := NewReflector()
	err := r.AddResponse(reflect.TypeFor[fixture.Collections]())
	require.NoError(t, err)
	res, err := r.Finalize()
	require.NoError(t, err)
	response := component(t, res, "fixture.Collections")

	r = NewReflector()
	_, err = r.AddRequest(reflect.TypeFor[fixture.Collections]())
	require.NoError(t, err)
	res, err = r.Finalize()
	require.NoError(t, err)
	request := component(t, res, "fixture.Collections")

	pairs := [][2]string{{"m", "s"}, {"om", "os"}, {"zm", "zs"}, {"pm", "ps"}, {"pom", "pos"}, {"nm", "ns"}, {"rm", "rs"}, {"xm", "xs"}}
	for _, s := range []map[string]any{response, request} {
		for _, pair := range pairs {
			slice, err := json.Marshal(prop(t, s, pair[1])["type"])
			require.NoError(t, err)
			got, err := json.Marshal(prop(t, s, pair[0])["type"])
			require.NoError(t, err)
			require.Equal(t, strings.Replace(string(slice), `"array"`, `"object"`, 1), string(got), pair[0])
		}
	}

	require.Equal(t, []any{"object", "null"}, prop(t, response, "m")["type"])
	require.Equal(t, []any{"object", "null"}, prop(t, response, "om")["type"])
	require.Equal(t, "object", prop(t, response, "pom")["type"])
	require.Equal(t, "object", prop(t, response, "nm")["type"])
	require.Equal(t, "object", prop(t, request, "rm")["type"])
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
	require.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	require.True(t, strings.HasPrefix(ez.ErrorMessage(err), "openapi: duplicate component name units.ListRequest"), ez.ErrorMessage(err))
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
