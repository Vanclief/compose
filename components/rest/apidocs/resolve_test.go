package apidocs

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// refWalker visits every schema, parameter, request body, response, header
// and example of a document, inline or under components, and records each
// $ref that does not point at an existing component of the right kind.
type refWalker struct {
	doc     *Document
	seen    map[*Schema]bool
	checked int
	missing []string
}

// unresolvedRefs returns the refs of doc that do not resolve.
func unresolvedRefs(doc *Document) []string {
	w := &refWalker{doc: doc, seen: map[*Schema]bool{}}
	w.walk()
	return w.missing
}

func (w *refWalker) check(where, ref, prefix string, exists func(string) bool) bool {
	if ref == "" {
		return false
	}
	w.checked++
	name := RefName(ref, prefix)
	if name == "" || !exists(name) {
		w.missing = append(w.missing, where+": "+ref)
	}
	return true
}

// has reports whether a component exists and is not null.
func has[T comparable](o Ordered[T]) func(string) bool {
	return func(name string) bool {
		var zero T
		v, ok := o.Get(name)
		return ok && v != zero
	}
}

func (w *refWalker) walk() {
	c := w.doc.Components
	for _, key := range c.Schemas.Keys {
		w.schema("components.schemas."+key, c.Schemas.Map[key])
	}
	for _, key := range c.Parameters.Keys {
		w.parameter("components.parameters."+key, c.Parameters.Map[key], refParameters, c.Parameters)
	}
	for _, key := range c.Headers.Keys {
		w.parameter("components.headers."+key, c.Headers.Map[key], refHeaders, c.Headers)
	}
	for _, key := range c.RequestBodies.Keys {
		w.requestBody("components.requestBodies."+key, c.RequestBodies.Map[key])
	}
	for _, key := range c.Responses.Keys {
		w.response("components.responses."+key, c.Responses.Map[key])
	}
	w.examples("components.examples", c.Examples)

	for _, op := range w.doc.Operations() {
		where := op.Method + " " + op.Path
		for _, p := range op.Item.Parameters {
			w.parameter(where+" path parameter", p, refParameters, c.Parameters)
		}
		for _, p := range op.Operation.Parameters {
			w.parameter(where+" parameter", p, refParameters, c.Parameters)
		}
		w.requestBody(where+" requestBody", op.Operation.RequestBody)
		for _, status := range op.Operation.Responses.Keys {
			w.response(where+" "+status, op.Operation.Responses.Map[status])
		}
	}
}

func (w *refWalker) schema(where string, s *Schema) {
	if s == nil || w.seen[s] {
		return
	}
	w.seen[s] = true

	if w.check(where, s.Ref, refSchemas, has(w.doc.Components.Schemas)) {
		return
	}
	if s.Discriminator != nil {
		for value, ref := range s.Discriminator.Mapping {
			w.check(where+" discriminator "+value, ref, refSchemas, has(w.doc.Components.Schemas))
		}
	}
	for _, key := range s.Properties.Keys {
		w.schema(where+"."+key, s.Properties.Map[key])
	}
	w.schema(where+"[]", s.Items)
	w.schema(where+" not", s.Not)
	w.schema(where+" additionalProperties", s.AdditionalSchema())
	for _, sub := range s.AllOf {
		w.schema(where+" allOf", sub)
	}
	for _, sub := range s.OneOf {
		w.schema(where+" oneOf", sub)
	}
	for _, sub := range s.AnyOf {
		w.schema(where+" anyOf", sub)
	}
}

func (w *refWalker) parameter(where string, p *Parameter, prefix string, components Ordered[*Parameter]) {
	if p == nil {
		return
	}
	if w.check(where, p.Ref, prefix, has(components)) {
		return
	}
	w.schema(where, p.Schema)
	w.content(where, p.Content)
	w.examples(where, p.Examples)
}

func (w *refWalker) requestBody(where string, b *RequestBody) {
	if b == nil {
		return
	}
	if w.check(where, b.Ref, refRequestBodies, has(w.doc.Components.RequestBodies)) {
		return
	}
	w.content(where, b.Content)
}

