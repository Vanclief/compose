package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/vanclief/compose/components/rest/api"
	"github.com/vanclief/compose/components/rest/api/internal/fixture"
	"github.com/vanclief/compose/components/rest/apitest"
	"github.com/vanclief/compose/components/rest/requests"
	"github.com/vanclief/compose/primitives/enums"
	"github.com/vanclief/compose/types"
)

const moneyPattern = `^[0-9]+\.[0-9]{2} [A-Z]{3}$`

// Money is the first example of api.SchemaProvider's doc comment: it writes
// and reads itself as "12.50 MXN", and says so with the same text.
type Money struct {
	Cents    int64
	Currency string
}

func (m Money) MarshalJSON() ([]byte, error) {
	return json.Marshal(fmt.Sprintf("%d.%02d %s", m.Cents/100, m.Cents%100, m.Currency))
}

func (m *Money) UnmarshalJSON(raw []byte) error {
	var text string
	err := json.Unmarshal(raw, &text)
	if err != nil {
		return err
	}
	var whole, cents int64
	_, err = fmt.Sscanf(text, "%d.%d %s", &whole, &cents, &m.Currency)
	if err != nil {
		return err
	}
	m.Cents = whole*100 + cents
	return nil
}

func (Money) JSONSchema(refs *api.SchemaRefs) string {
	return `{"type": "string", "pattern": "^[0-9]+\\.[0-9]{2} [A-Z]{3}$"}`
}

// Discount is the second example of the doc comment: a Money or a
// percentage.
type Discount struct {
	Amount  *Money
	Percent int
}

func (d Discount) MarshalJSON() ([]byte, error) {
	if d.Amount != nil {
		return json.Marshal(d.Amount)
	}
	return json.Marshal(d.Percent)
}

func (d *Discount) UnmarshalJSON(raw []byte) error {
	if len(raw) > 0 && raw[0] == '"' {
		d.Amount = &Money{}
		return d.Amount.UnmarshalJSON(raw)
	}
	return json.Unmarshal(raw, &d.Percent)
}

func (Discount) JSONSchema(refs *api.SchemaRefs) string {
	return `{"anyOf": [` + refs.For(reflect.TypeFor[Money]()) +
		`, {"type": "integer", "minimum": 0, "maximum": 100}]}`
}

type discountRequest struct {
	requests.NoRules
	Discount Discount `json:"discount" required:"true"`
}

type discountResponse struct {
	Discount Discount `json:"discount"`
	Total    Money    `json:"total"`
}

var (
	moneyRef    = map[string]any{"$ref": "#/components/schemas/api_test.Money"}
	nullSchema  = map[string]any{"type": "null"}
	moneySchema = map[string]any{"type": "string", "pattern": moneyPattern}
)

