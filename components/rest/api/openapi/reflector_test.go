package openapi

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"net/netip"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/vanclief/ez"

	"github.com/vanclief/compose/components/rest/api/internal/huma"
	"github.com/vanclief/compose/components/rest/api/openapi/internal/fixture"
	"github.com/vanclief/compose/types"
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

type urlField struct {
	Callback *url.URL `json:"callback"`
}

type urlList struct {
	Links []url.URL `json:"links"`
}

type errorField struct {
	Err error `json:"err"`
}

type wideByteArray struct {
	B [2]byte `json:"b" maximum:"300"`
}

type nonNullPointer struct {
	Note *string `json:"note,omitempty" nullable:"false"`
}

// nonNullPointerHolder reaches nonNullPointer through a field.
type nonNullPointerHolder struct {
	Inner nonNullPointer `json:"inner"`
}

type requiredNonNullPointer struct {
	Note *string `json:"note" required:"true" nullable:"false"`
}

type requiredNullablePointer struct {
	Note *string `json:"note" required:"true" nullable:"true"`
}

// requiredNullablePointerList reaches requiredNullablePointer through a
// slice.
type requiredNullablePointerList struct {
	Items []requiredNullablePointer `json:"items"`
}

type optionalNullablePointer struct {
	Note *string `json:"note" nullable:"true"`
}

// EmbeddedNote is a struct embedded with its fields promoted.
type EmbeddedNote struct {
	Note string `json:"note"`
}

// nullableEmbedded tags an embedded struct nullable.
type nullableEmbedded struct {
	EmbeddedNote `nullable:"true"`
}

// plainEmbedded embeds a struct with no nullable tag.
type plainEmbedded struct {
	EmbeddedNote
}

// prefixMinLengthZero sets minLength 0 on a required netip.Prefix.
type prefixMinLengthZero struct {
	A netip.Prefix `json:"a" required:"true" minLength:"0"`
}

// prefixListMinLengthZero sets minLength 0 on the items of a query list of
// netip.Prefix.
type prefixListMinLengthZero struct {
	L []netip.Prefix `json:"-" query:"l" minLength:"0"`
}

// optionalAddrPortMinLength sets a negative minLength on a
// types.Optional[netip.AddrPort].
type optionalAddrPortMinLength struct {
	A types.Optional[netip.AddrPort] `json:"a" minLength:"-1"`
}

// prefixMapMinLength sets minLength 0 on a map of netip.Prefix values.
type prefixMapMinLength struct {
	M map[string]netip.Prefix `json:"m" minLength:"0"`
}

// prefixMinLengthTwo sets minLength 2 on a netip.Prefix, which keeps ""
// refused.
type prefixMinLengthTwo struct {
	A *netip.Prefix `json:"a" minLength:"2"`
}

// blankAdditionalProperties gives the blank field an empty
// additionalProperties tag.
type blankAdditionalProperties struct {
	_    struct{} `additionalProperties:""`
	Name string   `json:"name"`
}

// nullableStructPointer tags a pointer to a struct nullable.
type nullableStructPointer struct {
	Owner *nullOwner `json:"owner,omitempty" nullable:"true"`
}

// structPointer is a pointer to a struct with no nullable tag.
type structPointer struct {
	Owner *nullOwner `json:"owner,omitempty"`
}

// emptyRequired puts a required tag with no value on a body field.
type emptyRequired struct {
	Name string `json:"name" required:""`
}

// looseRequired puts a required tag that is not exactly true on a body
// field.
type looseRequired struct {
	Name string `json:"name" required:"True"`
}

// emptyNullable puts a nullable tag with no value on a body field.
type emptyNullable struct {
	Note *string `json:"note" nullable:""`
}

// optionalPrefixItems sets minLength 0 on the items of a slice of
// types.Optional[netip.Prefix].
type optionalPrefixItems struct {
	A []types.Optional[netip.Prefix] `json:"a" minLength:"0"`
}

// nullableOptionalStruct tags a types.Optional of a struct nullable.
type nullableOptionalStruct struct {
	Owner types.Optional[nullOwner] `json:"owner,omitzero" nullable:"true"`
}

// nullableOptionalStructPointer tags a types.Optional of a struct pointer
// nullable.
type nullableOptionalStructPointer struct {
	Owner types.Optional[*nullOwner] `json:"owner,omitzero" nullable:"true"`
}

// nullableIgnoredStruct tags a struct field that is not read from the body
// nullable.
type nullableIgnoredStruct struct {
	Owner nullOwner `json:"-" nullable:"true"`
}

// nullableIgnoredEmbedded tags an embedded struct that is not read from the
// body nullable.
type nullableIgnoredEmbedded struct {
	EmbeddedNote `json:"-" nullable:"true"`
}

// nullableFormStruct tags a struct form field nullable.
type nullableFormStruct struct {
	Meta nullOwner `json:"-" form:"meta" nullable:"true"`
}

// nullableQueryStruct tags a struct query parameter nullable.
type nullableQueryStruct struct {
	Owner nullOwner `json:"-" query:"owner" nullable:"true"`
}

// nullableHeaderStruct tags a struct pointer header parameter nullable.
type nullableHeaderStruct struct {
	Owner *nullOwner `json:"-" header:"X-Owner" nullable:"true"`
}

// looseEmbedded puts boolean tags that are not exactly true or false on an
// embedded struct.
type looseEmbedded struct {
	EmbeddedNote `nullable:"True" required:"yes" hidden:"1"`
}

// emptyEmbeddedPointer puts empty boolean tags on an embedded struct
// pointer.
type emptyEmbeddedPointer struct {
	*EmbeddedNote `required:"" nullable:""`
}

// optionalPath tags a path parameter required:"false".
type optionalPath struct {
	ID string `json:"-" path:"id" required:"false"`
}

type nullableString struct {
	Name string `json:"name" required:"true" minLength:"3" nullable:"true"`
}

type nullableInt struct {
	Count int `json:"count" minimum:"1" nullable:"true"`
}

type nullableUUID struct {
	ID uuid.UUID `json:"id" nullable:"true"`
}

// nullableTimeHolder reaches nullableTime through a slice.
type nullableTimeHolder struct {
	Items []nullableTime `json:"items"`
}

type nullableTime struct {
	At time.Time `json:"at" nullable:"true"`
}

// nullableHolders carries nullable:"true" on every body field that holds
// null, and on a parameter, which is no body field.
type nullableHolders struct {
	Note   *string                `json:"note" nullable:"true"`
	Tags   []string               `json:"tags" nullable:"true"`
	Labels map[string]string      `json:"labels" nullable:"true"`
	Title  types.Optional[string] `json:"title" nullable:"true"`
	Owner  uuid.NullUUID          `json:"owner" nullable:"true"`
	Q      string                 `json:"-" query:"q" nullable:"true"`
}

type exclusiveFloat struct {
	Rate float64 `json:"rate" exclusiveMinimum:"0"`
}

type exclusiveFloatParam struct {
	Max *float32 `json:"-" query:"max" exclusiveMaximum:"1"`
}

type exclusiveFloatItems struct {
	Weights types.Optional[[]float64] `json:"weights" exclusiveMinimum:"0"`
}