func (w *refWalker) response(where string, r *Response) {
	if r == nil {
		return
	}
	if w.check(where, r.Ref, refResponses, has(w.doc.Components.Responses)) {
		return
	}
	for _, key := range r.Headers.Keys {
		w.parameter(where+" header "+key, r.Headers.Map[key], refHeaders, w.doc.Components.Headers)
	}
	w.content(where, r.Content)
}

func (w *refWalker) content(where string, content Ordered[*MediaType]) {
	for _, key := range content.Keys {
		mt := content.Map[key]
		if mt == nil {
			continue
		}
		w.schema(where+" "+key, mt.Schema)
		w.examples(where+" "+key, mt.Examples)
	}
}

func (w *refWalker) examples(where string, examples Ordered[*Example]) {
	for _, key := range examples.Keys {
		e := examples.Map[key]
		if e != nil {
			w.check(where+" example "+key, e.Ref, refExamples, has(w.doc.Components.Examples))
		}
	}
}

func TestEmmaRefsAllResolve(t *testing.T) {
	// The emma-backend generator references four crm leads schemas it never
	// emits. Pin them so any other unresolved ref fails the test.
	const leads = "#/components/schemas/application_modules_crm_leads_"
	known := []string{
		"POST /crm/leads/demo-request requestBody application/json: " + leads + "DemoRequestRequest",
		"POST /crm/leads/demo-request 200 application/json: " + leads + "DemoRequestResponse",
		"POST /crm/leads/touch requestBody application/json: " + leads + "TouchRequest",
		"POST /crm/leads/touch 200 application/json: " + leads + "TouchResponse",
	}

	for _, name := range []string{"emma.yaml", "emma.json"} {
		t.Run(name, func(t *testing.T) {
			doc := loadEmma(t, name)
			w := &refWalker{doc: doc, seen: map[*Schema]bool{}}
			w.walk()

			require.ElementsMatch(t, known, w.missing)
			require.Greater(t, w.checked, 1500)
		})
	}
}

func TestEmmaQuirks(t *testing.T) {
	doc := loadEmma(t, "emma.yaml")

	// Shared base models come in through allOf of a $ref plus an inline object.
	access := doc.Flatten(doc.Components.Schemas.Map["application_models_accesscontrol_AccessPoint"])
	require.Empty(t, access.AllOf)
	require.Contains(t, access.Properties.Keys, "complex_id")
	require.Equal(t, "object", access.Type.Primary())

	// Request shapes are oneOf variants that refine the parent properties.
	create := doc.Components.Schemas.Map["application_modules_tickets_categories_CreateRequest"]
	require.Len(t, create.OneOf, 2)
	require.Equal(t, []string{"color", "name", "organization_id", "type"}, create.Properties.Keys)
	require.NotNil(t, create.OneOf[1].Not)
	require.Equal(t, []string{"organization_id"}, create.OneOf[1].Not.Required)

	// Mutually exclusive fields are spelled as anyOf of required lists.
	assign := doc.Components.Schemas.Map["application_modules_property_complexes_staff_AssignStaffRoleRequest"]
	require.NotNil(t, assign.OneOf[0].AdditionalProperties)
	require.False(t, assign.OneOf[0].AdditionalProperties.Allowed)
	require.Len(t, assign.OneOf[0].AnyOf, 2)
	require.Equal(t, []string{"email"}, assign.OneOf[0].AnyOf[0].Required)
	require.Empty(t, assign.OneOf[0].AnyOf[0].Type)
}

func TestRefName(t *testing.T) {
	require.Equal(t, "Pet", RefName("#/components/schemas/Pet", refSchemas))
	require.Equal(t, "a/b~c", RefName("#/components/schemas/a~1b~0c", refSchemas))
	require.Equal(t, "Foo Bar", RefName("#/components/schemas/Foo%20Bar", refSchemas))
	require.Equal(t, "100%", RefName("#/components/schemas/100%", refSchemas))
	require.Equal(t, "", RefName("#/components/responses/Pet", refSchemas))
	require.Equal(t, "", RefName("other.yaml#/components/schemas/Pet", refSchemas))
	require.Equal(t, "", RefName("", refSchemas))
}