// TestSchemaProviderExample: the example of the doc comment registers, and a
// request and a response go through it, validated and checked against the
// schema its text states, in both documents.
func TestSchemaProviderExample(t *testing.T) {
	e := newEcho()
	reg := api.New(e, api.Public())
	api.POST(reg.Group("/discounts"), "", "apply", func(ctx context.Context, req *discountRequest) (discountResponse, error) {
		return discountResponse{Discount: req.Discount, Total: Money{Cents: 1000, Currency: "MXN"}}, nil
	})
	reg.Ready()

	discount := map[string]any{"anyOf": []any{moneyRef, map[string]any{"type": "integer", "minimum": 0.0, "maximum": 100.0}}}
	for _, v := range []api.Version{api.V31, api.V30} {
		raw, err := reg.Spec(v)
		require.NoError(t, err)
		doc := decode(t, raw)
		require.Equal(t, moneySchema, dig(t, doc, "components", "schemas", "api_test.Money"))
		require.Equal(t, discount, dig(t, doc, "components", "schemas", "api_test.Discount"))
		require.Equal(t, map[string]any{"$ref": "#/components/schemas/api_test.Discount"}, dig(t, doc, "components", "schemas", "api_test.discountResponse", "properties", "discount"))
	}

	contract := apitest.New(t, reg)
	for body, want := range map[string]string{
		`{"discount":"12.50 MXN"}`: `{"discount":"12.50 MXN","total":"10.00 MXN"}`,
		`{"discount":15}`:          `{"discount":15,"total":"10.00 MXN"}`,
	} {
		fake := &fakeTB{TB: t}
		req := httptest.NewRequest(http.MethodPost, "/discounts", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := contract.Do(fake, e, "discounts.apply", req)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.JSONEq(t, want, rec.Body.String())
		require.Empty(t, fake.errors)
	}

	s := &server{e: e, reg: reg}
	for _, body := range []string{`{"discount":101}`, `{"discount":"12.5 MXN"}`, `{"discount":null}`, `{"discount":{"Percent":5}}`} {
		rec := s.do(http.MethodPost, "/discounts", body)
		requireError(t, rec, http.StatusBadRequest, "discount")
	}
}

// Points writes itself as a number; its JSONSchema method has a pointer
// receiver.
type Points struct {
	N int
}

func (p Points) MarshalJSON() ([]byte, error) {
	return json.Marshal(p.N)
}

func (*Points) JSONSchema(refs *api.SchemaRefs) string {
	return `{"type": "integer", "minimum": 0}`
}

type moneyResponse struct {
	Price       Money                 `json:"price"`
	Maybe       *Money                `json:"maybe"`
	Omitted     *Money                `json:"omitted,omitempty"`
	List        []Money               `json:"list"`
	ByCode      map[string]Money      `json:"by_code"`
	Optional    types.Optional[Money] `json:"optional,omitzero"`
	Points      Points                `json:"points"`
	MaybePoints *Points               `json:"maybe_points"`
}

// TestSchemaProviderResponse: a type with a JSONSchema method is a component
// that every place of a response refers to; a pointer or a types.Optional
// adds null as anyOf [ref, null] (nullable allOf [ref] in 3.0) unless
// omitempty keeps null from being sent; both documents and the strict
// contract carry it, and the contract checks a response against it.
func TestSchemaProviderResponse(t *testing.T) {
	e := newEcho()
	reg := api.New(e, api.Public())
	res := moneyResponse{
		Price:    Money{Cents: 1250, Currency: "MXN"},
		List:     []Money{{Cents: 1, Currency: "USD"}},
		ByCode:   map[string]Money{"eur": {Cents: 100, Currency: "EUR"}},
		Optional: types.Some(Money{Cents: 5, Currency: "MXN"}),
		Points:   Points{N: 3},
	}
	api.GET(reg.Group("/prices"), "", "get", func(ctx context.Context, req *fixture.HealthRequest) (moneyResponse, error) {
		return res, nil
	})
	api.GET(reg.Group("/price"), "", "get", func(ctx context.Context, req *fixture.HealthRequest) (Money, error) {
		return Money{Cents: 1250, Currency: "MXN"}, nil
	})
	reg.Ready()

	pointsRef := map[string]any{"$ref": "#/components/schemas/api_test.Points"}
	maybe := map[string]any{"anyOf": []any{moneyRef, nullSchema}}
	strict := decode(t, reg.StrictSpec())
	raw, err := reg.Spec(api.V31)
	require.NoError(t, err)
	for _, doc := range []map[string]any{decode(t, raw), strict} {
		require.Equal(t, moneySchema, dig(t, doc, "components", "schemas", "api_test.Money"))
		require.Equal(t, map[string]any{"type": "integer", "minimum": 0.0}, dig(t, doc, "components", "schemas", "api_test.Points"))
		props := dig(t, doc, "components", "schemas", "api_test.moneyResponse", "properties")
		require.Equal(t, moneyRef, dig(t, props, "price"))
		require.Equal(t, maybe, dig(t, props, "maybe"))
		require.Equal(t, moneyRef, dig(t, props, "omitted"))
		require.Equal(t, map[string]any{"type": []any{"array", "null"}, "items": moneyRef}, dig(t, props, "list"))
		require.Equal(t, map[string]any{"type": []any{"object", "null"}, "additionalProperties": moneyRef}, dig(t, props, "by_code"))
		require.Equal(t, maybe, dig(t, props, "optional"))
		require.Equal(t, pointsRef, dig(t, props, "points"))
		require.Equal(t, map[string]any{"anyOf": []any{pointsRef, nullSchema}}, dig(t, props, "maybe_points"))
		require.Equal(t, moneyRef, dig(t, doc, "paths", "/price", "get", "responses", "200", "content", "application/json", "schema"))
	}
	raw, err = reg.Spec(api.V30)
	require.NoError(t, err)
	require.NotContains(t, string(raw), `"null"`)
	props := dig(t, decode(t, raw), "components", "schemas", "api_test.moneyResponse", "properties")
	require.Equal(t, map[string]any{"nullable": true, "allOf": []any{moneyRef}}, dig(t, props, "maybe"))

	contract := apitest.New(t, reg)
	fake := &fakeTB{TB: t}
	rec := contract.Do(fake, e, "prices.get", httptest.NewRequest(http.MethodGet, "/prices", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `{"price":"12.50 MXN","maybe":null,"list":["0.01 USD"],"by_code":{"eur":"1.00 EUR"},"optional":"0.05 MXN","points":3,"maybe_points":null}`, rec.Body.String())
	require.Empty(t, fake.errors)

	rec = contract.Do(fake, e, "price.get", httptest.NewRequest(http.MethodGet, "/price", nil))
	require.JSONEq(t, `"12.50 MXN"`, rec.Body.String())
	require.Empty(t, fake.errors)

	// The fields' shape, which the type never sends, fails the contract.
	fake = &fakeTB{TB: t}
	contract.AssertResponse(fake, "prices.get", http.StatusOK, []byte(`{"price":{"Cents":1250,"Currency":"MXN"},"maybe":null,"list":null,"by_code":null,"points":3,"maybe_points":null}`))
	require.Len(t, fake.errors, 1)
	require.Contains(t, fake.errors[0], "price")
}

// Liar says it is an integer and writes a string.
type Liar struct{}

func (Liar) MarshalJSON() ([]byte, error) {
	return []byte(`"x"`), nil
}

func (Liar) JSONSchema(refs *api.SchemaRefs) string {
	return `{"type": "integer"}`
}

type liarResponse struct {
	V Liar `json:"v"`
}

// TestSchemaProviderLiar: the contract fails a type that writes something
// other than what its JSONSchema method says.
func TestSchemaProviderLiar(t *testing.T) {
	e := newEcho()
	reg := api.New(e, api.Public())
	api.GET[fixture.HealthRequest, liarResponse](reg.Group("/liars"), "", "get", stub)
	reg.Ready()

	fake := &fakeTB{TB: t}
	rec := apitest.New(t, reg).Do(fake, e, "liars.get", httptest.NewRequest(http.MethodGet, "/liars", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `{"v":"x"}`, rec.Body.String())
	require.Len(t, fake.errors, 1)
	require.Contains(t, fake.errors[0], "v must be integer")
}

type moneyRequest struct {
	requests.NoRules
	Price    Money                 `json:"price" required:"true"`
	Maybe    *Money                `json:"maybe"`
	List     []Money               `json:"list" minItems:"1"`
	Optional types.Optional[Money] `json:"optional"`
}

// TestSchemaProviderRequest: a body value is validated against the schema
// the type states, and one that passes reaches the handler decoded by the
// type's own decoder. A pointer accepts null unless it is required.
func TestSchemaProviderRequest(t *testing.T) {
	e := newEcho()
	reg := api.New(e, api.Public())
	var got *moneyRequest
	api.POST(reg.Group("/payments"), "", "create", func(ctx context.Context, req *moneyRequest) (nodeResponse, error) {
		got = req
		return nodeResponse{OK: true}, nil
	})
	reg.Ready()
	s := &server{e: e, reg: reg}

	raw, err := reg.Spec(api.V31)
	require.NoError(t, err)
	props := dig(t, decode(t, raw), "components", "schemas", "api_test.moneyRequest", "properties")
	require.Equal(t, moneyRef, dig(t, props, "price"))
	require.Equal(t, map[string]any{"anyOf": []any{moneyRef, nullSchema}}, dig(t, props, "maybe"))

	rec := s.do(http.MethodPost, "/payments", `{"price":"12.50 MXN","maybe":"1.00 USD","list":["0.01 EUR"],"optional":"0.05 MXN"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, Money{Cents: 1250, Currency: "MXN"}, got.Price)
	require.Equal(t, &Money{Cents: 100, Currency: "USD"}, got.Maybe)
	require.Equal(t, []Money{{Cents: 1, Currency: "EUR"}}, got.List)
	optional, ok := got.Optional.Get()
	require.True(t, ok)
	require.Equal(t, Money{Cents: 5, Currency: "MXN"}, optional)

	rec = s.do(http.MethodPost, "/payments", `{"price":"1.00 MXN","maybe":null,"optional":null}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Nil(t, got.Maybe)

	for body, want := range map[string]string{
		`{"price":"12.5 MXN"}`:                  "price must match the expected format",
		`{"price":{"Cents":1250}}`:              "price must be string",
		`{"price":null}`:                        "price must be string",
		`{"price":"1.00 MXN","list":["x"]}`:     "list.0 must match the expected format",
		`{"price":"1.00 MXN","maybe":"1 MXN"}`:  "maybe",
		`{"price":"1.00 MXN","optional":"MXN"}`: "optional",
		`{"price":"1.00 MXN","list":[]}`:        "list must have at least 1 item",
		`{"price":"1.00 MXN","maybe":{"N":1}}`:  "maybe",
		`{"price":"1.00 MXN","list":[1]}`:       "list.0 must be string",
	} {
		rec = s.do(http.MethodPost, "/payments", body)
		requireError(t, rec, http.StatusBadRequest, want)
	}
}

// Choice is a string or an integer, and says so with no type.
type Choice struct {
	raw json.RawMessage
}

func (c Choice) MarshalJSON() ([]byte, error) {
	if c.raw == nil {
		return []byte(`"none"`), nil
	}
	return c.raw, nil
}

func (c *Choice) UnmarshalJSON(raw []byte) error {
	c.raw = append(json.RawMessage(nil), raw...)
	return nil
}

func (Choice) JSONSchema(refs *api.SchemaRefs) string {
	return `{"anyOf": [{"type": "string"}, {"type": "integer"}]}`
}

// Blank is text or null, and says so in its type.
type Blank struct {
	raw json.RawMessage
}

func (b Blank) MarshalJSON() ([]byte, error) {
	if b.raw == nil {
		return []byte(`null`), nil
	}
	return b.raw, nil
}

func (b *Blank) UnmarshalJSON(raw []byte) error {
	b.raw = append(json.RawMessage(nil), raw...)
	return nil
}

func (Blank) JSONSchema(refs *api.SchemaRefs) string {
	return `{"type": ["string", "null"]}`
}

// Maybe is a string, an integer or null, and says so with a null member.
type Maybe struct {
	raw json.RawMessage
}

func (m Maybe) MarshalJSON() ([]byte, error) {
	if m.raw == nil {
		return []byte(`null`), nil
	}
	return m.raw, nil
}

func (m *Maybe) UnmarshalJSON(raw []byte) error {
	m.raw = append(json.RawMessage(nil), raw...)
	return nil
}

func (Maybe) JSONSchema(refs *api.SchemaRefs) string {
	return `{"description": "A string, an integer or nothing.", "anyOf": [{"type": "string"}, {"type": "integer"}, {"type": "null"}]}`
}

type nullRequest struct {
	requests.NoRules
	C      Choice                 `json:"c" required:"true"`
	P      *Choice                `json:"p"`
	Sure   *Choice                `json:"sure" required:"true"`
	O      types.Optional[Choice] `json:"o"`
	Items  []*Choice              `json:"items"`
	Values map[string]*Choice     `json:"values"`
	B      Blank                  `json:"b"`
	M      Maybe                  `json:"m"`
}

type nullResponse struct {
	P      *Choice                `json:"p"`
	O      types.Optional[Choice] `json:"o,omitzero"`
	Items  []*Choice              `json:"items"`
	Values map[string]*Choice     `json:"values"`
	B      Blank                  `json:"b"`
	M      Maybe                  `json:"m"`
}

// TestSchemaProviderNull: null written in the text, null a pointer or a
// types.Optional adds, and a schema with no type are published so that the
// 3.1 document, the 3.0 document (no null type) and the validator agree.
func TestSchemaProviderNull(t *testing.T) {
	e := newEcho()
	reg := api.New(e, api.Public())
	api.POST(reg.Group("/choices"), "", "create", func(ctx context.Context, req *nullRequest) (nullResponse, error) {
		return nullResponse{O: types.Null[Choice](), Items: []*Choice{nil}, Values: map[string]*Choice{"k": nil}}, nil
	})
	reg.Ready()

	choiceRef := map[string]any{"$ref": "#/components/schemas/api_test.Choice"}
	raw, err := reg.Spec(api.V31)
	require.NoError(t, err)
	doc := decode(t, raw)
	schemas := dig(t, doc, "components", "schemas")
	require.Equal(t, map[string]any{"type": []any{"string", "null"}}, dig(t, schemas, "api_test.Blank"))
	require.Equal(t, map[string]any{"description": "A string, an integer or nothing.", "anyOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "integer"}, nullSchema}}, dig(t, schemas, "api_test.Maybe"))
	props := dig(t, schemas, "api_test.nullResponse", "properties")
	require.Equal(t, map[string]any{"anyOf": []any{choiceRef, nullSchema}}, dig(t, props, "p"))
	require.Equal(t, map[string]any{"anyOf": []any{choiceRef, nullSchema}}, dig(t, props, "items", "items"))

	raw, err = reg.Spec(api.V30)
	require.NoError(t, err)
	require.NotContains(t, string(raw), `"null"`)
	doc = decode(t, raw)
	schemas = dig(t, doc, "components", "schemas")
	require.Equal(t, map[string]any{"type": "string", "nullable": true}, dig(t, schemas, "api_test.Blank"))
	require.Equal(t, map[string]any{"description": "A string, an integer or nothing.", "anyOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "integer"}}, "nullable": true}, dig(t, schemas, "api_test.Maybe"))
	nullableChoice := map[string]any{"nullable": true, "allOf": []any{choiceRef}}
	for _, name := range []string{"api_test.nullRequest", "api_test.nullResponse"} {
		props = dig(t, schemas, name, "properties")
		require.Equal(t, nullableChoice, dig(t, props, "p"), name)
		require.Equal(t, nullableChoice, dig(t, props, "o"), name)
		require.Equal(t, nullableChoice, dig(t, props, "items", "items"), name)
		require.Equal(t, nullableChoice, dig(t, props, "values", "additionalProperties"), name)
	}

	s := &server{e: e, reg: reg}
	rec := s.do(http.MethodPost, "/choices", `{"c":1,"sure":"a","p":null,"o":null,"items":[null,"a"],"values":{"k":null},"b":null,"m":null}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	for body, want := range map[string]string{
		`{"c":null,"sure":"a"}`:             "c",
		`{"c":true,"sure":"a"}`:             "c",
		`{"c":1,"sure":null}`:               "sure",
		`{"c":1,"sure":"a","items":[true]}`: "items.0",
	} {
		rec = s.do(http.MethodPost, "/choices", body)
		requireError(t, rec, http.StatusBadRequest, want)
	}

	fake := &fakeTB{TB: t}
	req := httptest.NewRequest(http.MethodPost, "/choices", strings.NewReader(`{"c":1,"sure":2}`))
	req.Header.Set("Content-Type", "application/json")
	rec = apitest.New(t, reg).Do(fake, e, "choices.create", req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.JSONEq(t, `{"p":null,"o":null,"items":[null],"values":{"k":null},"b":null,"m":null}`, rec.Body.String())
	require.Empty(t, fake.errors)
}

// textOf is a JSONSchema text, held by a type so that Texted can return it.
type textOf interface {
	text() string
}

// Texted codes its own JSON and returns the text of T.
type Texted[T textOf] struct{}

func (Texted[T]) MarshalJSON() ([]byte, error) {
	return []byte(`"x"`), nil
}

func (*Texted[T]) UnmarshalJSON([]byte) error {
	return nil
}

func (Texted[T]) JSONSchema(refs *api.SchemaRefs) string {
	var text T
	return text.text()
}

type textedResponse[T textOf] struct {
	V Texted[T] `json:"v"`
}

// refusedText returns the registration error of a response holding
// Texted[T], after the type's name.
func refusedText[T textOf](t *testing.T) string {
	t.Helper()
	reg := api.New(newEcho(), api.Public())
	msg := panicMessage(t, func() {
		api.GET[fixture.HealthRequest, textedResponse[T]](reg.Group("/things"), "", "op", stub)
	})
	prefix := `api: GET /things (things.op): openapi: ` + reflect.TypeFor[Texted[T]]().String() + ": "
	require.True(t, strings.HasPrefix(msg, prefix), msg)
	return strings.TrimPrefix(msg, prefix)
}

type (
	notJSON          struct{}
	twoValues        struct{}
	notObject        struct{}
	twiceKey         struct{}
	badUTF8          struct{}
	emptyText        struct{}
	unknownKeyword   struct{}
	booleanSchema    struct{}
	unknownType      struct{}
	binaryFormat     struct{}
	badPattern       struct{}
	inexactNumber    struct{}
	fractionCount    struct{}
	nullableKeyword  struct{}
	nullConst        struct{}
	nullDefault      struct{}
	bothExamples     struct{}
	emptyEnum        struct{}
	handRef          struct{}
	nullAlone        struct{}
	nullInOneOf      struct{}
	nullOnlyAnyOf    struct{}
	twoNulls         struct{}
	typedNullHolder  struct{}
	sharedKey        struct{}
	twoTypes         struct{}
	describedNull    struct{}
	duplicateRequire struct{}
	inexactValue     struct{}
	renamedProperty  struct{}
	renamedRequired  struct{}
	twoExamples      struct{}
	formatEncoding   struct{}
	rewrittenX       struct{}
	rewrittenExample struct{}
	nullEnumMember   struct{}
	nullConstMember  struct{}
	untypedNullEnum  struct{}
	emptyTypeList    struct{}
	typeTwice        struct{}
	emptyAllOf       struct{}
	emptyAnyOf       struct{}
	emptyOneOf       struct{}
	negativeCount    struct{}
	zeroMultipleOf   struct{}
	numberRequired   struct{}
)

func (notJSON) text() string        { return `{"type": "string"` }
func (twoValues) text() string      { return `{} {}` }
func (notObject) text() string      { return `["string"]` }
func (twiceKey) text() string       { return `{"properties": {"a": {"type": "string", "type": "integer"}}}` }
func (badUTF8) text() string        { return "{\"description\": \"\xff\"}" }
func (emptyText) text() string      { return `` }
func (unknownKeyword) text() string { return `{"type": "object", "properties": {"a/b": {"if": {}}}}` }
func (booleanSchema) text() string  { return `{"type": "array", "items": true}` }
func (unknownType) text() string    { return `{"type": "strng"}` }
func (binaryFormat) text() string   { return `{"type": "string", "format": "binary"}` }
func (badPattern) text() string {
	return `{"type": "array", "items": {"type": "string", "pattern": "(["}}`
}
func (inexactNumber) text() string   { return `{"type": "integer", "maximum": 9007199254740993}` }
func (fractionCount) text() string   { return `{"type": "string", "minLength": 1.5}` }
func (nullableKeyword) text() string { return `{"type": "string", "nullable": true}` }
func (nullConst) text() string       { return `{"const": null}` }
func (nullDefault) text() string     { return `{"type": "string", "default": null}` }
func (bothExamples) text() string    { return `{"type": "string", "example": "a", "examples": ["b"]}` }
func (emptyEnum) text() string       { return `{"type": "string", "enum": []}` }
func (handRef) text() string         { return `{"$ref": "#/components/schemas/api_test.InvoiceDetail"}` }
func (nullAlone) text() string       { return `{"type": "null"}` }
func (nullInOneOf) text() string     { return `{"oneOf": [{"type": "string"}, {"type": "null"}]}` }
func (nullOnlyAnyOf) text() string   { return `{"anyOf": [{"type": "null"}]}` }
func (twoNulls) text() string {
	return `{"anyOf": [{"type": "string"}, {"type": "null"}, {"type": "null"}]}`
}
func (typedNullHolder) text() string {
	return `{"type": "string", "anyOf": [{"maxLength": 3}, {"type": "null"}]}`
}
func (sharedKey) text() string {
	return `{"description": "a", "anyOf": [{"type": "string", "description": "b"}, {"type": "null"}]}`
}
func (twoTypes) text() string { return `{"type": ["string", "integer"]}` }
func (describedNull) text() string {
	return `{"anyOf": [{"type": "string"}, {"type": "null", "description": "none"}]}`
}
func (duplicateRequire) text() string { return `{"type": "object", "required": ["a", "a"]}` }
func (inexactValue) text() string {
	return `{"type": "object", "example": {"n": [9007199254740993]}}`
}
func (renamedProperty) text() string {
	return `{"type": "object", "properties": {"contentEncoding": {"type": "string"}}}`
}
func (renamedRequired) text() string { return `{"type": "object", "required": ["contentMediaType"]}` }
func (twoExamples) text() string     { return `{"type": "string", "examples": ["a", "b"]}` }
func (formatEncoding) text() string {
	return `{"type": "string", "format": "byte", "contentEncoding": "base64"}`
}
func (rewrittenX) text() string { return `{"type": "object", "x-meta": {"exclusiveMinimum": 3}}` }
func (rewrittenExample) text() string {
	return `{"type": "object", "example": {"kind": {"type": ["a", "null"]}}}`
}
func (nullEnumMember) text() string {
	return `{"anyOf": [{"type": "string", "enum": ["a", "b"]}, {"type": "null"}]}`
}
func (nullConstMember) text() string {
	return `{"anyOf": [{"type": "integer"}, {"const": "a"}, {"type": "null"}]}`
}
func (untypedNullEnum) text() string { return `{"enum": ["a", null]}` }
func (emptyTypeList) text() string   { return `{"type": []}` }
func (typeTwice) text() string       { return `{"type": ["string", "string"]}` }
func (emptyAllOf) text() string      { return `{"allOf": []}` }
func (emptyAnyOf) text() string      { return `{"anyOf": []}` }
func (emptyOneOf) text() string      { return `{"oneOf": []}` }
func (negativeCount) text() string   { return `{"type": "array", "maxItems": -1}` }
func (zeroMultipleOf) text() string  { return `{"type": "number", "multipleOf": 0}` }
func (numberRequired) text() string  { return `{"type": "object", "required": ["a", 1]}` }

// TestSchemaProviderText: registration fails, naming the type and the JSON
// pointer of the place, for a text that is not one JSON object or holds a
// key twice, for a keyword or value the registry would not publish and
// validate as written, for null written in a way OpenAPI 3.0 cannot carry,
// and for what the OpenAPI 3.0 document would carry otherwise than written.
func TestSchemaProviderText(t *testing.T) {
	const nullEnumAdvice = `write a type list with null in the enum, as {"type": ["string", "null"], "enum": ["a", "b", null]}`
	const rewritten = `is not supported in this value: the OpenAPI 3.0 document would rewrite it as if the value were a schema; leave it out of the value`
	const holderOnly = `a schema with an anyOf that holds null may hold only annotations (title, description, default, examples, example, deprecated, readOnly, writeOnly, x-) beside it, not "type": OpenAPI 3.0 writes it as nullable, which would let null past "type"`
	const nullType = `a schema of type null alone is not supported, as OpenAPI 3.0 has no null type: write null in a type list, as ["string", "null"], or as a member {"type": "null"} of an anyOf`
	for _, tt := range []struct {
		name string
		got  func(t *testing.T) string
		want string
	}{
		{"not JSON", refusedText[notJSON], `the JSONSchema text: it is not valid JSON: unexpected end of JSON input`},
		{"two values", refusedText[twoValues], `the JSONSchema text: it holds more after the JSON value`},
		{"not an object", refusedText[notObject], `the JSONSchema text: a schema must be a JSON object, not an array`},
		{"key twice", refusedText[twiceKey], `the JSONSchema text at /properties/a: the key "type" appears twice`},
		{"bad UTF-8", refusedText[badUTF8], `the JSONSchema text: it is not valid UTF-8`},
		{"empty", refusedText[emptyText], `the JSONSchema text: it is not valid JSON: unexpected end of JSON input`},
		{"unknown keyword", refusedText[unknownKeyword], `the JSONSchema text at /properties/a~1b/if: "if" is not a keyword the registry publishes and validates (see api.SchemaProvider)`},
		{"boolean schema", refusedText[booleanSchema], `the JSONSchema text at /items: a schema must be a JSON object, not a boolean`},
		{"unknown type", refusedText[unknownType], `the JSONSchema text at /type: "strng" is not a JSON Schema type (string, number, integer, boolean, array, object or null)`},
		{"binary format", refusedText[binaryFormat], `the JSONSchema text at /format: the format binary is not supported: the document would add a contentMediaType the text does not hold`},
		{"bad pattern", refusedText[badPattern], "the JSONSchema text at /items/pattern: the pattern is not a valid regular expression: error parsing regexp: missing closing ]: `[`"},
		{"inexact number", refusedText[inexactNumber], `the JSONSchema text at /maximum: the number 9007199254740993 cannot be published as written, as a float64 does not hold it`},
		{"fraction count", refusedText[fractionCount], `the JSONSchema text at /minLength: it must be a whole number of 0 or more, written without a fraction or an exponent, not 1.5`},
		{"nullable keyword", refusedText[nullableKeyword], `the JSONSchema text at /nullable: nullable is not a JSON Schema keyword (it is OpenAPI 3.0's); write null in the type, as ["string", "null"]`},
		{"null const", refusedText[nullConst], `the JSONSchema text at /const: a const of null is not supported: the document would leave it out; write {"type": "null"} as a member of an anyOf`},
		{"null default", refusedText[nullDefault], `the JSONSchema text at /default: a default of null is not supported: the document would leave it out`},
		{"both examples", refusedText[bothExamples], `the JSONSchema text at /example: example and examples together are not supported: the OpenAPI 3.0 document has room for one of them`},
		{"empty enum", refusedText[emptyEnum], `the JSONSchema text at /enum: an empty enum is not supported: the document would leave it out and accept any value`},
		{"required twice", refusedText[duplicateRequire], `the JSONSchema text at /required: the property "a" is listed twice`},
		{"hand-written $ref", refusedText[handRef], `the JSONSchema text at /$ref: the $ref #/components/schemas/api_test.InvoiceDetail is not one refs.For returned during this call; refer to another type with refs.For(reflect.TypeFor[T]())`},
		{"null alone", refusedText[nullAlone], `the JSONSchema text at /type: ` + nullType},
		{"null in oneOf", refusedText[nullInOneOf], `the JSONSchema text at /oneOf/1/type: ` + nullType},
		{"described null", refusedText[describedNull], `the JSONSchema text at /anyOf/1/type: ` + nullType},
		{"anyOf of null only", refusedText[nullOnlyAnyOf], `the JSONSchema text at /anyOf: an anyOf with a null member needs exactly one {"type": "null"} and at least one other member`},
		{"two nulls", refusedText[twoNulls], `the JSONSchema text at /anyOf: an anyOf with a null member needs exactly one {"type": "null"} and at least one other member`},
		{"typed null holder", refusedText[typedNullHolder], `the JSONSchema text at /anyOf: ` + holderOnly},
		{"shared key", refusedText[sharedKey], `the JSONSchema text at /anyOf: the schema and the one member of its anyOf other than null both hold "description", which the OpenAPI 3.0 document merges into one`},
		{"two types", refusedText[twoTypes], `the JSONSchema text at /type: a type list of more than one type besides null is not supported, as OpenAPI 3.0 has none; use an anyOf`},
		{"inexact value", refusedText[inexactValue], `the JSONSchema text at /example/n/0: the number 9007199254740993 cannot be published as written, as a float64 does not hold it`},
		{"renamed property", refusedText[renamedProperty], `the JSONSchema text at /properties/contentEncoding: the property name "contentEncoding" is renamed by the 3.0 downgrade; use another name`},
		{"renamed required", refusedText[renamedRequired], `the JSONSchema text at /required/0: the property name "contentMediaType" is renamed by the 3.0 downgrade; use another name`},
		{"two examples", refusedText[twoExamples], `the JSONSchema text at /examples: examples with more than one item are not supported: the OpenAPI 3.0 document has room for one of them`},
		{"format and encoding", refusedText[formatEncoding], `the JSONSchema text at /format: format and contentEncoding together are not supported: the OpenAPI 3.0 document writes a contentEncoding of base64 as the format, in place of this one; use one of them`},
		{"rewritten x- value", refusedText[rewrittenX], `the JSONSchema text at /x-meta/exclusiveMinimum: the key "exclusiveMinimum" ` + rewritten},
		{"rewritten example", refusedText[rewrittenExample], `the JSONSchema text at /example/kind/type: the key "type" ` + rewritten},
		{"enum beside null", refusedText[nullEnumMember], `the JSONSchema text at /anyOf/0/enum: enum in a member of an anyOf with a null member is not supported: the OpenAPI 3.0 document writes the null member as nullable: true, which does not let null past the enum; ` + nullEnumAdvice},
		{"const beside null", refusedText[nullConstMember], `the JSONSchema text at /anyOf/1/const: const in a member of an anyOf with a null member is not supported: the OpenAPI 3.0 document writes the null member as nullable: true, which does not let null past the const; ` + nullEnumAdvice},
		{"null enum without null type", refusedText[untypedNullEnum], `the JSONSchema text at /enum: an enum that lists null is not supported unless the type lists null too: the OpenAPI 3.0 document carries the enum without nullable: true, which refuses null; ` + nullEnumAdvice},
		{"empty type list", refusedText[emptyTypeList], `the JSONSchema text at /type: the type list is empty`},
		{"type twice", refusedText[typeTwice], `the JSONSchema text at /type: the type string is listed twice`},
		{"empty allOf", refusedText[emptyAllOf], `the JSONSchema text at /allOf: it needs at least one schema`},
		{"empty anyOf", refusedText[emptyAnyOf], `the JSONSchema text at /anyOf: it needs at least one schema`},
		{"empty oneOf", refusedText[emptyOneOf], `the JSONSchema text at /oneOf: it needs at least one schema`},
		{"negative count", refusedText[negativeCount], `the JSONSchema text at /maxItems: it must be a whole number of 0 or more, written without a fraction or an exponent, not -1`},
		{"multipleOf 0", refusedText[zeroMultipleOf], `the JSONSchema text at /multipleOf: multipleOf must be greater than 0`},
		{"number in required", refusedText[numberRequired], `the JSONSchema text at /required/1: required lists property names, which are strings`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, tt.got(t))
		})
	}
}

type invoiceDetailResponse struct {
	D InvoiceDetail `json:"d"`
}

type handRefResponse struct {
	V Texted[handRef] `json:"v"`
}

// RefAllOf writes a $ref refs.For returned beside an allOf.
type RefAllOf struct{}

func (RefAllOf) MarshalJSON() ([]byte, error) {
	return []byte(`{"number":"1"}`), nil
}

func (RefAllOf) JSONSchema(refs *api.SchemaRefs) string {
	ref := refs.For(reflect.TypeFor[InvoiceDetail]())
	return `{"$ref": ` + strconv.Quote(ref[len(`{"$ref":"`):len(ref)-2]) + `, "allOf": [{"type": "object"}]}`
}

type refAllOfResponse struct {
	V RefAllOf `json:"v"`
}

// TestSchemaProviderHandRef: a $ref the text writes by hand is refused even
// when another route made the component it names first; one refs.For
// returned is refused beside an allOf, which OpenAPI 3.0 cannot carry.
func TestSchemaProviderHandRef(t *testing.T) {
	reg := api.New(newEcho(), api.Public())
	api.GET[fixture.HealthRequest, invoiceDetailResponse](reg.Group("/details"), "", "get", stub)
	msg := panicMessage(t, func() {
		api.GET[fixture.HealthRequest, handRefResponse](reg.Group("/things"), "", "op", stub)
	})
	require.Contains(t, msg, `the JSONSchema text at /$ref: the $ref #/components/schemas/api_test.InvoiceDetail is not one refs.For returned during this call`)

	msg = refused(t, func(g *api.Group) { api.GET[fixture.HealthRequest, refAllOfResponse](g, "", "op", stub) })
	require.Equal(t, refusedGet+`openapi: api_test.RefAllOf: the JSONSchema text at /$ref: a $ref beside an allOf is not supported: OpenAPI 3.0 ignores the keywords beside a $ref, and its document can only move the $ref into an allOf where there is none; list the $ref in the allOf`, msg)
}

// Invoice writes itself as its InvoiceDetail, or a list of them, and says so
// with refs.For: a $ref For returned, and one inside the inline schema For
// returned for a slice.
type Invoice struct {
	detail InvoiceDetail
}

type InvoiceDetail struct {
	Number string `json:"number"`
}

func (i Invoice) MarshalJSON() ([]byte, error) {
	return json.Marshal(i.detail)
}

func (Invoice) JSONSchema(refs *api.SchemaRefs) string {
	return `{"anyOf": [` + refs.For(reflect.TypeFor[InvoiceDetail]()) + `, ` + refs.For(reflect.TypeFor[[]InvoiceDetail]()) + `]}`
}

type invoiceResponse struct {
	Invoice Invoice `json:"invoice"`
}

// TestSchemaProviderRef: a type refs.For asks for becomes a component of the
// documents that reach it, and the text may use the $refs For returned.
func TestSchemaProviderRef(t *testing.T) {
	e := newEcho()
	reg := api.New(e, api.Public())
	api.GET(reg.Group("/invoices"), "", "get", func(ctx context.Context, req *fixture.HealthRequest) (invoiceResponse, error) {
		return invoiceResponse{Invoice: Invoice{detail: InvoiceDetail{Number: "A-1"}}}, nil
	})
	reg.Ready()

	ref := map[string]any{"$ref": "#/components/schemas/api_test.InvoiceDetail"}
	for _, v := range []api.Version{api.V31, api.V30} {
		raw, err := reg.Spec(v)
		require.NoError(t, err)
		doc := decode(t, raw)
		requireRefsResolve(t, doc)
		require.Equal(t, map[string]any{"type": "object", "properties": map[string]any{"number": map[string]any{"type": "string"}}, "required": []any{"number"}}, dig(t, doc, "components", "schemas", "api_test.InvoiceDetail"))
	}
	raw, err := reg.Spec(api.V31)
	require.NoError(t, err)
	require.Equal(t, map[string]any{"anyOf": []any{ref, map[string]any{"type": []any{"array", "null"}, "items": ref}}}, dig(t, decode(t, raw), "components", "schemas", "api_test.Invoice"))

	fake := &fakeTB{TB: t}
	rec := apitest.New(t, reg).Do(fake, e, "invoices.get", httptest.NewRequest(http.MethodGet, "/invoices", nil))
	require.JSONEq(t, `{"invoice":{"number":"A-1"}}`, rec.Body.String())
	require.Empty(t, fake.errors)
}

// Account has a pointer field, so its request shape differs from its
// response shape.
type Account struct {
	Number string  `json:"number"`
	Note   *string `json:"note"`
}

// Transfer reads and writes itself as an Account.
type Transfer struct {
	account Account
}

func (t Transfer) MarshalJSON() ([]byte, error) {
	return json.Marshal(t.account)
}

func (t *Transfer) UnmarshalJSON(raw []byte) error {
	return json.Unmarshal(raw, &t.account)
}

func (Transfer) JSONSchema(refs *api.SchemaRefs) string {
	return refs.For(reflect.TypeFor[Account]())
}

type transferRequest struct {
	requests.NoRules
	T Transfer `json:"t" required:"true"`
}

type transferResponse struct {
	T Transfer `json:"t"`
}

// TestSchemaProviderVariant: a request reaches the request shape of the
// struct a JSONSchema method refers to, through a request variant of the
// type's component, so a response's required keys are not asked of it.
func TestSchemaProviderVariant(t *testing.T) {
	e := newEcho()
	reg := api.New(e, api.Public())
	api.POST(reg.Group("/transfers"), "", "create", func(ctx context.Context, req *transferRequest) (transferResponse, error) {
		return transferResponse{T: req.T}, nil
	})
	reg.Ready()

	raw, err := reg.Spec(api.V31)
	require.NoError(t, err)
	schemas := dig(t, decode(t, raw), "components", "schemas")
	require.Equal(t, map[string]any{"$ref": "#/components/schemas/api_test.Account"}, dig(t, schemas, "api_test.Transfer"))
	require.Equal(t, map[string]any{"$ref": "#/components/schemas/api_test.AccountInput"}, dig(t, schemas, "api_test.TransferInput"))
	require.Equal(t, map[string]any{"$ref": "#/components/schemas/api_test.TransferInput"}, dig(t, schemas, "api_test.transferRequest", "properties", "t"))

	s := &server{e: e, reg: reg}
	rec := s.do(http.MethodPost, "/transfers", `{"t":{"number":"1"}}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.JSONEq(t, `{"t":{"number":"1","note":null}}`, rec.Body.String())
}

// Sticker is an object its text closes; Badge one its text leaves open;
// Pair an allOf of two inline objects. Each writes the JSON it holds.
type Sticker struct {
	raw json.RawMessage
}

func (s Sticker) MarshalJSON() ([]byte, error) {
	return s.raw, nil
}

func (s *Sticker) UnmarshalJSON(raw []byte) error {
	s.raw = append(json.RawMessage(nil), raw...)
	return nil
}

const stickerText = `{"title": "A sticker", "type": "object", "properties": {"a": {"type": "string", "readOnly": true}}, "required": ["a"], "additionalProperties": false, "x-kind": "sticker", "example": {"a": "x"}, "default": {"a": "y"}, "maxProperties": 3}`

func (Sticker) JSONSchema(refs *api.SchemaRefs) string {
	return stickerText
}

type Badge struct {
	raw json.RawMessage
}

func (b Badge) MarshalJSON() ([]byte, error) {
	return b.raw, nil
}

func (b *Badge) UnmarshalJSON(raw []byte) error {
	b.raw = append(json.RawMessage(nil), raw...)
	return nil
}

func (Badge) JSONSchema(refs *api.SchemaRefs) string {
	return `{"type": "object", "properties": {"a": {"type": "string"}}}`
}

type Pair struct {
	raw json.RawMessage
}

func (p Pair) MarshalJSON() ([]byte, error) {
	return p.raw, nil
}

func (Pair) JSONSchema(refs *api.SchemaRefs) string {
	return `{"allOf": [{"type": "object", "properties": {"a": {"type": "string"}}, "required": ["a"]}, {"type": "object", "properties": {"b": {"type": "integer"}}}]}`
}

type stickerRequest struct {
	requests.NoRules
	S *Sticker `json:"s"`
	B *Badge   `json:"b"`
}

type stickerResponse struct {
	S Sticker `json:"s"`
	B Badge   `json:"b"`
	P Pair    `json:"p"`
}

// TestSchemaProviderAsWritten: the text is published, validated and checked
// in the strict contract as written: its additionalProperties false stays
// and refuses an extra key, an object it leaves open stays open even in the
// strict contract, and an allOf that holds both members' keys passes.
func TestSchemaProviderAsWritten(t *testing.T) {
	e := newEcho()
	reg := api.New(e, api.Public())
	api.POST[stickerRequest, nodeResponse](reg.Group("/stickers"), "", "create", stub)
	api.GET(reg.Group("/stickers"), "", "get", func(ctx context.Context, req *fixture.HealthRequest) (stickerResponse, error) {
		return stickerResponse{
			S: Sticker{raw: json.RawMessage(`{"a":"x","extra":1}`)},
			B: Badge{raw: json.RawMessage(`{"a":"x","extra":1}`)},
			P: Pair{raw: json.RawMessage(`{"a":"x","b":1}`)},
		}, nil
	})
	reg.Ready()

	var want map[string]any
	err := json.Unmarshal([]byte(stickerText), &want)
	require.NoError(t, err)
	raw, err := reg.Spec(api.V31)
	require.NoError(t, err)
	for _, doc := range []map[string]any{decode(t, raw), decode(t, reg.StrictSpec())} {
		require.Equal(t, want, dig(t, doc, "components", "schemas", "api_test.Sticker"))
		require.Equal(t, map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}}}, dig(t, doc, "components", "schemas", "api_test.Badge"))
	}

	s := &server{e: e, reg: reg}
	rec := s.do(http.MethodPost, "/stickers", `{"s":{"a":"x"},"b":{"a":"x","extra":1}}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = s.do(http.MethodPost, "/stickers", `{"s":{"a":"x","extra":1}}`)
	requireError(t, rec, http.StatusBadRequest, "s")

	fake := &fakeTB{TB: t}
	rec = apitest.New(t, reg).Do(fake, e, "stickers.get", httptest.NewRequest(http.MethodGet, "/stickers", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, fake.errors, 1)
	require.Contains(t, fake.errors[0], "s")
	require.Contains(t, fake.errors[0], "extra")
	require.NotContains(t, fake.errors[0], "b.extra")
}

// Stamp writes its own JSON and does not say what.
type Stamp struct {
	At int64
}

func (s Stamp) MarshalJSON() ([]byte, error) {
	return json.Marshal(fmt.Sprint(s.At))
}

// PointerStamp writes its own JSON through a pointer receiver and does not
// say what.
type PointerStamp struct {
	At int64
}

func (s *PointerStamp) MarshalJSON() ([]byte, error) {
	return json.Marshal(fmt.Sprint(s.At))
}

// Cents writes its own JSON but is no struct: it stays an integer.
type Cents int64

func (c Cents) MarshalJSON() ([]byte, error) {
	return json.Marshal(int64(c))
}

// Code reads and writes itself as text and has a JSONSchema method.
type Code struct {
	V string
}

func (c Code) MarshalText() ([]byte, error) {
	return []byte(c.V), nil
}

func (c *Code) UnmarshalText(text []byte) error {
	c.V = string(text)
	return nil
}

func (Code) JSONSchema(refs *api.SchemaRefs) string {
	return `{"type": "string", "pattern": "^[A-Z]+$"}`
}

// Slug is a string kind with a JSONSchema method, which reads its own text.
type Slug string

func (s *Slug) UnmarshalText(text []byte) error {
	*s = Slug(text)
	return nil
}

func (Slug) JSONSchema(refs *api.SchemaRefs) string {
	return `{"type": "string", "pattern": "^[a-z-]+$"}`
}

// Tags is a slice of strings with a JSONSchema method, which reads its own
// JSON.
type Tags []string

func (t *Tags) UnmarshalJSON(raw []byte) error {
	return json.Unmarshal(raw, (*[]string)(t))
}

func (Tags) JSONSchema(refs *api.SchemaRefs) string {
	return `{"type": "array", "items": {"type": "string"}}`
}

type codeQuery struct {
	requests.NoRules
	Code Code `json:"-" query:"code"`
}

type codesHeader struct {
	requests.NoRules
	Codes []Code `json:"-" header:"X-Codes"`
}

type codeForm struct {
	requests.NoRules
	File *multipart.FileHeader `json:"-" form:"file"`
	Code Code                  `json:"code"`
}

type moneyTagRequest struct {
	requests.NoRules
	Price Money `json:"price" maxLength:"9"`
}

type moneyItemsTagRequest struct {
	requests.NoRules
	List []Money `json:"list" enum:"1.00 MXN"`
}

type moneyOptionalItemsTagRequest struct {
	requests.NoRules
	List []types.Optional[Money] `json:"list" maxLength:"9"`
}

type slugFormatRequest struct {
	requests.NoRules
	Slug Slug `json:"slug" format:"email"`
}

type tagsFormatRequest struct {
	requests.NoRules
	Tags Tags `json:"tags" format:"email"`
}

type stampResponse struct {
	At Stamp `json:"at"`
}

type stampsResponse struct {
	At []PointerStamp `json:"at"`
}

type stampOptionalResponse struct {
	At types.Optional[map[string]Stamp] `json:"at,omitzero"`
}

type stampRequest struct {
	requests.NoRules
	At Stamp `json:"at"`
}

type centsResponse struct {
	Total Cents `json:"total"`
}

// refused registers a route on a new registry and returns the message it
// panics with.
func refused(t *testing.T, register func(g *api.Group)) string {
	t.Helper()
	reg := api.New(newEcho(), api.Public())
	return panicMessage(t, func() { register(reg.Group("/things")) })
}

const (
	refusedGet  = `api: GET /things (things.op): `
	refusedPost = `api: POST /things (things.op): `
)

// TestSchemaProviderRefusals: registration fails, naming the type, for a
// type with a JSONSchema method as a parameter or a form value, a bound,
// length, enum or format tag on one in a request (or on its items), and a
// response struct that writes its own JSON without one, wherever the
// response holds it. A request struct that writes its own JSON, and a
// response type of another kind that does, are still published by their Go
// shape.
func TestSchemaProviderRefusals(t *testing.T) {
	msg := refused(t, func(g *api.Group) { api.GET[codeQuery, nodeResponse](g, "", "op", stub) })
	require.Equal(t, refusedGet+`query parameter code: type api_test.Code has a JSONSchema method, which is not supported on a parameter: it states the schema of the type's JSON, not of the text a parameter carries (use a string, bool or number kind, an encoding.TextUnmarshaler, or a slice of those)`, msg)
	msg = refused(t, func(g *api.Group) { api.GET[codesHeader, nodeResponse](g, "", "op", stub) })
	require.Equal(t, refusedGet+`header parameter x-codes: type []api_test.Code has a JSONSchema method, which is not supported on a parameter: it states the schema of the type's JSON, not of the text a parameter carries (use a string, bool or number kind, an encoding.TextUnmarshaler, or a slice of those)`, msg)
	msg = refused(t, func(g *api.Group) { api.POST[codeForm, nodeResponse](g, "", "op", stub) })
	require.Equal(t, refusedPost+`multipart body field Code (code): type api_test.Code has a JSONSchema method, which is not supported in a form: it states the schema of the type's JSON, not of the text a form value carries (use a string, bool or number kind, an encoding.TextUnmarshaler, or a slice of those)`, msg)

	msg = refused(t, func(g *api.Group) { api.POST[moneyTagRequest, nodeResponse](g, "", "op", stub) })
	require.Equal(t, refusedPost+`openapi: api_test.moneyTagRequest.Price: the maxLength tag is not supported on api_test.Money in a request type: api_test.Money states its schema with its JSONSchema method, and the tag would be written beside that schema whatever its type, where nothing may check it; state the rule in the schema JSONSchema returns`, msg)
	msg = refused(t, func(g *api.Group) { api.POST[moneyItemsTagRequest, nodeResponse](g, "", "op", stub) })
	require.Equal(t, refusedPost+`openapi: api_test.moneyItemsTagRequest.List: the enum tag is not supported on []api_test.Money in a request type: api_test.Money states its schema with its JSONSchema method, and the tag would be written beside that schema whatever its type, where nothing may check it; state the rule in the schema JSONSchema returns`, msg)
	msg = refused(t, func(g *api.Group) { api.POST[moneyOptionalItemsTagRequest, nodeResponse](g, "", "op", stub) })
	require.Equal(t, refusedPost+`openapi: api_test.moneyOptionalItemsTagRequest.List: the maxLength tag is not supported on []types.Optional[github.com/vanclief/compose/components/rest/api_test.Money] in a request type: api_test.Money states its schema with its JSONSchema method, and the tag would be written beside that schema whatever its type, where nothing may check it; state the rule in the schema JSONSchema returns`, msg)
	msg = refused(t, func(g *api.Group) { api.POST[slugFormatRequest, nodeResponse](g, "", "op", stub) })
	require.Equal(t, refusedPost+`openapi: api_test.slugFormatRequest.Slug: the format tag is not supported on api_test.Slug, whose format is set by the type; the schema and the decoder would disagree on the value's text`, msg)
	msg = refused(t, func(g *api.Group) { api.POST[tagsFormatRequest, nodeResponse](g, "", "op", stub) })
	require.Equal(t, refusedPost+`openapi: api_test.tagsFormatRequest.Tags: the format tag is not supported on api_test.Tags, whose format is set by the type; the schema and the decoder would disagree on the value's text`, msg)

	stamp := `api_test.Stamp writes its own JSON with MarshalJSON, which the schema of its fields would not describe; add a JSONSchema method (api.SchemaProvider) that returns the schema of what MarshalJSON writes`
	msg = refused(t, func(g *api.Group) { api.GET[fixture.HealthRequest, stampResponse](g, "", "op", stub) })
	require.Equal(t, refusedGet+`openapi: `+stamp, msg)
	msg = refused(t, func(g *api.Group) { api.GET[fixture.HealthRequest, Stamp](g, "", "op", stub) })
	require.Equal(t, refusedGet+`openapi: `+stamp, msg)
	msg = refused(t, func(g *api.Group) { api.GET[fixture.HealthRequest, stampOptionalResponse](g, "", "op", stub) })
	require.Equal(t, refusedGet+`openapi: `+stamp, msg)
	msg = refused(t, func(g *api.Group) { api.GET[fixture.HealthRequest, stampsResponse](g, "", "op", stub) })
	require.Equal(t, refusedGet+`openapi: api_test.PointerStamp writes its own JSON with MarshalJSON, which the schema of its fields would not describe; add a JSONSchema method (api.SchemaProvider) that returns the schema of what MarshalJSON writes`, msg)

	reg := api.New(newEcho(), api.Public())
	api.POST[stampRequest, centsResponse](reg.Group("/things"), "", "op", stub)
	reg.Ready()
	raw, err := reg.Spec(api.V31)
	require.NoError(t, err)
	doc := decode(t, raw)
	require.Equal(t, map[string]any{"type": "integer", "format": "int64"}, dig(t, doc, "components", "schemas", "api_test.centsResponse", "properties", "total"))
	require.Contains(t, string(raw), `"api_test.Stamp"`)
}

// Shape states an object it has no coder for; Shaped gets its JSONSchema
// method by promotion.
type Shape struct {
	A string `json:"a"`
}

func (Shape) JSONSchema(refs *api.SchemaRefs) string {
	return `{"type": "object", "properties": {"a": {"type": "string"}}}`
}

type Shaped struct {
	Shape
	Extra string `json:"extra"`
}

type shapedResponse struct {
	S Shaped `json:"s"`
}

type shapedRequest struct {
	requests.NoRules
	S Shaped `json:"s"`
}

// NotedMoney gets Money's JSONSchema method and its coders by promotion.
type NotedMoney struct {
	Money
	Note string `json:"note"`
}

type notedMoneyResponse struct {
	M NotedMoney `json:"m"`
}

type notedMoneyRequest struct {
	requests.NoRules
	M NotedMoney `json:"m"`
}

// PointerShaped and NotedMoneyPtr get the JSONSchema method through an
// embedded pointer, which is nil in the value the registry would call it
// on.
type PointerShaped struct {
	*Shape
	Extra string `json:"extra"`
}

type NotedMoneyPtr struct {
	*Money
	Note string `json:"note"`
}

// StampedNote declares its own JSONSchema method and gets its JSON encoder
// from an embedded field.
type StampedNote struct {
	Stamp
	Note string `json:"note"`
}

func (StampedNote) JSONSchema(refs *api.SchemaRefs) string {
	return `{"type": "string"}`
}

type stampedNoteResponse struct {
	N StampedNote `json:"n"`
}

type pointerShapedResponse struct {
	S PointerShaped `json:"s"`
}

type notedMoneyPtrResponse struct {
	M NotedMoneyPtr `json:"m"`
}

type notedMoneyPtrRequest struct {
	requests.NoRules
	M NotedMoneyPtr `json:"m"`
}

// TestSchemaProviderPromoted: a struct that gets its JSONSchema method from
// an embedded field, by value or through a pointer, is refused in a request
// and in a response before the method is called, also when it gets its
// JSON coder from that field too; one that declares the method and gets its
// coder from an embedded field is refused for the coder.
func TestSchemaProviderPromoted(t *testing.T) {
	const shaped = `the JSONSchema method of embedded field Shape would state the schema of the whole value; make it a named field`
	msg := refused(t, func(g *api.Group) { api.GET[fixture.HealthRequest, shapedResponse](g, "", "op", stub) })
	require.Equal(t, refusedGet+`openapi: api_test.Shaped: `+shaped, msg)
	msg = refused(t, func(g *api.Group) { api.POST[shapedRequest, nodeResponse](g, "", "op", stub) })
	require.Equal(t, refusedPost+`openapi: api_test.Shaped: `+shaped, msg)
	msg = refused(t, func(g *api.Group) { api.GET[fixture.HealthRequest, pointerShapedResponse](g, "", "op", stub) })
	require.Equal(t, refusedGet+`openapi: api_test.PointerShaped: `+shaped, msg)

	const noted = `the JSONSchema method of embedded field Money would state the schema of the whole value; make it a named field`
	msg = refused(t, func(g *api.Group) { api.GET[fixture.HealthRequest, notedMoneyResponse](g, "", "op", stub) })
	require.Equal(t, refusedGet+`openapi: api_test.NotedMoney: `+noted, msg)
	msg = refused(t, func(g *api.Group) { api.POST[notedMoneyRequest, nodeResponse](g, "", "op", stub) })
	require.Equal(t, refusedPost+`openapi: api_test.NotedMoney: `+noted, msg)
	msg = refused(t, func(g *api.Group) { api.GET[fixture.HealthRequest, notedMoneyPtrResponse](g, "", "op", stub) })
	require.Equal(t, refusedGet+`openapi: api_test.NotedMoneyPtr: `+noted, msg)
	msg = refused(t, func(g *api.Group) { api.POST[notedMoneyPtrRequest, nodeResponse](g, "", "op", stub) })
	require.Equal(t, refusedPost+`openapi: api_test.NotedMoneyPtr: `+noted, msg)

	msg = refused(t, func(g *api.Group) { api.GET[fixture.HealthRequest, stampedNoteResponse](g, "", "op", stub) })
	require.Equal(t, refusedGet+`openapi: api_test.StampedNote: the JSON encoder of embedded field Stamp would take over the whole value, not the fields the schema lists; make it a named field`, msg)
}

// Amount states a schema its fields do not have, and codes no JSON of its
// own.
type Amount struct {
	Value int `json:"amt"`
}

func (Amount) JSONSchema(refs *api.SchemaRefs) string {
	return `{"type": "object", "properties": {"amount": {"type": "integer", "minimum": 0}}}`
}

// Words is a slice stated as a string, coding no JSON of its own.
type Words []string

func (Words) JSONSchema(refs *api.SchemaRefs) string {
	return `{"type": "string"}`
}

// Stars is an integer that writes itself and reads by its kind.
type Stars int

func (s Stars) MarshalJSON() ([]byte, error) {
	return json.Marshal(int(s))
}

func (Stars) JSONSchema(refs *api.SchemaRefs) string {
	return `{"type": "integer", "maximum": 5}`
}

type amountRequestBody struct {
	requests.NoRules
	A Amount `json:"a"`
}

type amountResponseBody struct {
	A Amount `json:"a"`
}

type wordsResponse struct {
	W Words `json:"w"`
}

type starsRequest struct {
	requests.NoRules
	S []Stars `json:"s"`
}

// Keywords writes itself, an empty array when nil, by a value receiver.
type Keywords []string

func (l Keywords) MarshalJSON() ([]byte, error) {
	if l == nil {
		return []byte(`[]`), nil
	}
	return json.Marshal([]string(l))
}

func (Keywords) JSONSchema(refs *api.SchemaRefs) string {
	return `{"type": "array", "items": {"type": "string"}}`
}

type keywordsResponse struct {
	L Keywords `json:"l"`
}

// Spelled codes its JSON as text and states a number.
type Spelled struct {
	V string
}

func (s Spelled) MarshalText() ([]byte, error) {
	return []byte(s.V), nil
}

func (s *Spelled) UnmarshalText(text []byte) error {
	s.V = string(text)
	return nil
}

func (Spelled) JSONSchema(refs *api.SchemaRefs) string {
	return `{"type": "integer"}`
}

type spelledRequest struct {
	requests.NoRules
	S Spelled `json:"s"`
}

type spelledResponse struct {
	S Spelled `json:"s"`
}

type codeResponse struct {
	C Code `json:"c"`
}

// TestSchemaProviderOwnCoder: a type with a JSONSchema method must decode
// its own JSON in a request and encode it in a response, whatever its kind,
// since encoding/json otherwise reads or writes it by its Go type; one it
// codes as text must state a string. A nil slice that encodes itself is
// written by its method, not as null.
func TestSchemaProviderOwnCoder(t *testing.T) {
	decoder := ` has a JSONSchema method but no JSON decoder of its own: encoding/json reads it by its Go type, which the JSONSchema method does not change; add an UnmarshalJSON that reads what the schema describes, or remove the JSONSchema method`
	encoder := ` has a JSONSchema method but no JSON encoder of its own: encoding/json writes it by its Go type, which the JSONSchema method does not change; add a MarshalJSON that writes what the schema describes, or remove the JSONSchema method`

	msg := refused(t, func(g *api.Group) { api.POST[amountRequestBody, nodeResponse](g, "", "op", stub) })
	require.Equal(t, refusedPost+`openapi: api_test.Amount`+decoder, msg)
	msg = refused(t, func(g *api.Group) { api.GET[fixture.HealthRequest, amountResponseBody](g, "", "op", stub) })
	require.Equal(t, refusedGet+`openapi: api_test.Amount`+encoder, msg)
	msg = refused(t, func(g *api.Group) { api.GET[fixture.HealthRequest, wordsResponse](g, "", "op", stub) })
	require.Equal(t, refusedGet+`openapi: api_test.Words`+encoder, msg)
	msg = refused(t, func(g *api.Group) { api.POST[starsRequest, nodeResponse](g, "", "op", stub) })
	require.Equal(t, refusedPost+`openapi: api_test.Stars`+decoder, msg)

	msg = refused(t, func(g *api.Group) { api.POST[spelledRequest, nodeResponse](g, "", "op", stub) })
	require.Equal(t, refusedPost+`openapi: api_test.Spelled codes its JSON as text with UnmarshalText, which is always a JSON string, and its JSONSchema text does not say so; give it "type": "string"`, msg)
	msg = refused(t, func(g *api.Group) { api.GET[fixture.HealthRequest, spelledResponse](g, "", "op", stub) })
	require.Equal(t, refusedGet+`openapi: api_test.Spelled codes its JSON as text with MarshalText, which is always a JSON string, and its JSONSchema text does not say so; give it "type": "string"`, msg)

	e := newEcho()
	reg := api.New(e, api.Public())
	api.GET[fixture.HealthRequest, keywordsResponse](reg.Group("/keywords"), "", "get", stub)
	api.GET[fixture.HealthRequest, codeResponse](reg.Group("/codes"), "", "get", stub)
	reg.Ready()
	fake := &fakeTB{TB: t}
	rec := apitest.New(t, reg).Do(fake, e, "keywords.get", httptest.NewRequest(http.MethodGet, "/keywords", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `{"l":[]}`, rec.Body.String())
	require.Empty(t, fake.errors)
}

// Tree asks for its own schema.
type Tree struct{}

func (Tree) MarshalJSON() ([]byte, error) {
	return []byte(`[]`), nil
}

func (Tree) JSONSchema(refs *api.SchemaRefs) string {
	return `{"type": "array", "items": ` + refs.For(reflect.TypeFor[Tree]()) + `}`
}

// Ping and Pong ask for each other's schema.
type Ping struct{}

func (Ping) MarshalJSON() ([]byte, error) {
	return []byte(`[]`), nil
}

func (Ping) JSONSchema(refs *api.SchemaRefs) string {
	return `{"type": "array", "items": ` + refs.For(reflect.TypeFor[[]Pong]()) + `}`
}

type Pong struct{}

func (Pong) MarshalJSON() ([]byte, error) {
	return []byte(`[]`), nil
}

func (Pong) JSONSchema(refs *api.SchemaRefs) string {
	return `{"type": "array", "items": ` + refs.For(reflect.TypeFor[Ping]()) + `}`
}

// Folder asks for FolderEntry, a struct that holds a Folder: the
// component's $ref ends the recursion.
type Folder struct {
	entry *FolderEntry
}

func (f Folder) MarshalJSON() ([]byte, error) {
	return json.Marshal(f.entry)
}

func (Folder) JSONSchema(refs *api.SchemaRefs) string {
	return `{"anyOf": [` + refs.For(reflect.TypeFor[FolderEntry]()) + `, {"type": "null"}]}`
}

type FolderEntry struct {
	Name string `json:"name"`
	Sub  Folder `json:"sub"`
}

type treeResponse struct {
	T Tree `json:"t"`
}

type pingResponse struct {
	P Ping `json:"p"`
}

type folderResponse struct {
	F Folder `json:"f"`
}

// TestSchemaProviderRecursion: a JSONSchema method that asks for its own
// type's schema, directly or through another JSONSchema method, fails
// registration instead of recursing; one that reaches its type again
// through a struct component ends at the component's $ref.
func TestSchemaProviderRecursion(t *testing.T) {
	msg := refused(t, func(g *api.Group) { api.GET[fixture.HealthRequest, treeResponse](g, "", "op", stub) })
	require.Equal(t, refusedGet+`openapi: api_test.Tree: its JSONSchema method asked refs.For for api_test.Tree, which refers to api_test.Tree, whose JSONSchema method is running; a JSONSchema method cannot ask for its own type's schema, directly or through another type's JSONSchema method`, msg)
	msg = refused(t, func(g *api.Group) { api.GET[fixture.HealthRequest, pingResponse](g, "", "op", stub) })
	require.Equal(t, refusedGet+`openapi: api_test.Pong: its JSONSchema method asked refs.For for api_test.Ping, which refers to api_test.Ping, whose JSONSchema method is running; a JSONSchema method cannot ask for its own type's schema, directly or through another type's JSONSchema method`, msg)

	reg := api.New(newEcho(), api.Public())
	api.GET[fixture.HealthRequest, folderResponse](reg.Group("/folders"), "", "get", stub)
	reg.Ready()
	raw, err := reg.Spec(api.V31)
	require.NoError(t, err)
	doc := decode(t, raw)
	folder := map[string]any{"$ref": "#/components/schemas/api_test.Folder"}
	require.Equal(t, map[string]any{"anyOf": []any{map[string]any{"$ref": "#/components/schemas/api_test.FolderEntry"}, nullSchema}}, dig(t, doc, "components", "schemas", "api_test.Folder"))
	require.Equal(t, folder, dig(t, doc, "components", "schemas", "api_test.folderResponse", "properties", "f"))
	require.Equal(t, folder, dig(t, doc, "components", "schemas", "api_test.FolderEntry", "properties", "sub"))
}

// Card is bound by encoding/json inside a Payment.
type Card struct {
	Number string `json:"number"`
	Label  string `json:"label" default:"main"`
	Secret int    `json:"secret" hidden:"true"`
}

// Payment reads and writes itself as a Card, and says so with refs.For.
type Payment struct {
	card Card
}

func (p Payment) MarshalJSON() ([]byte, error) {
	return json.Marshal(p.card)
}

func (p *Payment) UnmarshalJSON(raw []byte) error {
	return json.Unmarshal(raw, &p.card)
}

func (Payment) JSONSchema(refs *api.SchemaRefs) string {
	return refs.For(reflect.TypeFor[Card]())
}

// Receipt writes itself as a stampResponse, whose Stamp writes its own JSON
// and does not say what.
type Receipt struct{}

func (Receipt) MarshalJSON() ([]byte, error) {
	return []byte(`{"at":"1"}`), nil
}

func (Receipt) JSONSchema(refs *api.SchemaRefs) string {
	return refs.For(reflect.TypeFor[stampResponse]())
}

// Envelope writes itself as an unnamed struct.
type Envelope struct{}

func (Envelope) MarshalJSON() ([]byte, error) {
	return []byte(`{"n":1}`), nil
}

func (Envelope) JSONSchema(refs *api.SchemaRefs) string {
	return refs.For(reflect.TypeFor[struct {
		N int `json:"n"`
	}]())
}

type paymentRequest struct {
	requests.NoRules
	P Payment `json:"p"`
}

type receiptResponse struct {
	R Receipt `json:"r"`
}

type envelopeResponse struct {
	E Envelope `json:"e"`
}

// TestSchemaProviderAskedTypes: a type a JSONSchema method asks refs.For for
// is linted and key-checked as a field of the type that holds the provider
// is, in the same direction; an unnamed struct gets a component named after
// the provider.
func TestSchemaProviderAskedTypes(t *testing.T) {
	msg := refused(t, func(g *api.Group) { api.POST[paymentRequest, nodeResponse](g, "", "op", stub) })
	require.Equal(t, refusedPost+`openapi: api_test.Card.Label: the default tag is not supported on a request type, the registry does not apply it; set the default in the handler and describe it with doc; api_test.Card: field Secret has hidden:"true", which is not allowed on request types (encoding/json still binds it)`, msg)
	msg = refused(t, func(g *api.Group) { api.GET[fixture.HealthRequest, receiptResponse](g, "", "op", stub) })
	require.Equal(t, refusedGet+`openapi: api_test.Stamp writes its own JSON with MarshalJSON, which the schema of its fields would not describe; add a JSONSchema method (api.SchemaProvider) that returns the schema of what MarshalJSON writes`, msg)

	reg := api.New(newEcho(), api.Public())
	api.GET[fixture.HealthRequest, envelopeResponse](reg.Group("/envelopes"), "", "get", stub)
	reg.Ready()
	raw, err := reg.Spec(api.V31)
	require.NoError(t, err)
	doc := decode(t, raw)
	require.Equal(t, map[string]any{"$ref": "#/components/schemas/EnvelopeSchema"}, dig(t, doc, "components", "schemas", "api_test.Envelope"))
	require.Equal(t, "object", dig(t, doc, "components", "schemas", "EnvelopeSchema", "type"))
}

// Tone is an enum that also states its schema.
type Tone string

var _ = enums.Set([]Tone{"warm", "cold"})

func (Tone) JSONSchema(refs *api.SchemaRefs) string {
	return `{"type": "string"}`
}

type toneResponse struct {
	T Tone `json:"t"`
}

// TestSchemaProviderEnum: an enums.Set type with a JSONSchema method fails
// registration, as it would state its schema twice.
func TestSchemaProviderEnum(t *testing.T) {
	msg := refused(t, func(g *api.Group) { api.GET[fixture.HealthRequest, toneResponse](g, "", "op", stub) })
	require.Equal(t, refusedGet+`openapi: api_test.Tone is an enum (enums.Set) and has a JSONSchema method, which would state its schema twice; drop one of them`, msg)
}

// TestSchemaProviderFailedRegistration: a registration that fails leaves no
// component behind: the same route fails again for the same reason instead
// of publishing the placeholder of the first try, and a later route
// publishes nothing of it.
func TestSchemaProviderFailedRegistration(t *testing.T) {
	reg := api.New(newEcho(), api.Public())
	const want = `api: GET /pings (pings.get): openapi: api_test.Pong: its JSONSchema method asked refs.For for api_test.Ping, which refers to api_test.Ping, whose JSONSchema method is running; a JSONSchema method cannot ask for its own type's schema, directly or through another type's JSONSchema method`
	for range 2 {
		msg := panicMessage(t, func() {
			api.GET[fixture.HealthRequest, pingResponse](reg.Group("/pings"), "", "get", stub)
		})
		require.Equal(t, want, msg)
	}
	api.GET[fixture.HealthRequest, moneyResponse](reg.Group("/prices"), "", "get", stub)
	reg.Ready()
	raw, err := reg.Spec(api.V31)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "Ping")
	require.NotContains(t, string(raw), "Pong")
	require.NotContains(t, string(reg.StrictSpec()), "Ping")
}

// kept is the SchemaRefs a Keeper's JSONSchema method was given.
var kept *api.SchemaRefs

// Keeper keeps the SchemaRefs it is given.
type Keeper struct{}

func (Keeper) MarshalJSON() ([]byte, error) {
	return []byte(`"x"`), nil
}

func (Keeper) JSONSchema(refs *api.SchemaRefs) string {
	kept = refs
	return `{"type": "string"}`
}

// NilAsker asks refs.For for a nil type.
type NilAsker struct{}

func (NilAsker) MarshalJSON() ([]byte, error) {
	return []byte(`"x"`), nil
}

func (NilAsker) JSONSchema(refs *api.SchemaRefs) string {
	return refs.For(nil)
}

// Boom panics in its JSONSchema method.
type Boom struct{}

func (Boom) MarshalJSON() ([]byte, error) {
	return []byte(`"x"`), nil
}

func (Boom) JSONSchema(refs *api.SchemaRefs) string {
	panic("boom")
}

// BoomAsker asks refs.For for Boom.
type BoomAsker struct{}

func (BoomAsker) MarshalJSON() ([]byte, error) {
	return []byte(`"x"`), nil
}

func (BoomAsker) JSONSchema(refs *api.SchemaRefs) string {
	return refs.For(reflect.TypeFor[Boom]())
}

type keeperResponse struct {
	K Keeper `json:"k"`
}

type nilAskerResponse struct {
	N NilAsker `json:"n"`
}

type boomResponse struct {
	B Boom `json:"b"`
}

type boomAskerResponse struct {
	B BoomAsker `json:"b"`
}

// TestSchemaRefs: a nil *SchemaRefs, and one kept past its call, return {};
// refs.For with a nil type fails registration naming the type; a panic in a
// JSONSchema method, also one reached through refs.For, is not recovered,
// and leaves nothing behind.
func TestSchemaRefs(t *testing.T) {
	var refs *api.SchemaRefs
	require.Equal(t, "{}", refs.For(reflect.TypeFor[Money]()))
	require.Equal(t, `{"anyOf": [{}, {"type": "integer", "minimum": 0, "maximum": 100}]}`, Discount{}.JSONSchema(nil))

	reg := api.New(newEcho(), api.Public())
	api.GET[fixture.HealthRequest, keeperResponse](reg.Group("/keepers"), "", "get", stub)
	require.NotNil(t, kept)
	require.Equal(t, "{}", kept.For(reflect.TypeFor[Money]()))

	msg := refused(t, func(g *api.Group) { api.GET[fixture.HealthRequest, nilAskerResponse](g, "", "op", stub) })
	require.Equal(t, refusedGet+`openapi: api_test.NilAsker: its JSONSchema method called refs.For with a nil type`, msg)

	reg = api.New(newEcho(), api.Public())
	for range 2 {
		require.PanicsWithValue(t, "boom", func() {
			api.GET[fixture.HealthRequest, boomResponse](reg.Group("/booms"), "", "get", stub)
		})
		require.PanicsWithValue(t, "boom", func() {
			api.GET[fixture.HealthRequest, boomAskerResponse](reg.Group("/askers"), "", "get", stub)
		})
	}
}

// Shade is "a", "b" or null, and says so with a type list and null in the
// enum, the form OpenAPI 3.0 carries as nullable: true.
type Shade struct {
	raw json.RawMessage
}

func (s Shade) MarshalJSON() ([]byte, error) {
	if s.raw == nil {
		return []byte(`null`), nil
	}
	return s.raw, nil
}

func (s *Shade) UnmarshalJSON(raw []byte) error {
	s.raw = append(json.RawMessage(nil), raw...)
	return nil
}

func (Shade) JSONSchema(refs *api.SchemaRefs) string {
	return `{"type": ["string", "null"], "enum": ["a", "b", null]}`
}

type shadeRequest struct {
	requests.NoRules
	S Shade `json:"s" required:"true"`
}

type shadeResponse struct {
	S Shade `json:"s"`
}

// TestSchemaProviderNullEnum: an enum that lets null through, written as a
// type list with null in the enum, is published so that the 3.1 document,
// the 3.0 document and the validator agree: null and a listed value pass,
// another value does not.
func TestSchemaProviderNullEnum(t *testing.T) {
	e := newEcho()
	reg := api.New(e, api.Public())
	api.POST(reg.Group("/shades"), "", "create", func(ctx context.Context, req *shadeRequest) (shadeResponse, error) {
		return shadeResponse{S: req.S}, nil
	})
	reg.Ready()

	raw, err := reg.Spec(api.V31)
	require.NoError(t, err)
	require.Equal(t, map[string]any{"type": []any{"string", "null"}, "enum": []any{"a", "b", nil}}, dig(t, decode(t, raw), "components", "schemas", "api_test.Shade"))
	raw, err = reg.Spec(api.V30)
	require.NoError(t, err)
	require.Equal(t, map[string]any{"type": "string", "nullable": true, "enum": []any{"a", "b", nil}}, dig(t, decode(t, raw), "components", "schemas", "api_test.Shade"))

	contract := apitest.New(t, reg)
	for _, body := range []string{`{"s":null}`, `{"s":"a"}`} {
		fake := &fakeTB{TB: t}
		req := httptest.NewRequest(http.MethodPost, "/shades", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := contract.Do(fake, e, "shades.create", req)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.JSONEq(t, body, rec.Body.String())
		require.Empty(t, fake.errors)
	}
	s := &server{e: e, reg: reg}
	rec := s.do(http.MethodPost, "/shades", `{"s":"c"}`)
	requireError(t, rec, http.StatusBadRequest, "s")
}

// Kind is an object whose example and x- value hold keys the OpenAPI 3.0
// document leaves as they are.
type Kind struct{}

func (Kind) MarshalJSON() ([]byte, error) {
	return []byte(`{"type":"card"}`), nil
}

const kindText = `{"type": "object", "example": {"type": "card"}, "x-meta": {"exclusiveMinimum": true, "anyOf": [{"type": "string"}]}}`

func (Kind) JSONSchema(refs *api.SchemaRefs) string {
	return kindText
}

type kindResponse struct {
	K Kind `json:"k"`
}

// TestSchemaProviderValueKeys: a value whose keys the OpenAPI 3.0 document
// does not rewrite, a type that is not a list among them, is published as
// written in both documents.
func TestSchemaProviderValueKeys(t *testing.T) {
	reg := api.New(newEcho(), api.Public())
	api.GET[fixture.HealthRequest, kindResponse](reg.Group("/kinds"), "", "get", stub)
	reg.Ready()

	var want map[string]any
	err := json.Unmarshal([]byte(kindText), &want)
	require.NoError(t, err)
	raw, err := reg.Spec(api.V31)
	require.NoError(t, err)
	require.Equal(t, want, dig(t, decode(t, raw), "components", "schemas", "api_test.Kind"))
	raw, err = reg.Spec(api.V30)
	require.NoError(t, err)
	require.Equal(t, want, dig(t, decode(t, raw), "components", "schemas", "api_test.Kind"))
}

// Holder writes itself as an object of a *Money, a types.Optional of an
// enum and a pointer to an enum, and says so with refs.For.
type Holder struct{}

func (Holder) MarshalJSON() ([]byte, error) {
	return []byte(`{}`), nil
}

func (Holder) JSONSchema(refs *api.SchemaRefs) string {
	return `{"type": "object", "properties": {"m": ` + refs.For(reflect.TypeFor[*Money]()) +
		`, "o": ` + refs.For(reflect.TypeFor[types.Optional[fixture.RunStatus]]()) +
		`, "s": ` + refs.For(reflect.TypeFor[*fixture.RunStatus]()) + `}}`
}

type holderResponse struct {
	H Holder `json:"h"`
}

// TestSchemaRefsNull: refs.For gives a pointer to a type with a JSONSchema
// method, a types.Optional and a pointer to an enum as anyOf [ref, null].
func TestSchemaRefsNull(t *testing.T) {
	reg := api.New(newEcho(), api.Public())
	api.GET[fixture.HealthRequest, holderResponse](reg.Group("/holders"), "", "get", stub)
	reg.Ready()

	raw, err := reg.Spec(api.V31)
	require.NoError(t, err)
	props := dig(t, decode(t, raw), "components", "schemas", "api_test.Holder", "properties")
	status := map[string]any{"anyOf": []any{map[string]any{"$ref": "#/components/schemas/fixture.RunStatus"}, nullSchema}}
	require.Equal(t, map[string]any{"anyOf": []any{moneyRef, nullSchema}}, dig(t, props, "m"))
	require.Equal(t, status, dig(t, props, "o"))
	require.Equal(t, status, dig(t, props, "s"))
}

// Label and Labels write themselves and state their schema, and have no
// decoder of their own.
type Label string

func (l Label) MarshalJSON() ([]byte, error) {
	return json.Marshal(string(l))
}

func (Label) JSONSchema(refs *api.SchemaRefs) string {
	return `{"type": "string"}`
}

type Labels []string

func (l Labels) MarshalJSON() ([]byte, error) {
	return json.Marshal([]string(l))
}

func (Labels) JSONSchema(refs *api.SchemaRefs) string {
	return `{"type": "array", "items": {"type": "string"}}`
}

type labelFormatResponse struct {
	L Label `json:"l" format:"email"`
}

type labelsFormatResponse struct {
	L Labels `json:"l" format:"email"`
}

type tagsItemsRequest struct {
	requests.NoRules
	Tags Tags `json:"tags" minItems:"1"`
}

// NullableCode codes its JSON as text and states a string or null.
type NullableCode struct {
	V string
}

func (c NullableCode) MarshalText() ([]byte, error) {
	return []byte(c.V), nil
}

func (NullableCode) JSONSchema(refs *api.SchemaRefs) string {
	return `{"type": ["string", "null"]}`
}

type nullableCodeResponse struct {
	C NullableCode `json:"c"`
}

// TestSchemaProviderTags: a format tag on a type with a JSONSchema method is
// refused in a response too, also on one with no decoder of its own, and an
// array tag on one is refused in a request; one that codes its JSON as text
// must not state null.
func TestSchemaProviderTags(t *testing.T) {
	msg := refused(t, func(g *api.Group) { api.GET[fixture.HealthRequest, labelFormatResponse](g, "", "op", stub) })
	require.Equal(t, refusedGet+`openapi: api_test.labelFormatResponse.L: the format tag is not supported on api_test.Label, whose format is set by the type; the schema and the decoder would disagree on the value's text`, msg)
	msg = refused(t, func(g *api.Group) { api.GET[fixture.HealthRequest, labelsFormatResponse](g, "", "op", stub) })
	require.Equal(t, refusedGet+`openapi: api_test.labelsFormatResponse.L: the format tag is not supported on api_test.Labels, whose format is set by the type; the schema and the decoder would disagree on the value's text`, msg)
	msg = refused(t, func(g *api.Group) { api.POST[tagsItemsRequest, nodeResponse](g, "", "op", stub) })
	require.Equal(t, refusedPost+`openapi: api_test.tagsItemsRequest.Tags: the minItems tag is not supported on api_test.Tags in a request type: api_test.Tags states its schema with its JSONSchema method, and the tag would be written beside that schema whatever its type, where nothing may check it; state the rule in the schema JSONSchema returns`, msg)
	msg = refused(t, func(g *api.Group) { api.GET[fixture.HealthRequest, nullableCodeResponse](g, "", "op", stub) })
	require.Equal(t, refusedGet+`openapi: api_test.NullableCode codes its JSON as text with MarshalText, which is always a JSON string, and its JSONSchema text does not say so; give it "type": "string"`, msg)
}

// Asker codes its own JSON and states the schema of T with refs.For.
type Asker[T any] struct{}

func (Asker[T]) MarshalJSON() ([]byte, error) {
	return []byte(`null`), nil
}

func (*Asker[T]) UnmarshalJSON([]byte) error {
	return nil
}

func (Asker[T]) JSONSchema(refs *api.SchemaRefs) string {
	return refs.For(reflect.TypeFor[T]())
}

type askerResponse[T any] struct {
	A Asker[T] `json:"a"`
}

type askerRequest[T any] struct {
	requests.NoRules
	A Asker[T] `json:"a"`
}

// Tally writes itself through a pointer receiver.
type Tally int

func (t *Tally) MarshalJSON() ([]byte, error) {
	return json.Marshal(int(*t))
}

// queryHolder holds a query tag outside a request type.
type queryHolder struct {
	Q string `json:"q" query:"q"`
}

// TestSchemaProviderAskedLint: a type refs.For is asked for is held to the
// checks of a field's type that the walk into it does not make: an
// interface type with methods, a map keyed by an enum and a parameter tag in a request, and a
// pointer-receiver marshaler held by value in a map of a response.
func TestSchemaProviderAskedLint(t *testing.T) {
	name := reflect.TypeFor[Asker[fmt.Stringer]]().String()
	msg := refused(t, func(g *api.Group) { api.POST[askerRequest[fmt.Stringer], nodeResponse](g, "", "op", stub) })
	require.Equal(t, refusedPost+`openapi: `+name+`: refs.For(fmt.Stringer): the interface type fmt.Stringer is not supported in a request: encoding/json cannot decode a value into it; use a concrete type`, msg)

	name = reflect.TypeFor[Asker[map[fixture.RunStatus]string]]().String()
	msg = refused(t, func(g *api.Group) {
		api.POST[askerRequest[map[fixture.RunStatus]string], nodeResponse](g, "", "op", stub)
	})
	require.Equal(t, refusedPost+`openapi: `+name+`: refs.For(map[fixture.RunStatus]string): map keys are not validated, so a key of the enum type fixture.RunStatus could hold any string; use a string key and check it in Validate()`, msg)

	msg = refused(t, func(g *api.Group) { api.POST[askerRequest[queryHolder], nodeResponse](g, "", "op", stub) })
	require.Equal(t, refusedPost+`openapi: api_test.queryHolder.Q: the query tag is not supported on a nested struct; parameters and files are read only from the request type and the structs it embeds without a JSON name`, msg)

	msg = refused(t, func(g *api.Group) { api.GET[fixture.HealthRequest, askerResponse[map[string]Tally]](g, "", "op", stub) })
	require.Equal(t, refusedGet+`openapi: api_test.Tally is held by value in a map of a response type: encoding/json cannot call its marshaler there, which has a pointer receiver, and would send its fields instead; hold *api_test.Tally`, msg)
}