// floatBoundsAllowed carries the bounds that hold through a float's
// rounding, and exclusive bounds where nothing is rounded.
type floatBoundsAllowed struct {
	Rate   float64     `json:"rate" minimum:"0" maximum:"1"`
	Amount json.Number `json:"amount" exclusiveMinimum:"0"`
	Count  int         `json:"count" exclusiveMinimum:"0" exclusiveMaximum:"10"`
}

// fixedArray: a [N]T is never null, required or not, in a request and in a
// response; a pointer to one is.
type fixedArray struct {
	Codes [3]int    `json:"codes" required:"true" minimum:"1"`
	Tags  [2]string `json:"tags"`
	Maybe *[2]int   `json:"maybe"`
}

// arrayText is a text type of array kind.
type arrayText [4]byte

func (a arrayText) MarshalText() ([]byte, error) {
	return []byte(hex.EncodeToString(a[:])), nil
}

func (a *arrayText) UnmarshalText(text []byte) error {
	_, err := hex.Decode(a[:], text)
	return err
}

// pointerArrays: a nil pointer to a fixed-size array, or to a text type of
// array kind, is written as null, so both are documented nullable and a
// response may hold them without omitempty.
type pointerArrays struct {
	Codes *[3]int    `json:"codes"`
	Hash  *arrayText `json:"hash"`
}

type nullableBlank struct {
	_     struct{} `nullable:"true"`
	Level int      `json:"level" required:"true" minimum:"1"`
}

// nullableBlankHolder reaches nullableBlank through a field.
type nullableBlankHolder struct {
	Inner nullableBlank `json:"inner" required:"true"`
}

// paging holds parameters and is embedded by value, unexported.
type paging struct {
	Limit int     `json:"-" query:"limit" default:"10" minimun:"1"`
	Ratio float64 `json:"-" query:"ratio" exclusiveMinimum:"1"`
}

type unexportedEmbedParams struct {
	paging
}

// Paging is embedded with json:"-".
type Paging struct {
	IDs []uuid.UUID `json:"-" query:"ids" uniqueItems:"true"`
}

type ignoredEmbedParams struct {
	Paging `json:"-"`
}

type boundOnMap struct {
	Scores map[string]int `json:"scores" maximum:"10"`
}

type lengthOnMap struct {
	Labels map[string]string `json:"labels" maxLength:"3"`
}

type boundOnStruct struct {
	Inner ShadowedName `json:"inner" maximum:"3"`
}

type boundOnGrid struct {
	Grid [][]int `json:"grid" maximum:"10"`
}

type lengthOnInt struct {
	N *int `json:"n" minLength:"2"`
}

type itemsOnString struct {
	Name types.Optional[string] `json:"name" minItems:"1"`
}

type boundOnTime struct {
	At time.Time `json:"-" query:"at" minimum:"0"`
}

// placedTags carries every bound, length and item tag on a schema of the
// type it constrains.
type placedTags struct {
	Count  *int                   `json:"count" minimum:"1" maximum:"9" multipleOf:"2"`
	Rates  []float64              `json:"rates" minimum:"0" maxItems:"3"`
	Amount json.Number            `json:"amount" exclusiveMinimum:"0"`
	At     types.UnixSeconds      `json:"at" minimum:"0"`
	Name   types.Optional[string] `json:"name" minLength:"1" pattern:"^[a-z]+$"`
	Names  []string               `json:"names" maxLength:"8" minItems:"1" uniqueItems:"true"`
	Blob   []byte                 `json:"blob" maxLength:"64"`
	ID     uuid.UUID              `json:"id" maxLength:"36"`
	Hash   [4]byte                `json:"hash" maximum:"200" uniqueItems:"true"`
	Limit  int                    `json:"-" query:"limit" maximum:"100"`
}

type itemsOnFixedArray struct {
	Codes [3]int `json:"codes" minItems:"1" minimum:"1"`
}

type maxItemsOnFixedArray struct {
	Codes types.Optional[[3]int] `json:"codes" maxItems:"5"`
}

type nonNullOptionalStruct struct {
	Inner types.Optional[ShadowedName] `json:"inner" nullable:"false"`
}

type nonNullAny struct {
	Value any `json:"value" nullable:"false"`
}

type nonNullRaw struct {
	Value json.RawMessage `json:"value" nullable:"false"`
}

// nonNullHonoured carries nullable:"false" where the schema obeys it.
type nonNullHonoured struct {
	Name  types.Optional[string]       `json:"name" nullable:"false"`
	Mode  types.Optional[fixture.Mode] `json:"mode" nullable:"false"`
	Tags  []string                     `json:"tags" nullable:"false"`
	Inner ShadowedName                 `json:"inner" nullable:"false"`
}

type unexportedParam struct {
	projectID string `json:"-" query:"project_id" required:"true"`
}

type EmbeddedToken int

type embeddedFieldParam struct {
	EmbeddedToken `json:"-" query:"token" required:"true"`
}

type boundOnAny struct {
	Value any `json:"value" maximum:"5"`
}

type lengthOnRaw struct {
	Meta json.RawMessage `json:"meta" maxLength:"5"`
}

type itemsOnAny struct {
	Value types.Optional[any] `json:"value" maxItems:"2"`
}

type ShadowPaging struct {
	Limit int `json:"-" query:"limit" maximum:"10"`
}

type OtherPaging struct {
	Limit int `json:"-" header:"x-limit"`
}

type shadowedParam struct {
	ShadowPaging
	Limit int `json:"limit"`
}

type ambiguousParam struct {
	ShadowPaging
	OtherPaging
}

type nullableAny struct {
	Data any `json:"data" nullable:"true"`
}

type nullableOptionalAny struct {
	Data types.Optional[any] `json:"data" nullable:"true"`
}

// NamedPaging is embedded with a JSON name, so it is a body object.
type NamedPaging struct {
	Limit int    `json:"-" query:"limit"`
	Note  string `json:"note" required:"true" minLength:"3"`
}

type namedEmbedParam struct {
	*NamedPaging `json:"paging"`
	Name         string `json:"name"`
}

type namedValueEmbedParam struct {
	NamedPaging `json:"paging"`
}

// anonymousItems holds three anonymous structs under the hint "Item": the
// second is numbered, and the third is the second's type again.
type anonymousItems struct {
	A []struct {
		X int `json:"x"`
	} `json:"a"`
	B []struct {
		Y string `json:"y" required:"true" maxLength:"3"`
	} `json:"b"`
	C []struct {
		Y string `json:"y" required:"true" maxLength:"3"`
	} `json:"c"`
}

type bigByValueInMap struct {
	Totals map[string]big.Int `json:"totals"`
}

// bigNested reaches the map value through a struct held by value.
type bigNested struct {
	Rows map[string]bigHolder `json:"rows"`
}

type bigHolder struct {
	N big.Int `json:"n"`
}

// bigAddressable holds a big.Int everywhere encoding/json can take its
// address.
type bigAddressable struct {
	N      big.Int                  `json:"n"`
	List   []big.Int                `json:"list"`
	Pair   [2]big.Int               `json:"pair"`
	Maybe  types.Optional[big.Int]  `json:"maybe"`
	Totals map[string]*big.Int      `json:"totals"`
	Lists  map[string][]big.Int     `json:"lists"`
	Rows   map[string]*bigHolder    `json:"rows"`
	At     map[string]time.Time     `json:"at"`
	IDs    map[string]uuid.NullUUID `json:"ids"`
}