func parseYAML(t *testing.T, input string) *Document {
	t.Helper()
	doc, err := Parse([]byte(input))
	require.NoError(t, err)
	return doc
}

const resolveFixture = `
openapi: 3.1.0
info: {title: t, version: "1"}
components:
  schemas:
    A: {$ref: '#/components/schemas/B'}
    B: {$ref: '#/components/schemas/C'}
    C: {type: object, title: C}
    Dangling: {$ref: '#/components/schemas/Missing'}
    Loop1: {$ref: '#/components/schemas/Loop2'}
    Loop2: {$ref: '#/components/schemas/Loop1'}
  parameters:
    Limit: {name: limit, in: query, schema: {type: integer}}
    Alias: {$ref: '#/components/parameters/Limit'}
  headers:
    Rate: {description: rate, schema: {type: integer}}
    RateAlias: {$ref: '#/components/headers/Rate'}
  requestBodies:
    Body: {description: body, content: {application/json: {schema: {type: object}}}}
    BodyAlias: {$ref: '#/components/requestBodies/Body'}
  responses:
    Ok: {description: ok}
    OkAlias: {$ref: '#/components/responses/Ok'}
  examples:
    One: {summary: one, value: 1}
    OneAlias: {$ref: '#/components/examples/One'}
`

func TestSchemaFollowsRefChains(t *testing.T) {
	doc := parseYAML(t, resolveFixture)

	s, name := doc.Schema(&Schema{Ref: "#/components/schemas/A"})
	require.Equal(t, "C", name)
	require.Equal(t, "C", s.Title)
	require.Equal(t, doc.Components.Schemas.Map["C"], s)

	inline := &Schema{Title: "inline"}
	s, name = doc.Schema(inline)
	require.Same(t, inline, s)
	require.Equal(t, "", name)

	s, name = doc.Schema(nil)
	require.Nil(t, s)
	require.Equal(t, "", name)
}

func TestSchemaUnresolvableRef(t *testing.T) {
	doc := parseYAML(t, resolveFixture)

	missing := &Schema{Ref: "#/components/schemas/Missing"}
	s, name := doc.Schema(missing)
	require.Same(t, missing, s)
	require.Equal(t, "Missing", name)

	// A chain ending in a missing component stops at the last schema found.
	s, name = doc.Schema(&Schema{Ref: "#/components/schemas/Dangling"})
	require.Same(t, doc.Components.Schemas.Map["Dangling"], s)
	require.Equal(t, "Missing", name)

	remote := &Schema{Ref: "common.yaml#/Pet"}
	s, name = doc.Schema(remote)
	require.Same(t, remote, s)
	require.Equal(t, "common.yaml#/Pet", name)

	// Cyclic refs terminate.
	s, _ = doc.Schema(&Schema{Ref: "#/components/schemas/Loop1"})
	require.NotNil(t, s)
	require.NotEmpty(t, s.Ref)
}

