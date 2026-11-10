package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"

	"github.com/vanclief/ez"

	"github.com/vanclief/compose/components/rest/api/internal/huma"
	"github.com/vanclief/compose/components/rest/api/openapi"
	"github.com/vanclief/compose/components/rest/api/validate"
)

var paramLocations = []string{"path", "query", "header"}

// Ready finalizes the reflector, compiles every operation's validator
// and builds the documents. Like every other invalid registration, a
// failure here panics with the ez error, as does a protected operation
// (hidden ones included) without an authenticator: every operation needs
// Authenticate or Public, from api.New, a group or itself. Before any of
// that it calls the Echo instance's HTTPErrorHandler once with an ez
// EINVALID error on a made-up request and panics unless the handler answers
// it with a 4xx, since operations return every error to Echo: Echo's default
// handler (a 500), a nil one, or one that writes nothing fails, so the app
// sets e.HTTPErrorHandler = rest.ErrorHandler() (or its own) first. That call
// reaches the handler's reporter and translator like any error, with
// handler.Request nil. It also panics, naming the operation, when a route
// registered on Echo after an operation, on its method and path, replaced it
// (Echo's default router allows that); a replacement made after Ready goes
// unnoticed. Registering after Ready panics; requests that arrive
// before Ready fail with EINTERNAL. Idempotent once it succeeds: later calls
// are no-ops, while a call that panicked leaves the registry not ready and
// the next one checks everything again.
func (r *Registry) Ready() {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.result != nil {
		return
	}

	err := r.checkErrorHandler()
	if err != nil {
		panic(err)
	}

	err = r.checkRoutes()
	if err != nil {
		panic(err)
	}

	err = r.finalize()
	if err != nil {
		panic(err)
	}

	r.ready.Store(true)
}

// probeMessage is the message of the error checkErrorHandler hands the
// app's error handler.
const probeMessage = "api: Ready checks that the Echo error handler answers an ez error with its status"

// checkErrorHandler calls the Echo instance's HTTPErrorHandler once with an
// ez EINVALID error on a made-up request and fails unless it answers with a
// 4xx. A panic in the handler propagates as it is.
func (r *Registry) checkErrorHandler() error {
	const fix = "set e.HTTPErrorHandler = rest.ErrorHandler() (or the app's own handler, answering an ez error with its status) before Ready"
	if r.echo.HTTPErrorHandler == nil {
		return ez.New(ez.EINVALID, "api: the Echo error handler is nil, so no error an operation returns is answered: "+fix, nil)
	}

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	c := r.echo.NewContext(req, rec)
	r.echo.HTTPErrorHandler(c, ez.New(ez.EINVALID, probeMessage, nil))

	if rec.Code < http.StatusBadRequest || rec.Code >= http.StatusInternalServerError {
		msg := fmt.Sprintf("api: the Echo error handler answered an ez EINVALID error with %d, not a 4xx (Echo's default handler answers every error an operation returns as a 500): %s", rec.Code, fix)
		return ez.New(ez.EINVALID, msg, nil)
	}
	return nil
}

// checkRoutes fails when an operation no longer owns its route. Echo's
// default router lets a route registered later on the same method and path,
// whatever its placeholder names, replace the registry's, whose
// authentication and validation then never run. It asks the router which
// route a request to the operation's own path reaches (/runs/:id, its
// placeholders read as values, which no static route matches): the router's
// route list cannot tell, as it keeps a replaced route beside a replacement
// that names its placeholders differently. A router that reports no route at
// all cannot answer and passes.
func (r *Registry) checkRoutes() error {
	for _, op := range r.ops {
		req := httptest.NewRequest(op.method, "/", nil)
		req.URL.Path = op.path
		c := r.echo.NewContext(req, httptest.NewRecorder())
		r.echo.Router().Route(c)
		route := c.RouteInfo()
		if route.Name == op.routeName() {
			continue
		}
		if route.Name == "" && route.Path == "" {
			continue
		}
		return op.registrationError(ez.New(ez.EINVALID, "another route was registered on its method and path after the registry's and replaced it, so its authentication and validation do not run: remove that route or move it to another path", nil))
	}
	return nil
}

// Spec returns the OpenAPI document (hidden operations excluded) as indented
// JSON with sorted keys. It calls Ready.
func (r *Registry) Spec(v Version) ([]byte, error) {
	r.Ready()

	spec, ok := r.specs[v]
	if !ok {
		return nil, ez.New(ez.EINVALID, "unknown OpenAPI version", nil)
	}
	return bytes.Clone(spec), nil
}

// StrictSpec returns the 3.1 document including hidden operations, so with
// the components only they use, with additionalProperties:false on every
// response object component and with the properties of hidden:"true" fields
// listed (optional), since a response still sends them. It is meant for
// contract tests (see apitest). It calls Ready.
func (r *Registry) StrictSpec() []byte {
	r.Ready()
	return bytes.Clone(r.strict)
}