// IgnoredMeta is embedded with json:"-": its Label is no body field although
// it carries the JSON name of ShadowedName's.
type IgnoredMeta struct {
	Label string `json:"name" required:"true"`
}

type ShadowedName struct {
	Name *string `json:"name"`
}

type ignoredShadow struct {
	IgnoredMeta `json:"-"`
	ShadowedName
}

type uniqueUUIDs struct {
	IDs []uuid.UUID `json:"ids" uniqueItems:"true"`
}

type uniqueUUIDParam struct {
	IDs []uuid.UUID `json:"-" query:"ids" uniqueItems:"true"`
}

type uniqueOptionalUUIDs struct {
	IDs types.Optional[[]uuid.UUID] `json:"ids" uniqueItems:"true"`
}

type uniquePointerUUIDs struct {
	IDs *[2]uuid.UUID `json:"ids" uniqueItems:"true"`
}

type uniqueFloats struct {
	V []float64 `json:"v" uniqueItems:"true"`
}

type uniqueLocales struct {
	Locales []types.Locale `json:"locales" uniqueItems:"true"`
}

// uniqueObjectsHolder reaches uniqueObjects through a field.
type uniqueObjectsHolder struct {
	Inner uniqueObjects `json:"inner"`
}

type uniqueObjects struct {
	Items []nullableHolders `json:"items" uniqueItems:"true"`
}