func TestResolveComponentRefs(t *testing.T) {
	doc := parseYAML(t, resolveFixture)

	p := doc.Parameter(&Parameter{Ref: "#/components/parameters/Alias"})
	require.Equal(t, "limit", p.Name)
	missingParam := &Parameter{Ref: "#/components/parameters/Nope"}
	require.Same(t, missingParam, doc.Parameter(missingParam))
	wrongKind := &Parameter{Ref: "#/components/schemas/C"}
	require.Same(t, wrongKind, doc.Parameter(wrongKind))
	require.Nil(t, doc.Parameter(nil))

	h := doc.Header(&Header{Ref: "#/components/headers/RateAlias"})
	require.Equal(t, "rate", h.Description)
	missingHeader := &Header{Ref: "#/components/headers/Nope"}
	require.Same(t, missingHeader, doc.Header(missingHeader))

	b := doc.RequestBody(&RequestBody{Ref: "#/components/requestBodies/BodyAlias"})
	require.Equal(t, "body", b.Description)
	missingBody := &RequestBody{Ref: "#/components/requestBodies/Nope"}
	require.Same(t, missingBody, doc.RequestBody(missingBody))

	r := doc.Response(&Response{Ref: "#/components/responses/OkAlias"})
	require.Equal(t, "ok", r.Description)
	missingResponse := &Response{Ref: "#/components/responses/Nope"}
	require.Same(t, missingResponse, doc.Response(missingResponse))

	e := doc.Example(&Example{Ref: "#/components/examples/OneAlias"})
	require.Equal(t, "one", e.Summary)
	require.EqualValues(t, 1, e.Value)
	missingExample := &Example{Ref: "#/components/examples/Nope"}
	require.Same(t, missingExample, doc.Example(missingExample))

	inline := &Response{Description: "inline"}
	require.Same(t, inline, doc.Response(inline))
}

const flattenFixture = `
openapi: 3.1.0
info: {title: t, version: "1"}
components:
  schemas:
    Base:
      type: object
      description: base description
      format: base-format
      required: [id]
      properties:
        id: {type: integer}
        shared: {type: string, description: from base}
    Named:
      allOf:
        - $ref: '#/components/schemas/Base'
        - type: object
          title: member title
          required: [name, id]
          properties:
            name: {type: string}
            shared: {type: integer, description: from member}
    Child:
      description: child description
      required: [extra]
      properties:
        extra: {type: boolean}
      allOf:
        - $ref: '#/components/schemas/Named'
        - properties:
            last: {type: string}
          minProperties: 1
    CycleA:
      properties:
        a: {type: string}
      allOf:
        - $ref: '#/components/schemas/CycleB'
    CycleB:
      properties:
        b: {type: string}
      allOf:
        - $ref: '#/components/schemas/CycleA'
    Plain:
      type: string
`

func TestFlattenMergesAllOf(t *testing.T) {
	doc := parseYAML(t, flattenFixture)

	named := doc.Flatten(&Schema{Ref: "#/components/schemas/Named"})
	require.Empty(t, named.AllOf)
	require.Empty(t, named.Ref)
	require.Equal(t, []string{"id", "shared", "name"}, named.Properties.Keys)
	// The later member replaces the earlier definition of shared.
	require.Equal(t, "from member", named.Properties.Map["shared"].Description)
	require.Equal(t, []string{"id", "name"}, named.Required)
	// Scalars: first non-empty walking parent then members in order.
	require.Equal(t, TypeList{"object"}, named.Type)
	require.Equal(t, "base description", named.Description)
	require.Equal(t, "base-format", named.Format)
	require.Equal(t, "member title", named.Title)
}

func TestFlattenParentFirstAndNested(t *testing.T) {
	doc := parseYAML(t, flattenFixture)

	child := doc.Flatten(doc.Components.Schemas.Map["Child"])
	require.Empty(t, child.AllOf)
	// Parent properties first, then the nested allOf of Named, then the inline member.
	require.Equal(t, []string{"extra", "id", "shared", "name", "last"}, child.Properties.Keys)
	require.Equal(t, []string{"extra", "id", "name"}, child.Required)
	require.Equal(t, "child description", child.Description)
	require.Equal(t, "member title", child.Title)
	require.Equal(t, TypeList{"object"}, child.Type)
	require.NotNil(t, child.MinProperties)
	require.Equal(t, 1, *child.MinProperties)
}

func TestFlattenWithoutAllOfReturnsResolved(t *testing.T) {
	doc := parseYAML(t, flattenFixture)

	plain := doc.Flatten(&Schema{Ref: "#/components/schemas/Plain"})
	require.Same(t, doc.Components.Schemas.Map["Plain"], plain)
	require.Nil(t, doc.Flatten(nil))

	missing := &Schema{Ref: "#/components/schemas/Missing"}
	require.Same(t, missing, doc.Flatten(missing))
}