// finalize runs with r.mu held.
func (r *Registry) finalize() error {
	var unauthenticated []string
	for _, op := range r.ops {
		if !op.public && op.authenticate == nil {
			unauthenticated = append(unauthenticated, op.id)
		}
	}
	if len(unauthenticated) > 0 {
		return ez.New(ez.EINVALID, "api: operations without an authenticator (use api.Authenticate or api.Public): "+strings.Join(unauthenticated, ", "), nil)
	}

	res, err := r.reflector.Finalize()
	if err != nil {
		return ez.Wrap(err)
	}

	// Validators compile against every component, including those only
	// hidden operations reference (huma prunes them from the public doc).
	compileDoc, err := json.Marshal(map[string]any{
		"openapi":    "3.1.0",
		"info":       &r.info,
		"paths":      map[string]any{},
		"components": map[string]any{"schemas": res.Components},
	})
	if err != nil {
		return ez.New(ez.EINTERNAL, "could not encode the validation document", err)
	}

	compiler, err := validate.NewCompiler(compileDoc)
	if err != nil {
		return ez.Wrap(err)
	}

	for _, op := range r.ops {
		err = op.compile(compiler, res)
		if err != nil {
			return ez.Wrap(err)
		}
	}

	public := r.document(res, res.Components, false)

	v31, err := public.MarshalJSON()
	if err != nil {
		return ez.New(ez.EINTERNAL, "could not encode the OpenAPI 3.1 document", err)
	}

	v30, err := public.Downgrade()
	if err != nil {
		return ez.New(ez.EINTERNAL, "could not downgrade the OpenAPI document", err)
	}

	strict, err := r.document(res, res.StrictComponents(), true).MarshalJSON()
	if err != nil {
		return ez.New(ez.EINTERNAL, "could not encode the strict OpenAPI document", err)
	}

	r.specs = map[Version][]byte{}
	r.specs[V31], err = canonical(v31, false)
	if err != nil {
		return ez.Wrap(err)
	}
	r.specs[V30], err = canonical(v30, true)
	if err != nil {
		return ez.Wrap(err)
	}
	r.strict, err = canonical(strict, false)
	if err != nil {
		return ez.Wrap(err)
	}

	r.result = res
	return nil
}

// compile prepares the coercion kinds, body properties and the validator.
func (op *operation) compile(compiler *validate.Compiler, res *openapi.Result) error {
	op.kinds = make([]validate.Kind, len(op.params))
	for i, p := range op.params {
		op.kinds[i] = kindOf(p.Schema, res.Components)
	}
	ref, _, ok := res.BodyRef(op.reqType)
	if ok {
		op.bodySchema = &huma.Schema{Ref: ref}
	}
	if ok && len(op.files) > 0 {
		op.formKinds = formKindsOf(ref, res.Components)
	}

	schema, err := op.validatorSchema(res)
	if err != nil {
		return err
	}

	op.schema, err = compiler.Compile("op:"+op.id, schema)
	if err != nil {
		return ez.Wrap(err)
	}
	return nil
}

// validatorSchema builds the synthetic {path, query, header, body} schema.
// Every location that declares parameters is always present in the instance
// (possibly empty), so it is required; body is required when its schema has
// required properties.
func (op *operation) validatorSchema(res *openapi.Result) (map[string]any, error) {
	props := map[string]any{}
	required := []string{}

	for _, in := range paramLocations {
		locProps := map[string]any{}
		locRequired := []string{}
		for _, p := range op.params {
			if p.In != in {
				continue
			}
			raw, err := json.Marshal(p.Schema)
			if err != nil {
				return nil, ez.New(ez.EINTERNAL, "could not encode parameter "+p.Name, err)
			}
			locProps[p.Name] = json.RawMessage(specRefs(raw))
			if p.Required {
				locRequired = append(locRequired, p.Name)
			}
		}
		if len(locProps) == 0 {
			continue
		}

		props[in] = map[string]any{"type": "object", "properties": locProps, "required": locRequired}
		required = append(required, in)
	}

	ref, bodyRequired, ok := res.BodyRef(op.reqType)
	op.hasBody = ok
	if ok {
		props["body"] = map[string]any{"$ref": "spec.json" + ref}
		if bodyRequired {
			required = append(required, "body")
		}
	}

	return map[string]any{"type": "object", "properties": props, "required": required}, nil
}

// specRefs points component refs at the "spec.json" resource.
func specRefs(raw []byte) []byte {
	return bytes.ReplaceAll(raw, []byte(`"`+openapi.RefPrefix), []byte(`"spec.json`+openapi.RefPrefix))
}

