package api

import (
	"bytes"
	"encoding/json"
	"maps"
	"slices"

	"github.com/danielgtaylor/huma/v2"
	"github.com/vanclief/ez"

	"github.com/vanclief/compose/components/rest/api/openapi"
	"github.com/vanclief/compose/components/rest/api/validate"
)

var paramLocations = []string{"path", "query", "header"}

// Ready finalizes the reflector, compiles every operation's validator
// and builds the documents. Registering after Ready panics; requests that
// arrive before Ready fail with EINTERNAL. Idempotent: later calls return
// the first result.
func (r *Registry) Ready() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.result != nil || r.readyErr != nil {
		return r.readyErr
	}

	err := r.finalize()
	if err != nil {
		r.readyErr = err
		return err
	}

	r.ready.Store(true)
	return nil
}

// Spec returns the OpenAPI document (hidden operations excluded) as indented
// JSON with sorted keys. It calls Ready.
func (r *Registry) Spec(v Version) ([]byte, error) {
	err := r.Ready()
	if err != nil {
		return nil, err
	}

	spec, ok := r.specs[v]
	if !ok {
		return nil, ez.New(ez.EINVALID, "unknown OpenAPI version", nil)
	}
	return bytes.Clone(spec), nil
}

// StrictSpec returns the 3.1 document including hidden operations, with
// additionalProperties:false on every response object component and no
// component pruning. It is meant for contract tests (see apitest).
func (r *Registry) StrictSpec() ([]byte, error) {
	err := r.Ready()
	if err != nil {
		return nil, err
	}
	return bytes.Clone(r.strict), nil
}

// finalize runs with r.mu held.
func (r *Registry) finalize() error {
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
			return err
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
		return err
	}
	r.specs[V30], err = canonical(v30, true)
	if err != nil {
		return err
	}
	r.strict, err = canonical(strict, false)
	if err != nil {
		return err
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
				Content:     jsonContent(res.ResponseSchema(op.resType)),
			},
			"default": {
				Description: "Error",
				Content:     jsonContent(&huma.Schema{Ref: openapi.RefPrefix + "handler.ErrorResponse"}),
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
	if ok {
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