func TestFlattenCyclicAllOfTerminates(t *testing.T) {
	doc := parseYAML(t, flattenFixture)

	a := doc.Flatten(doc.Components.Schemas.Map["CycleA"])
	require.Equal(t, []string{"a", "b"}, a.Properties.Keys)
	require.Empty(t, a.AllOf)
}

func TestFlattenNeverMutatesTheDocument(t *testing.T) {
	doc := parseYAML(t, flattenFixture)

	before, err := json.Marshal(doc.Components.Schemas)
	require.NoError(t, err)

	for _, key := range doc.Components.Schemas.Keys {
		out := doc.Flatten(doc.Components.Schemas.Map[key])
		require.NotNil(t, out)
	}
	child := doc.Flatten(doc.Components.Schemas.Map["Child"])
	child.Properties.Set("added", &Schema{})
	child.Required = append(child.Required, "added")

	after, err := json.Marshal(doc.Components.Schemas)
	require.NoError(t, err)
	require.JSONEq(t, string(before), string(after))
	require.Len(t, doc.Components.Schemas.Map["Child"].AllOf, 2)
	require.Equal(t, []string{"extra"}, doc.Components.Schemas.Map["Child"].Properties.Keys)
}

func TestFlattenEmmaNeverMutates(t *testing.T) {
	doc := loadEmma(t, "emma.yaml")

	before, err := json.Marshal(doc.Components.Schemas)
	require.NoError(t, err)
	for _, key := range doc.Components.Schemas.Keys {
		doc.Flatten(doc.Components.Schemas.Map[key])
	}
	after, err := json.Marshal(doc.Components.Schemas)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after))
}

const operationsFixture = `
openapi: 3.0.3
info: {title: t, version: "1"}
paths:
  /zeta/{id}:
    parameters:
      - {name: id, in: path, required: true, description: path level}
      - {name: X-Trace, in: header}
      - {name: id, in: query, description: same name other location}
      - $ref: '#/components/parameters/Limit'
    delete: {operationId: zetaDelete, responses: {"204": {description: gone}}}
    get:
      operationId: zetaGet
      parameters:
        - {name: session, in: cookie}
        - {name: id, in: path, required: true, description: operation level}
        - {name: sort, in: query}
        - $ref: '#/components/parameters/Missing'
      responses: {"200": {description: ok}}
    post: {operationId: zetaPost, responses: {"200": {description: ok}}}
  /alpha:
    patch: {operationId: alphaPatch, responses: {"200": {description: ok}}}
    trace: {operationId: alphaTrace, responses: {"200": {description: ok}}}
    head: {operationId: alphaHead, responses: {"200": {description: ok}}}
components:
  parameters:
    Limit: {name: limit, in: query}
`

func TestOperationsOrder(t *testing.T) {
	doc := parseYAML(t, operationsFixture)

	var got []string
	for _, op := range doc.Operations() {
		got = append(got, op.Method+" "+op.Path+" "+op.Operation.OperationID)
		require.Same(t, doc.Paths.Map[op.Path], op.Item)
	}
	require.Equal(t, []string{
		"GET /zeta/{id} zetaGet",
		"POST /zeta/{id} zetaPost",
		"DELETE /zeta/{id} zetaDelete",
		"PATCH /alpha alphaPatch",
		"HEAD /alpha alphaHead",
		"TRACE /alpha alphaTrace",
	}, got)
}