// uniqueItemsAllowed carries uniqueItems on items the schema compares as
// the decoder holds them.
type uniqueItemsAllowed struct {
	Tags  []string                `json:"tags" uniqueItems:"true"`
	Modes []fixture.Mode          `json:"modes" uniqueItems:"true"`
	Codes [3]int8                 `json:"codes" uniqueItems:"true"`
	Refs  types.Optional[[]*uint] `json:"refs" uniqueItems:"true"`
	Flags *[]bool                 `json:"flags" uniqueItems:"true"`
	Pages []int                   `json:"-" query:"pages" uniqueItems:"true"`
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
		{"nullable struct ref", reflect.TypeFor[fixture.NullableStruct](), false, "fixture.NullableStruct.Meta: nullable tag is not supported on object references; in a request use *fixture.Meta, which accepts null unless it is required; to tell null from an absent key, or to send null in a response, use types.Optional[fixture.Meta], without the nullable tag"},
		{"nullable struct pointer in a request", reflect.TypeFor[nullableStructPointer](), true, "openapi.nullableStructPointer.Owner: nullable tag is not supported on object references; in a request *openapi.nullOwner already accepts null unless it is required, so drop the tag; to tell null from an absent key, or to send null in a response, use types.Optional[openapi.nullOwner], without the nullable tag"},
		{"nullable struct pointer in a response", reflect.TypeFor[nullableStructPointer](), false, "openapi.nullableStructPointer.Owner: nullable tag is not supported on object references; in a request *openapi.nullOwner already accepts null"},
		{"empty required tag on a body field", reflect.TypeFor[emptyRequired](), true, `openapi: field 'Name': the required tag must be "true" or "false", not ""`},
		{"loose required tag on a body field", reflect.TypeFor[looseRequired](), true, `openapi: field 'Name': the required tag must be "true" or "false", not "True"`},
		{"empty nullable tag", reflect.TypeFor[emptyNullable](), false, `openapi: field 'Note': the nullable tag must be "true" or "false", not ""`},
		{"path parameter tagged required false", reflect.TypeFor[optionalPath](), true, `openapi: openapi.optionalPath.ID: a path parameter is always required; drop required:"false"`},
		{"bound beyond int8", reflect.TypeFor[fixture.WideBound](), false, "fixture.WideBound.N: maximum 1000 is above the int8 range"},
		{"json string option in a request", reflect.TypeFor[fixture.StringOption](), true, "fixture.StringOption.N: the json string option is not supported"},
		{"json string option in a response", reflect.TypeFor[fixture.StringOption](), false, "fixture.StringOption.N: the json string option is not supported"},
		{"default tag in a request body", reflect.TypeFor[fixture.DefaultTag](), true, "fixture.DefaultTag.Limit: the default tag is not supported on a request type"},
		{"default tag on a parameter", reflect.TypeFor[fixture.DefaultParam](), true, "fixture.DefaultParam.Limit: the default tag is not supported on a request type"},
		{"query tag in a named field", reflect.TypeFor[fixture.NestedParam](), true, "fixture.NestedQuery.Q: the query tag is not supported on a nested struct; parameters and files are read only from the request type and the structs it embeds without a JSON name"},
		{"query tag in a slice element", reflect.TypeFor[fixture.NestedParamSlice](), true, "fixture.NestedQuery.Q: the query tag is not supported on a nested struct"},
		{"query tag in an Optional", reflect.TypeFor[fixture.NestedParamOptional](), true, "fixture.NestedQuery.Q: the query tag is not supported on a nested struct"},
		{"query tag embedded and nested", reflect.TypeFor[fixture.EmbeddedAndNested](), true, "fixture.NestedQuery.Q: the query tag is not supported on a nested struct"},
		{"form tag in a named field", reflect.TypeFor[fixture.NestedFile](), true, "fixture.NestedFileHolder.File: the form tag is not supported on a nested struct"},
		{"format tag on a time in a request", reflect.TypeFor[fixture.FormatOnTime](), true, "fixture.FormatOnTime.Day: the format tag is not supported on time.Time, whose format is set by the type; the schema and the decoder would disagree on the value's text"},
		{"format tag on a time in a response", reflect.TypeFor[fixture.FormatOnTime](), false, "fixture.FormatOnTime.Day: the format tag is not supported on time.Time"},
		{"format tag on an integer", reflect.TypeFor[fixture.FormatOnInt](), false, "fixture.FormatOnInt.N: the format tag is not supported on int"},
		{"format tag on a uuid parameter", reflect.TypeFor[fixture.FormatOnUUID](), true, "fixture.FormatOnUUID.ID: the format tag is not supported on uuid.UUID"},
		{"format tag on types.Email", reflect.TypeFor[fixture.FormatOnEmail](), true, "fixture.FormatOnEmail.Email: the format tag is not supported on *types.Email"},
		{"url.URL in a request", reflect.TypeFor[urlField](), true, "openapi.urlField.Callback: url.URL is not supported: encoding/json reads and writes it as an object, not as a URL string; use a string and parse it with url.Parse"},
		{"url.URL in a response", reflect.TypeFor[urlField](), false, "openapi.urlField.Callback: url.URL is not supported"},
		{"url.URL in a slice", reflect.TypeFor[urlList](), false, "openapi.urlList.Links: url.URL is not supported"},
		{"error in a request", reflect.TypeFor[errorField](), true, "openapi.errorField.Err: the interface type error is not supported in a request: encoding/json cannot decode a value into it; use a concrete type"},
		{"bound beyond a byte array item", reflect.TypeFor[wideByteArray](), false, "openapi.wideByteArray.B: maximum 300 is above the uint8 range"},
		{"non-null optional pointer in a request", reflect.TypeFor[nonNullPointer](), true, `openapi.nonNullPointer.Note: nullable:"false" has no effect on a pointer field of a request type, which accepts null unless it is required; tag it required:"true" to refuse null, or drop the pointer`},
		{"non-null optional pointer in a nested request struct", reflect.TypeFor[nonNullPointerHolder](), true, `openapi.nonNullPointer.Note: nullable:"false" has no effect`},
		{"nullable required pointer in a request", reflect.TypeFor[requiredNullablePointer](), true, `openapi.requiredNullablePointer.Note: nullable:"true" has no effect on a required pointer field of a request type, which refuses null; for a key that must be present and may be null, use types.Optional[T] with required:"true"`},
		{"nullable required pointer in a request slice item", reflect.TypeFor[requiredNullablePointerList](), true, `openapi.requiredNullablePointer.Note: nullable:"true" has no effect`},
		{"nullable string in a request", reflect.TypeFor[nullableString](), true, `openapi.nullableString.Name: nullable:"true" is not supported on a request field of type string: null would leave the zero value unchecked; use *string or types.Optional[string]`},
		{"nullable int in a request", reflect.TypeFor[nullableInt](), true, `openapi.nullableInt.Count: nullable:"true" is not supported on a request field of type int`},
		{"nullable uuid in a request", reflect.TypeFor[nullableUUID](), true, `openapi.nullableUUID.ID: nullable:"true" is not supported on a request field of type uuid.UUID`},
		{"nullable time in a request slice item", reflect.TypeFor[nullableTimeHolder](), true, `openapi.nullableTime.At: nullable:"true" is not supported on a request field of type time.Time`},
		{"unique uuids in a request", reflect.TypeFor[uniqueUUIDs](), true, `openapi.uniqueUUIDs.IDs: uniqueItems is not supported on []uuid.UUID in a request type: the schema compares the items' text, which the decoder may read as equal values; check uniqueness in Validate()`},
		{"unique uuid parameter", reflect.TypeFor[uniqueUUIDParam](), true, `openapi.uniqueUUIDParam.IDs: uniqueItems is not supported on []uuid.UUID in a request type`},
		{"unique optional uuids", reflect.TypeFor[uniqueOptionalUUIDs](), true, `openapi.uniqueOptionalUUIDs.IDs: uniqueItems is not supported on types.Optional[[]github.com/google/uuid.UUID] in a request type`},
		{"unique uuid array pointer", reflect.TypeFor[uniquePointerUUIDs](), true, `openapi.uniquePointerUUIDs.IDs: uniqueItems is not supported on *[2]uuid.UUID in a request type`},
		{"unique floats", reflect.TypeFor[uniqueFloats](), true, `openapi.uniqueFloats.V: uniqueItems is not supported on []float64 in a request type`},
		{"unique locales", reflect.TypeFor[uniqueLocales](), true, `openapi.uniqueLocales.Locales: uniqueItems is not supported on []types.Locale in a request type`},
		{"exclusive bound on a float", reflect.TypeFor[exclusiveFloat](), true, `openapi.exclusiveFloat.Rate: exclusiveMinimum is not supported on float64 in a request type: the schema checks the number as the client wrote it, and the float can round it onto the bound; use an inclusive bound, a json.Number field, or a check in Validate()`},
		{"exclusive bound on a float parameter", reflect.TypeFor[exclusiveFloatParam](), true, `openapi.exclusiveFloatParam.Max: exclusiveMaximum is not supported on *float32 in a request type`},
		{"exclusive bound on float items", reflect.TypeFor[exclusiveFloatItems](), true, `openapi.exclusiveFloatItems.Weights: exclusiveMinimum is not supported on types.Optional[[]float64] in a request type`},
		{"nullable blank field in a request", reflect.TypeFor[nullableBlankHolder](), true, `openapi.nullableBlank: a nullable tag on the blank field is not supported in a request type: null would pass for the whole struct and leave its zero value unchecked; use a pointer field`},
		{"default on a parameter of an unexported embedded struct", reflect.TypeFor[unexportedEmbedParams](), true, `openapi.paging.Limit: the default tag is not supported on a request type`},
		{"unknown tag on a parameter of an unexported embedded struct", reflect.TypeFor[unexportedEmbedParams](), true, `openapi.paging.Limit: unknown struct tag "minimun"`},
		{"exclusive bound on a float parameter of an unexported embedded struct", reflect.TypeFor[unexportedEmbedParams](), true, `openapi.paging.Ratio: exclusiveMinimum is not supported on float64 in a request type`},
		{"unique uuids on a parameter of an ignored embedded struct", reflect.TypeFor[ignoredEmbedParams](), true, `openapi.Paging.IDs: uniqueItems is not supported on []uuid.UUID in a request type`},
		{"bound on a map", reflect.TypeFor[boundOnMap](), true, `openapi.boundOnMap.Scores: maximum constrains a number, and on map[string]int the schema would carry it on an object, where nothing checks it; check the value in Validate()`},
		{"length on a map", reflect.TypeFor[lengthOnMap](), true, `openapi.lengthOnMap.Labels: maxLength constrains a string, and on map[string]string the schema would carry it on an object`},
		{"bound on a struct", reflect.TypeFor[boundOnStruct](), true, `openapi.boundOnStruct.Inner: maximum constrains a number, and on openapi.ShadowedName the schema would carry it on an object`},
		{"bound on a slice of slices", reflect.TypeFor[boundOnGrid](), true, `openapi.boundOnGrid.Grid: maximum constrains a number, and on [][]int the schema would carry it on an array`},
		{"length on an integer", reflect.TypeFor[lengthOnInt](), true, `openapi.lengthOnInt.N: minLength constrains a string, and on *int the schema would carry it on an integer`},
		{"items on a string", reflect.TypeFor[itemsOnString](), true, `openapi.itemsOnString.Name: minItems constrains an array, and on types.Optional[string] the schema would carry it on a string`},
		{"bound on a time parameter", reflect.TypeFor[boundOnTime](), true, `openapi.boundOnTime.At: minimum constrains a number, and on time.Time the schema would carry it on a string`},
		{"minItems on a fixed array", reflect.TypeFor[itemsOnFixedArray](), true, `openapi.itemsOnFixedArray.Codes: minItems is not supported on [3]int in a request type: the array always has 3 items, and encoding/json would fill a shorter one with zero values the schema never saw`},
		{"maxItems on an optional fixed array", reflect.TypeFor[maxItemsOnFixedArray](), true, `openapi.maxItemsOnFixedArray.Codes: maxItems is not supported on types.Optional[[3]int] in a request type: the array always has 3 items`},
		{"non-null optional struct in a request", reflect.TypeFor[nonNullOptionalStruct](), true, `openapi.nonNullOptionalStruct.Inner: nullable:"false" has no effect on types.Optional[github.com/vanclief/compose/components/rest/api/openapi.ShadowedName] in a request type, whose schema accepts null whatever the tag says; refuse null in Validate()`},
		{"non-null any in a request", reflect.TypeFor[nonNullAny](), true, `openapi.nonNullAny.Value: the nullable tag is not supported on interface {}, whose schema has no type and accepts null already`},
		{"non-null raw message in a request", reflect.TypeFor[nonNullRaw](), true, `openapi.nonNullRaw.Value: the nullable tag is not supported on json.RawMessage, whose schema has no type and accepts null already`},
		{"big.Int by value in a response map", reflect.TypeFor[bigByValueInMap](), false, `big.Int is held by value in a map of a response type: encoding/json cannot call its marshaler there, which has a pointer receiver, and would send its fields instead; hold *big.Int`},
		{"big.Int in a struct held by value in a response map", reflect.TypeFor[bigNested](), false, `big.Int is held by value in a map of a response type`},
		{"nullable any in a response", reflect.TypeFor[nullableAny](), false, `openapi.nullableAny.Data: the nullable tag is not supported on interface {}, whose schema has no type and accepts null already`},
		{"nullable optional any in a request", reflect.TypeFor[nullableOptionalAny](), true, `openapi.nullableOptionalAny.Data: the nullable tag is not supported on types.Optional[interface {}]`},
		{"parameter in an embedded struct with a JSON name", reflect.TypeFor[namedEmbedParam](), true, `openapi.NamedPaging.Limit: the query tag is not supported on a nested struct; parameters and files are read only from the request type and the structs it embeds without a JSON name`},
		{"parameter in a struct embedded by value with a JSON name", reflect.TypeFor[namedValueEmbedParam](), true, `openapi.NamedPaging.Limit: the query tag is not supported on a nested struct`},
		{"parameter tag on an unexported field", reflect.TypeFor[unexportedParam](), true, `openapi.unexportedParam.projectID: the query tag is never read on an unexported or embedded field; declare an exported, named field`},
		{"parameter tag on an embedded field", reflect.TypeFor[embeddedFieldParam](), true, `openapi.embeddedFieldParam.EmbeddedToken: the query tag is never read on an unexported or embedded field`},
		{"bound on an any", reflect.TypeFor[boundOnAny](), true, `openapi.boundOnAny.Value: maximum is not supported on interface {} in a request type: its schema has no type, so the tag would check only a number and pass any other value; check the value in Validate()`},
		{"length on a raw message", reflect.TypeFor[lengthOnRaw](), true, `openapi.lengthOnRaw.Meta: maxLength is not supported on json.RawMessage in a request type: its schema has no type, so the tag would check only a string and pass any other value`},
		{"items on an optional any", reflect.TypeFor[itemsOnAny](), true, `openapi.itemsOnAny.Value: maxItems is not supported on types.Optional[interface {}] in a request type: its schema has no type, so the tag would check only an array and pass any other value`},
		{"parameter tag on a shadowed field", reflect.TypeFor[shadowedParam](), true, `openapi.ShadowPaging.Limit: the query tag is never read: another field named Limit hides this one in the request type; rename one of them`},
		{"parameter tags on fields that hide each other", reflect.TypeFor[ambiguousParam](), true, `openapi.ShadowPaging.Limit: the query tag is never read: another field named Limit hides this one in the request type`},
		{"unique objects in a nested request struct", reflect.TypeFor[uniqueObjectsHolder](), true, `openapi.uniqueObjects.Items: uniqueItems is not supported on []openapi.nullableHolders in a request type`},
		{"nullable embedded struct in a request", reflect.TypeFor[nullableEmbedded](), true, `openapi.nullableEmbedded.EmbeddedNote: the nullable tag does not apply to an embedded struct, whose fields are promoted into the enclosing object; drop the tag`},
		{"nullable embedded struct in a response", reflect.TypeFor[nullableEmbedded](), false, `openapi.nullableEmbedded.EmbeddedNote: the nullable tag does not apply to an embedded struct`},
		{"minLength 0 on a netip.Prefix", reflect.TypeFor[prefixMinLengthZero](), true, `openapi.prefixMinLengthZero.A: minLength 0 is not supported on netip.Prefix in a request type: its decoder reads "" as the zero value without an error, and the schema's minLength 1 is what refuses it; drop the tag or set it to 1 or more`},
		{"minLength 0 on a netip.Prefix query list", reflect.TypeFor[prefixListMinLengthZero](), true, `openapi.prefixListMinLengthZero.L: minLength 0 is not supported on []netip.Prefix in a request type`},
		{"negative minLength on an optional netip.AddrPort", reflect.TypeFor[optionalAddrPortMinLength](), true, `openapi.optionalAddrPortMinLength.A: minLength -1 is not supported on types.Optional[net/netip.AddrPort] in a request type`},
		{"minLength on a map of netip.Prefix", reflect.TypeFor[prefixMapMinLength](), true, `openapi.prefixMapMinLength.M: minLength constrains a string, and on map[string]netip.Prefix the schema would carry it on an object`},
		{"empty additionalProperties tag on the blank field", reflect.TypeFor[blankAdditionalProperties](), true, `openapi: field '_': the additionalProperties tag must be "true" or "false", not ""`},
		{"minLength 0 on optional netip.Prefix items", reflect.TypeFor[optionalPrefixItems](), true, `openapi.optionalPrefixItems.A: minLength 0 is not supported on []types.Optional[net/netip.Prefix] in a request type`},
		{"nullable optional struct in a request", reflect.TypeFor[nullableOptionalStruct](), true, `openapi.nullableOptionalStruct.Owner: the nullable tag is not supported on a types.Optional of a struct, which accepts null already; drop the tag`},
		{"nullable optional struct in a response", reflect.TypeFor[nullableOptionalStruct](), false, `openapi.nullableOptionalStruct.Owner: the nullable tag is not supported on a types.Optional of a struct, which accepts null already; drop the tag`},
		{"nullable optional struct pointer", reflect.TypeFor[nullableOptionalStructPointer](), true, `openapi.nullableOptionalStructPointer.Owner: the nullable tag is not supported on a types.Optional of a struct`},
		{"nullable struct not read from the body", reflect.TypeFor[nullableIgnoredStruct](), true, `openapi.nullableIgnoredStruct.Owner: the nullable tag does not apply to a field that is not read from the body (json:"-"); drop the tag`},
		{"nullable struct not read from the body in a response", reflect.TypeFor[nullableIgnoredStruct](), false, `openapi.nullableIgnoredStruct.Owner: the nullable tag does not apply to a field that is not read from the body (json:"-"); drop the tag`},
		{"nullable embedded struct not read from the body", reflect.TypeFor[nullableIgnoredEmbedded](), true, `openapi.nullableIgnoredEmbedded.EmbeddedNote: the nullable tag does not apply to a field that is not read from the body (json:"-"); drop the tag`},
		{"nullable struct form field", reflect.TypeFor[nullableFormStruct](), true, `openapi.nullableFormStruct.Meta: the nullable tag does not apply to a field that is not read from the body (json:"-"); drop the tag`},
		{"nullable struct query parameter", reflect.TypeFor[nullableQueryStruct](), true, `openapi.nullableQueryStruct.Owner: the nullable tag does not apply to a field that is not read from the body (json:"-"); drop the tag`},
		{"nullable struct pointer header parameter", reflect.TypeFor[nullableHeaderStruct](), true, `openapi.nullableHeaderStruct.Owner: the nullable tag does not apply to a field that is not read from the body (json:"-"); drop the tag`},
		{"loose boolean tags on an embedded struct", reflect.TypeFor[looseEmbedded](), true, `openapi: openapi.looseEmbedded.EmbeddedNote: the required tag must be "true" or "false", not "yes"; openapi.looseEmbedded.EmbeddedNote: the nullable tag must be "true" or "false", not "True"; openapi.looseEmbedded.EmbeddedNote: the hidden tag must be "true" or "false", not "1"`},
		{"loose boolean tags on an embedded struct in a response", reflect.TypeFor[looseEmbedded](), false, `openapi.looseEmbedded.EmbeddedNote: the required tag must be "true" or "false", not "yes"`},
		{"empty boolean tags on an embedded struct pointer", reflect.TypeFor[emptyEmbeddedPointer](), true, `openapi: openapi.emptyEmbeddedPointer.EmbeddedNote: the required tag must be "true" or "false", not ""; openapi.emptyEmbeddedPointer.EmbeddedNote: the nullable tag must be "true" or "false", not ""`},
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

// TestStructPointerAcceptsNull: a pointer to a struct accepts null in a
// request without a nullable tag, as the lint tells an app that tags one.
func TestStructPointerAcceptsNull(t *testing.T) {
	r := NewReflector()
	_, err := r.AddRequest(reflect.TypeFor[structPointer]())
	require.NoError(t, err)
	res, err := r.Finalize()
	require.NoError(t, err)

	s := component(t, res, "openapi.structPointer")
	require.Equal(t, map[string]any{"anyOf": []any{
		map[string]any{"$ref": ref("openapi.nullOwner")},
		map[string]any{"type": "null"},
	}}, prop(t, s, "owner"))
}

// TestEmbeddedAndPrefixAllowed: an embedded struct without the nullable tag
// registers in a request and a response, and a minLength of 1 or more on a
// netip.Prefix replaces the 1.
func TestEmbeddedAndPrefixAllowed(t *testing.T) {
	r := NewReflector()
	_, err := r.AddRequest(reflect.TypeFor[plainEmbedded]())
	require.NoError(t, err)
	err = r.AddResponse(reflect.TypeFor[plainEmbedded]())
	require.NoError(t, err)
	_, err = r.AddRequest(reflect.TypeFor[prefixMinLengthTwo]())
	require.NoError(t, err)
	res, err := r.Finalize()
	require.NoError(t, err)

	require.Equal(t, map[string]any{"type": []any{"string", "null"}, "minLength": 2.0}, prop(t, component(t, res, "openapi.prefixMinLengthTwo"), "a"))
}

// TestBoolTagValues: a boolean tag on a body field or a path, query or
// header parameter must read exactly true or false, the empty value
// included.
func TestBoolTagValues(t *testing.T) {
	locations := []struct {
		name string
		tag  string
	}{
		{"body field", `json:"f"`},
		{"path parameter", `json:"-" path:"f"`},
		{"query parameter", `json:"-" query:"f"`},
		{"header parameter", `json:"-" header:"X-F"`},
	}
	for _, loc := range locations {
		for _, tag := range []string{"required", "nullable", "deprecated", "hidden", "uniqueItems"} {
			for _, value := range []string{"True", "1", ""} {
				t.Run(loc.name+" "+tag+" "+value, func(t *testing.T) {
					typ := reflect.StructOf([]reflect.StructField{{
						Name: "F",
						Type: reflect.TypeFor[[]string](),
						Tag:  reflect.StructTag(fmt.Sprintf(`%s %s:%q`, loc.tag, tag, value)),
					}})
					_, err := NewReflector().AddRequest(typ)
					require.Error(t, err)
					require.Equal(t, ez.EINVALID, ez.ErrorCode(err))
					require.Contains(t, ez.ErrorMessage(err), fmt.Sprintf(`the %s tag must be "true" or "false", not %q`, tag, value))
				})
			}
		}
	}
}

// TestEmptyTextMinLengthNestings: a minLength tag that reaches a
// netip.Prefix or netip.AddrPort through pointers, types.Optional and one
// slice or array, in any order, may not lower its minLength 1 in a request
// type, and may raise it. Under a second slice the tag is refused as
// misplaced either way.
func TestEmptyTextMinLengthNestings(t *testing.T) {
	nestings := []reflect.Type{
		reflect.TypeFor[netip.Prefix](),
		reflect.TypeFor[*netip.AddrPort](),
		reflect.TypeFor[[]types.Optional[netip.Prefix]](),
		reflect.TypeFor[types.Optional[[]netip.Prefix]](),
		reflect.TypeFor[types.Optional[[]*netip.AddrPort]](),
		reflect.TypeFor[types.Optional[*netip.Prefix]](),
		reflect.TypeFor[*[]*netip.Prefix](),
		reflect.TypeFor[[2]types.Optional[netip.AddrPort]](),
		reflect.TypeFor[types.Optional[[2]*netip.Prefix]](),
	}
	for _, typ := range nestings {
		field := func(minLength string) reflect.Type {
			return reflect.StructOf([]reflect.StructField{{
				Name: "A",
				Type: typ,
				Tag:  reflect.StructTag(fmt.Sprintf(`json:"a" minLength:%q`, minLength)),
			}})
		}
		t.Run(typ.String(), func(t *testing.T) {
			_, err := NewReflector().AddRequest(field("0"))
			require.Error(t, err)
			require.Equal(t, ez.EINVALID, ez.ErrorCode(err))
			require.Contains(t, ez.ErrorMessage(err), fmt.Sprintf(`.A: minLength 0 is not supported on %s in a request type`, typ))

			r := NewReflector()
			_, err = r.AddRequest(field("2"))
			require.NoError(t, err)
			_, err = r.Finalize()
			require.NoError(t, err)
		})
	}

	for _, minLength := range []string{"0", "2"} {
		typ := reflect.StructOf([]reflect.StructField{{
			Name: "A",
			Type: reflect.TypeFor[[][]netip.Prefix](),
			Tag:  reflect.StructTag(fmt.Sprintf(`json:"a" minLength:%q`, minLength)),
		}})
		_, err := NewReflector().AddRequest(typ)
		require.Error(t, err)
		require.Contains(t, ez.ErrorMessage(err), `.A: minLength constrains a string, and on [][]netip.Prefix the schema would carry it on an array`)
		require.NotContains(t, ez.ErrorMessage(err), "is not supported on [][]netip.Prefix")
	}
}

// TestEmbeddedBoolTagValues: a boolean tag on an embedded struct, whose
// fields are promoted, must read exactly true or false, the empty value
// included; false registers in a request and a response (uniqueItems is
// refused there as misplaced whatever its value).
func TestEmbeddedBoolTagValues(t *testing.T) {
	embed := func(tag string) reflect.Type {
		return reflect.StructOf([]reflect.StructField{{
			Name:      "EmbeddedNote",
			Type:      reflect.TypeFor[EmbeddedNote](),
			Tag:       reflect.StructTag(tag),
			Anonymous: true,
		}})
	}
	for _, tag := range []string{"required", "nullable", "deprecated", "hidden", "uniqueItems"} {
		for _, value := range []string{"True", "1", ""} {
			t.Run(tag+" "+value, func(t *testing.T) {
				_, err := NewReflector().AddRequest(embed(fmt.Sprintf(`%s:%q`, tag, value)))
				require.Error(t, err)
				require.Equal(t, ez.EINVALID, ez.ErrorCode(err))
				require.Contains(t, ez.ErrorMessage(err), fmt.Sprintf(`.EmbeddedNote: the %s tag must be "true" or "false", not %q`, tag, value))
			})
		}
	}

	typ := embed(`required:"false" nullable:"false" deprecated:"false" hidden:"false"`)
	r := NewReflector()
	_, err := r.AddRequest(typ)
	require.NoError(t, err)
	err = r.AddResponse(typ)
	require.NoError(t, err)
	_, err = r.Finalize()
	require.NoError(t, err)
}

// TestNullableFalseAllowed: nullable:"false" stays allowed where it has an
// effect, on a pointer in a response, and where it agrees with the request
// shape, on a required pointer.
func TestNullableFalseAllowed(t *testing.T) {
	r := NewReflector()
	err := r.AddResponse(reflect.TypeFor[nonNullPointer]())
	require.NoError(t, err)
	_, err = r.AddRequest(reflect.TypeFor[requiredNonNullPointer]())
	require.NoError(t, err)
	res, err := r.Finalize()
	require.NoError(t, err)

	require.Equal(t, map[string]any{"type": "string"}, prop(t, component(t, res, "openapi.nonNullPointer"), "note"))
	require.Equal(t, map[string]any{"type": "string"}, prop(t, component(t, res, "openapi.requiredNonNullPointer"), "note"))
}

// TestNullableTrueAllowed: nullable:"true" stays allowed where it has an
// effect, on a pointer in a response, and where it agrees with the request
// shape, on an optional pointer.
func TestNullableTrueAllowed(t *testing.T) {
	r := NewReflector()
	err := r.AddResponse(reflect.TypeFor[requiredNullablePointer]())
	require.NoError(t, err)
	_, err = r.AddRequest(reflect.TypeFor[optionalNullablePointer]())
	require.NoError(t, err)
	res, err := r.Finalize()
	require.NoError(t, err)

	nullableString := map[string]any{"type": []any{"string", "null"}}
	response := component(t, res, "openapi.requiredNullablePointer")
	require.Equal(t, nullableString, prop(t, response, "note"))
	require.Equal(t, []any{"note"}, response["required"])
	require.Equal(t, nullableString, prop(t, component(t, res, "openapi.optionalNullablePointer"), "note"))
}

// TestNullableTrueOnValue: nullable:"true" stays allowed on a request body
// field that holds null, on a parameter, and on a response field of any
// type.
func TestNullableTrueOnValue(t *testing.T) {
	r := NewReflector()
	params, err := r.AddRequest(reflect.TypeFor[nullableHolders]())
	require.NoError(t, err)
	require.Len(t, params, 1)
	for _, typ := range []reflect.Type{reflect.TypeFor[nullableString](), reflect.TypeFor[nullableInt](), reflect.TypeFor[nullableUUID](), reflect.TypeFor[nullableTime]()} {
		err = r.AddResponse(typ)
		require.NoError(t, err, typ.String())
	}
	_, err = r.Finalize()
	require.NoError(t, err)
}

// TestUniqueItemsAllowed: uniqueItems stays allowed on strings, enums,
// integers and booleans in a request, behind pointers and an Optional, and
// on any item in a response.
func TestUniqueItemsAllowed(t *testing.T) {
	r := NewReflector()
	params, err := r.AddRequest(reflect.TypeFor[uniqueItemsAllowed]())
	require.NoError(t, err)
	require.Len(t, params, 1)
	require.True(t, params[0].Schema.UniqueItems)
	for _, typ := range []reflect.Type{reflect.TypeFor[uniqueUUIDs](), reflect.TypeFor[uniqueFloats](), reflect.TypeFor[uniqueLocales]()} {
		err = r.AddResponse(typ)
		require.NoError(t, err, typ.String())
	}
	res, err := r.Finalize()
	require.NoError(t, err)

	s := component(t, res, "openapi.uniqueItemsAllowed")
	for _, name := range []string{"tags", "modes", "codes", "refs", "flags"} {
		require.Equal(t, true, prop(t, s, name)["uniqueItems"], name)
	}
}

// TestFloatBoundsAllowed: a float keeps its inclusive bounds in a request,
// a json.Number and an integer their exclusive ones, and a response float
// any bound.
func TestFloatBoundsAllowed(t *testing.T) {
	r := NewReflector()
	_, err := r.AddRequest(reflect.TypeFor[floatBoundsAllowed]())
	require.NoError(t, err)
	for _, typ := range []reflect.Type{reflect.TypeFor[exclusiveFloat](), reflect.TypeFor[exclusiveFloatItems]()} {
		err = r.AddResponse(typ)
		require.NoError(t, err, typ.String())
	}
	res, err := r.Finalize()
	require.NoError(t, err)

	s := component(t, res, "openapi.floatBoundsAllowed")
	require.Equal(t, float64(0), prop(t, s, "amount")["exclusiveMinimum"])
	require.Equal(t, float64(10), prop(t, s, "count")["exclusiveMaximum"])
	require.Equal(t, float64(1), prop(t, s, "rate")["maximum"])
}

// TestPlacedTags: a bound, length or item tag stays allowed in a request
// where the schema carries it on the type it constrains, and anywhere in a
// response, as does a nullable blank field there.
func TestPlacedTags(t *testing.T) {
	r := NewReflector()
	params, err := r.AddRequest(reflect.TypeFor[placedTags]())
	require.NoError(t, err)
	require.Len(t, params, 1)
	for _, typ := range []reflect.Type{reflect.TypeFor[boundOnMap](), reflect.TypeFor[boundOnGrid](), reflect.TypeFor[lengthOnInt](), reflect.TypeFor[nullableBlankHolder](), reflect.TypeFor[boundOnAny](), reflect.TypeFor[lengthOnRaw]()} {
		err = r.AddResponse(typ)
		require.NoError(t, err, typ.String())
	}
	res, err := r.Finalize()
	require.NoError(t, err)

	s := component(t, res, "openapi.placedTags")
	require.Equal(t, float64(0), prop(t, s, "rates")["items"].(map[string]any)["minimum"])
	require.Equal(t, float64(8), prop(t, s, "names")["items"].(map[string]any)["maxLength"])
	require.Equal(t, float64(64), prop(t, s, "blob")["maxLength"])
	require.Equal(t, float64(200), prop(t, s, "hash")["items"].(map[string]any)["maximum"])
}

// TestAnonymousStructComponents: an anonymous struct met again refers to the
// component it was given, not to the one that took its hint first.
func TestAnonymousStructComponents(t *testing.T) {
	r := NewReflector()
	_, err := r.AddRequest(reflect.TypeFor[anonymousItems]())
	require.NoError(t, err)
	res, err := r.Finalize()
	require.NoError(t, err)

	s := component(t, res, "openapi.anonymousItems")
	a := prop(t, s, "a")["items"].(map[string]any)["$ref"].(string)
	b := prop(t, s, "b")["items"].(map[string]any)["$ref"].(string)
	c := prop(t, s, "c")["items"].(map[string]any)["$ref"].(string)
	require.NotEqual(t, a, b)
	require.Equal(t, b, c)
	item := component(t, res, strings.TrimPrefix(c, RefPrefix))
	require.Equal(t, float64(3), prop(t, item, "y")["maxLength"])
}

// TestNonNullHonoured: nullable:"false" stays allowed in a request where the
// schema obeys it, and an item tag on a fixed array in a response.
func TestNonNullHonoured(t *testing.T) {
	r := NewReflector()
	_, err := r.AddRequest(reflect.TypeFor[nonNullHonoured]())
	require.NoError(t, err)
	for _, typ := range []reflect.Type{reflect.TypeFor[itemsOnFixedArray](), reflect.TypeFor[nonNullOptionalStruct]()} {
		err = r.AddResponse(typ)
		require.NoError(t, err, typ.String())
	}
	res, err := r.Finalize()
	require.NoError(t, err)

	s := component(t, res, "openapi.nonNullHonoured")
	require.Equal(t, "string", prop(t, s, "name")["type"])
	require.Equal(t, "array", prop(t, s, "tags")["type"])
	require.Equal(t, map[string]any{"$ref": ref("fixture.Mode")}, prop(t, s, "mode"))
}

// TestAddressableMarshalers: a marshaler with a pointer receiver is allowed
// in a response wherever encoding/json can take the value's address, and
// anywhere in a request, whose map values are decoded into new values.
func TestAddressableMarshalers(t *testing.T) {
	r := NewReflector()
	err := r.AddResponse(reflect.TypeFor[bigAddressable]())
	require.NoError(t, err)
	_, err = r.AddRequest(reflect.TypeFor[bigByValueInMap]())
	require.NoError(t, err)

	// What the lint allows is what encoding/json writes through a pointer.
	value := bigAddressable{List: []big.Int{*big.NewInt(2)}, Lists: map[string][]big.Int{"a": {*big.NewInt(3)}}}
	value.N.SetInt64(1)
	value.Pair[1].SetInt64(4)
	value.Maybe = types.Optional[big.Int]{Set: true, Value: *big.NewInt(5)}
	raw, err := json.Marshal(&value)
	require.NoError(t, err)
	require.JSONEq(t, `{"n":1,"list":[2],"pair":[0,4],"maybe":5,"totals":null,"lists":{"a":[3]},"rows":null,"at":null,"ids":null}`, string(raw))
}

// TestFixedArrayNotNullable: a [N]T accepts no null in a request, where
// encoding/json would leave the zero array unchecked, and promises none in a
// response; a pointer to one is nullable.
func TestFixedArrayNotNullable(t *testing.T) {
	r := NewReflector()
	_, err := r.AddRequest(reflect.TypeFor[fixedArray]())
	require.NoError(t, err)
	err = r.AddResponse(reflect.TypeFor[stdlibShapes]())
	require.NoError(t, err)
	res, err := r.Finalize()
	require.NoError(t, err)

	s := component(t, res, "openapi.fixedArray")
	require.Equal(t, "array", prop(t, s, "codes")["type"])
	require.Equal(t, "array", prop(t, s, "tags")["type"])
	require.Equal(t, []any{"array", "null"}, prop(t, s, "maybe")["type"])
	require.Equal(t, "array", prop(t, component(t, res, "openapi.stdlibShapes"), "hash")["type"])
}

// TestPointerToArrayNullable: a pointer to a fixed-size array, or to a text
// type of array kind, is nullable in a response, where Go writes a nil one
// as null, so registration takes it without omitempty.
func TestPointerToArrayNullable(t *testing.T) {
	r := NewReflector()
	err := r.AddResponse(reflect.TypeFor[pointerArrays]())
	require.NoError(t, err)
	res, err := r.Finalize()
	require.NoError(t, err)

	s := component(t, res, "openapi.pointerArrays")
	require.Equal(t, []any{"array", "null"}, prop(t, s, "codes")["type"])
	require.Equal(t, []any{"string", "null"}, prop(t, s, "hash")["type"])
}

// TestIgnoredEmbedShape: a field of an embedded struct tagged json:"-" does
// not lend its required tag to the body property that carries its JSON name.
func TestIgnoredEmbedShape(t *testing.T) {
	r := NewReflector()
	_, err := r.AddRequest(reflect.TypeFor[ignoredShadow]())
	require.NoError(t, err)
	res, err := r.Finalize()
	require.NoError(t, err)

	s := component(t, res, "openapi.ignoredShadow")
	require.NotContains(t, s, "required")
	require.Equal(t, map[string]any{"type": []any{"string", "null"}}, prop(t, s, "name"))
}

// A request map keyed by an enum type fails at any depth; a response may
// carry one, and a request may use an enum as the map value, which the
// schema validates.
func TestEnumMapKey(t *testing.T) {
	_, err := NewReflector().AddRequest(reflect.TypeFor[fixture.EnumKey]())
	require.Error(t, err)
	require.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	require.Equal(t, "openapi: fixture.EnumKey.Counts: map keys are not validated, so a key of the enum type fixture.RunStatus could hold any string; use a string key and check it in Validate()", ez.ErrorMessage(err))

	_, err = NewReflector().AddRequest(reflect.TypeFor[fixture.NestedEnumKey]())
	require.Error(t, err)
	require.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	require.Equal(t, "openapi: "+
		"fixture.NestedEnumKey.Counts: map keys are not validated, so a key of the enum type fixture.RunStatus could hold any string; use a string key and check it in Validate(); "+
		"fixture.NestedEnumKey.ByName: map keys are not validated, so a key of the enum type fixture.Mode could hold any string; use a string key and check it in Validate(); "+
		"fixture.EnumKey.Counts: map keys are not validated, so a key of the enum type fixture.RunStatus could hold any string; use a string key and check it in Validate()",
		ez.ErrorMessage(err))

	for _, typ := range []reflect.Type{reflect.TypeFor[fixture.EnumKey](), reflect.TypeFor[fixture.NestedEnumKey]()} {
		err = NewReflector().AddResponse(typ)
		require.NoError(t, err, typ.String())
	}

	_, err = NewReflector().AddRequest(reflect.TypeFor[fixture.EnumValue]())
	require.NoError(t, err)
}

// A format tag on a string, a named string, a slice of strings (on its
// items) or an Optional string describes text decoded as it is, in a
// request and a response.
func TestFormatTagOnStrings(t *testing.T) {
	r := NewReflector()
	params, err := r.AddRequest(reflect.TypeFor[fixture.FormatOnString]())
	require.NoError(t, err)
	err = r.AddResponse(reflect.TypeFor[fixture.FormatOnString]())
	require.NoError(t, err)
	res, err := r.Finalize()
	require.NoError(t, err)

	require.Len(t, params, 1)
	require.Equal(t, "date", params[0].Schema.Format)

	s := component(t, res, "fixture.FormatOnString")
	require.Equal(t, "date", prop(t, s, "day")["format"])
	require.Equal(t, "email", prop(t, s, "email")["format"])
	require.Equal(t, "hostname", prop(t, s, "slug")["format"])
	require.Equal(t, "hostname", prop(t, s, "hosts")["items"].(map[string]any)["format"])
	require.Equal(t, "date", prop(t, s, "since")["format"])
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