// document builds the OpenAPI 3.1 document on huma's types.
func (r *Registry) document(res *openapi.Result, comps map[string]*huma.Schema, includeHidden bool) *huma.OpenAPI {
	schemas := huma.NewMapRegistry(openapi.RefPrefix, huma.DefaultSchemaNamer)
	maps.Copy(schemas.Map(), comps)

	info := r.info
	doc := &huma.OpenAPI{
		OpenAPI:    "3.1.0",
		Info:       &info,
		Servers:    r.servers,
		Components: &huma.Components{Schemas: schemas},
	}
	if r.apiKeyHeader != "" {
		doc.Components.SecuritySchemes = map[string]*huma.SecurityScheme{
			securitySchemeName: {Type: "apiKey", In: "header", Name: r.apiKeyHeader},
		}
	}

	var tags []string
	for _, op := range r.ops {
		if op.hidden && !includeHidden {
			continue
		}
		doc.AddOperation(r.operationDoc(res, op))
		if op.tag != "" && !slices.Contains(tags, op.tag) {
			tags = append(tags, op.tag)
		}
	}

	slices.Sort(tags)
	for _, tag := range tags {
		doc.Tags = append(doc.Tags, &huma.Tag{Name: tag})
	}

	return doc
}

func (r *Registry) operationDoc(res *openapi.Result, op *operation) *huma.Operation {
	o := &huma.Operation{
		Method:      op.method,
		Path:        specPath(op.path),
		OperationID: op.id,
		Summary:     op.summary,
		Description: op.description,
		Deprecated:  op.deprecated,
		Responses: map[string]*huma.Response{
			"200": {
				Description: "OK",
				Content:     op.responseContent(res),
			},
			"default": {
				Description: "Error",
				Content:     jsonContent(res.ResponseSchema(errorResponseType)),
			},
		},
	}
	if op.tag != "" {
		o.Tags = []string{op.tag}
	}

	for _, p := range op.params {
		o.Parameters = append(o.Parameters, &huma.Param{
			Name:        p.Name,
			In:          p.In,
			Required:    p.Required,
			Schema:      p.Schema,
			Description: p.Doc,
			Deprecated:  p.Deprecated,
		})
	}

	ref, required, ok := res.BodyRef(op.reqType)
	if len(op.files) > 0 {
		o.RequestBody = op.multipartBody(res)
	} else if ok {
		o.RequestBody = &huma.RequestBody{Required: required, Content: jsonContent(&huma.Schema{Ref: ref})}
	}

	if r.apiKeyHeader != "" && !op.public {
		o.Security = []map[string][]string{{securitySchemeName: {}}}
	}

	return o
}

func jsonContent(s *huma.Schema) map[string]*huma.MediaType {
	return map[string]*huma.MediaType{"application/json": {Schema: s}}
}

// canonical re-encodes a JSON document with sorted keys, 2-space indent and
// no HTML escaping. With fix30 it also rewrites what Downgrade leaves
// invalid in 3.0 (anyOf with a {type: null} branch).
func canonical(raw []byte, fix30 bool) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()

	var v any
	err := dec.Decode(&v)
	if err != nil {
		return nil, ez.New(ez.EINTERNAL, "could not decode the OpenAPI document", err)
	}

	if fix30 {
		fixNullable(v)
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")

	err = enc.Encode(v)
	if err != nil {
		return nil, ez.New(ez.EINTERNAL, "could not encode the OpenAPI document", err)
	}
	return buf.Bytes(), nil
}

// fixNullable rewrites what 3.0 cannot express. A $ref with siblings
// (a field-level enum narrowing an enum component) becomes
// {allOf: [{$ref}], <siblings>}, since 3.0 ignores $ref siblings. anyOf
// [X, {type: null}] becomes {nullable: true, allOf: [X]} for a $ref X, or X
// merged with nullable: true for an inline X. With more than one non-null
// branch the null branch is dropped and nullable: true added.
func fixNullable(v any) {
	switch t := v.(type) {
	case []any:
		for _, item := range t {
			fixNullable(item)
		}
	case map[string]any:
		for _, child := range t {
			fixNullable(child)
		}
		rewriteRefSiblings(t)
		rewriteAnyOfNull(t)
	}
}

func rewriteRefSiblings(m map[string]any) {
	ref, ok := m["$ref"].(string)
	_, hasAllOf := m["allOf"]
	if !ok || len(m) == 1 || hasAllOf {
		return
	}
	delete(m, "$ref")
	m["allOf"] = []any{map[string]any{"$ref": ref}}
}

func rewriteAnyOfNull(m map[string]any) {
	list, ok := m["anyOf"].([]any)
	if !ok {
		return
	}

	var rest []any
	nulls := 0
	for _, item := range list {
		if isNullSchema(item) {
			nulls++
			continue
		}
		rest = append(rest, item)
	}
	if nulls != 1 || len(rest) == 0 {
		return
	}

	delete(m, "anyOf")
	m["nullable"] = true

	x, ok := rest[0].(map[string]any)
	if len(rest) > 1 || !ok {
		m["anyOf"] = rest
		return
	}

	_, isRef := x["$ref"]
	if isRef {
		m["allOf"] = []any{x}
		return
	}
	for k, val := range x {
		_, exists := m[k]
		if !exists {
			m[k] = val
		}
	}
}

func isNullSchema(v any) bool {
	m, ok := v.(map[string]any)
	return ok && len(m) == 1 && m["type"] == "null"
}