func TestParametersMergeAndSort(t *testing.T) {
	doc := parseYAML(t, operationsFixture)
	item := doc.Paths.Map["/zeta/{id}"]

	describe := func(params []*Parameter) []string {
		var out []string
		for _, p := range params {
			if p.Ref != "" {
				out = append(out, "unresolved "+p.Ref)
				continue
			}
			out = append(out, p.In+" "+p.Name+" "+p.Description)
		}
		return out
	}

	require.Equal(t, []string{
		"path id operation level",
		"query id same name other location",
		"query limit ",
		"query sort ",
		"header X-Trace ",
		"cookie session ",
		"unresolved #/components/parameters/Missing",
	}, describe(doc.Parameters(item, item.Get)))

	require.Equal(t, []string{
		"path id path level",
		"query id same name other location",
		"query limit ",
		"header X-Trace ",
	}, describe(doc.Parameters(item, item.Post)))

	require.Equal(t, []string{
		"query sort ",
		"cookie session ",
		"unresolved #/components/parameters/Missing",
	}, describe(doc.Parameters(nil, &Operation{Parameters: []*Parameter{
		{Name: "session", In: "cookie"},
		{Name: "sort", In: "query"},
		nil,
		{Ref: "#/components/parameters/Missing"},
	}})))

	require.Empty(t, doc.Parameters(nil, nil))

	// The document itself is untouched.
	require.Len(t, item.Parameters, 4)
	require.Equal(t, "path level", item.Parameters[0].Description)
	require.Len(t, item.Get.Parameters, 4)
}

func TestOperationSecurity(t *testing.T) {
	doc := parseYAML(t, `
openapi: 3.0.3
info: {title: t, version: "1"}
security: [{bearer: []}]
paths:
  /a:
    get: {responses: {"200": {description: ok}}}
    post: {security: [], responses: {"200": {description: ok}}}
    put: {security: [{apiKey: [read]}], responses: {"200": {description: ok}}}
`)
	item := doc.Paths.Map["/a"]
	require.Equal(t, []map[string][]string{{"bearer": {}}}, doc.OperationSecurity(item.Get))
	require.Empty(t, doc.OperationSecurity(item.Post))
	require.NotNil(t, doc.OperationSecurity(item.Post))
	require.Equal(t, []map[string][]string{{"apiKey": {"read"}}}, doc.OperationSecurity(item.Put))
}

// polymorphicSpec is the springdoc and NSwag shape: the base lists its
// subtypes in oneOf and each subtype extends the base through allOf.
const polymorphicSpec = `
openapi: 3.0.3
info: {title: t, version: "1"}
paths: {}
components:
  schemas:
    Pet:
      type: object
      required: [petType]
      properties:
        petType: {type: string}
        name: {type: string}
      discriminator:
        propertyName: petType
        mapping:
          cat: '#/components/schemas/Cat'
          dog: '#/components/schemas/Dog'
      oneOf:
        - $ref: '#/components/schemas/Cat'
        - $ref: '#/components/schemas/Dog'
    Cat:
      allOf:
        - $ref: '#/components/schemas/Pet'
        - type: object
          properties:
            lives: {type: integer}
    Dog:
      allOf:
        - $ref: '#/components/schemas/Pet'
        - type: object
          properties:
            barks: {type: boolean}
    Tabby:
      allOf:
        - $ref: '#/components/schemas/Cat'
        - type: object
          properties:
            stripes: {type: integer}
    Wrapper:
      allOf:
        - $ref: '#/components/schemas/Pet'
      description: unrelated to the oneOf members
`

func TestFlattenAllOfPolymorphism(t *testing.T) {
	doc := parseYAML(t, polymorphicSpec)

	cat := doc.Flatten(schemaRef("Cat"))
	require.Empty(t, cat.OneOf)
	require.Nil(t, cat.Discriminator)
	require.Equal(t, []string{"petType", "name", "lives"}, cat.Properties.Keys)

	// The back-reference is found anywhere in the allOf chain.
	tabby := doc.Flatten(schemaRef("Tabby"))
	require.Empty(t, tabby.OneOf)
	require.Nil(t, tabby.Discriminator)
	require.Equal(t, []string{"petType", "name", "lives", "stripes"}, tabby.Properties.Keys)

	// The base keeps its variants, and so does a schema that is not one of them.
	pet := doc.Flatten(schemaRef("Pet"))
	require.Len(t, pet.OneOf, 2)
	require.NotNil(t, pet.Discriminator)
	wrapper := doc.Flatten(schemaRef("Wrapper"))
	require.Len(t, wrapper.OneOf, 2)
	require.NotNil(t, wrapper.Discriminator)
}
