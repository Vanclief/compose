package mcp

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/vanclief/ez"

	"github.com/vanclief/compose/components/rest/reference"
)

// schemaRefPrefix is the local ref prefix of component schemas. The terms map
// also holds every term under the ref of its component, so a renderer that
// meets a nested $ref can name the term without the document.
const schemaRefPrefix = "#/components/schemas/"

// snakeName is the shape of composite module and function names.
var snakeName = regexp.MustCompile(`^[a-z0-9_]+$`)

// catalogBuilder holds the state of one buildCatalog call.
type catalogBuilder struct {
	doc     *reference.Document
	catalog *Catalog
	// tagDescriptions maps a snake_case tag name to its description.
	tagDescriptions map[string]string
	// flat maps a component name to its flattened schema. Terms, arguments
	// and returns that stand for the component share this pointer, so a
	// renderer can recognize a term by its schema.
	flat map[string]*reference.Schema
	// termNames maps a component name to its term display name.
	termNames map[string]string
	// outputTypes maps the term names registered by composites to their type.
	outputTypes map[string]reflect.Type
	// accepted and returned map a term name to the functions that accept or
	// return it.
	accepted map[string]map[string]bool
	returned map[string]map[string]bool
}

// buildCatalog derives the catalog from the document, then adds the
// composites and applies the concepts.
func buildCatalog(doc *reference.Document, composites []Composite, concepts map[string]Concept) (*Catalog, error) {
	b := &catalogBuilder{
		doc: doc,
		catalog: &Catalog{
			functions: map[string]*Function{},
			modules:   map[string]*Module{},
			terms:     map[string]*Term{},
			exact:     map[string]*Term{},
		},
		tagDescriptions: map[string]string{},
		flat:            map[string]*reference.Schema{},
		termNames:       map[string]string{},
		outputTypes:     map[string]reflect.Type{},
		accepted:        map[string]map[string]bool{},
		returned:        map[string]map[string]bool{},
	}

	for _, tag := range doc.Tags {
		name := snake(tag.Name)
		_, seen := b.tagDescriptions[name]
		if !seen {
			b.tagDescriptions[name] = tag.Description
		}
	}

	err := b.addSchemaTerms()
	if err != nil {
		return nil, ez.Wrap(err)
	}

	for _, ref := range doc.Operations() {
		if ref.Operation.Internal {
			continue
		}
		err = b.addOperation(ref)
		if err != nil {
			return nil, ez.Wrap(err)
		}
	}

	for _, c := range composites {
		err = b.addComposite(c)
		if err != nil {
			return nil, ez.Wrap(err)
		}
	}

	err = b.applyConcepts(concepts)
	if err != nil {
		return nil, ez.Wrap(err)
	}

	b.finish()
	return b.catalog, nil
}

// addSchemaTerms creates one term per component schema.
func (b *catalogBuilder) addSchemaTerms() error {
	keys := b.doc.Components.Schemas.Keys
	names := displayNames(keys)
	for _, key := range keys {
		b.flat[key] = b.doc.Flatten(b.doc.Components.Schemas.Map[key])
	}
	for _, key := range keys {
		raw := b.doc.Components.Schemas.Map[key]
		term := &Term{Name: names[key], Schema: b.flat[key]}
		if raw != nil {
			term.Description = raw.Description
		}
		err := b.addTerm(term.Name, term)
		if err != nil {
			return ez.Wrap(err)
		}
		b.termNames[key] = term.Name
		b.catalog.Terms = append(b.catalog.Terms, term)

		b.catalog.exact[schemaRefPrefix+key] = term
		ref := normalizeTerm(schemaRefPrefix + key)
		_, taken := b.catalog.terms[ref]
		if !taken {
			b.catalog.terms[ref] = term
		}
	}
	return nil
}

// displayNames shortens component keys to their last segment, split on "_"
// and ".", adding segments to the names that collide until every name is
// unique. Keys that stay ambiguous at full length keep the raw key.
func displayNames(keys []string) map[string]string {
	segments := map[string][]string{}
	depth := map[string]int{}
	for _, key := range keys {
		segments[key] = strings.FieldsFunc(key, func(r rune) bool {
			return r == '_' || r == '.'
		})
		depth[key] = 1
	}

	names := map[string]string{}
	for {
		groups := map[string][]string{}
		for _, key := range keys {
			names[key] = joinLast(segments[key], depth[key], key)
			group := normalizeTerm(names[key])
			groups[group] = append(groups[group], key)
		}

		grew := false
		for _, group := range groups {
			if len(group) < 2 {
				continue
			}
			for _, key := range group {
				if depth[key] < len(segments[key]) {
					depth[key]++
					grew = true
				}
			}
		}
		if grew {
			continue
		}

		for _, group := range groups {
			if len(group) < 2 {
				continue
			}
			for _, key := range group {
				names[key] = key
			}
		}
		return names
	}
}

// joinLast joins the last n segments with ".", or returns fallback when there
// are no segments.
func joinLast(segments []string, n int, fallback string) string {
	if len(segments) == 0 {
		return fallback
	}
	return strings.Join(segments[len(segments)-n:], ".")
}

// addTerm registers t under the exact name and the lookup key of name. An
// exact name already taken by another term is an error. A lookup key taken by
// another term, as for components "user" and "User", stays with the first
// term; the second is still found by its exact name.
func (b *catalogBuilder) addTerm(name string, t *Term) error {
	exact := strings.TrimSpace(name)
	existing, ok := b.catalog.exact[exact]
	if ok && existing != t {
		return ez.New(ez.EINVALID, fmt.Sprintf("mcp: term name %q is used by %q and %q", name, existing.Name, t.Name), nil)
	}
	b.catalog.exact[exact] = t

	key := normalizeTerm(name)
	_, taken := b.catalog.terms[key]
	if !taken {
		b.catalog.terms[key] = t
	}
	return nil
}

// addOperation adds the function of one document operation.
func (b *catalogBuilder) addOperation(ref reference.OperationRef) error {
	op := ref.Operation
	// grug: a request body without a JSON media type (multipart uploads,
	// raw files) cannot be sent through call_function, so the operation is
	// left out of the catalog rather than offered and failing.
	if !jsonBody(b.doc.RequestBody(op.RequestBody)) {
		return nil
	}
	module, function := operationName(ref)
	if module == "" || function == "" {
		return ez.New(ez.EINVALID, fmt.Sprintf("mcp: operation %s %s has no usable name", ref.Method, ref.Path), nil)
	}

	f := &Function{
		Name:        module + "." + function,
		Module:      module,
		Summary:     operationSummary(ref),
		Description: op.Description,
		Method:      ref.Method,
		Path:        ref.Path,
		Deprecated:  op.Deprecated,
		ReadOnly:    ref.Method == "GET" || ref.Method == "HEAD",
		Destructive: ref.Method == "DELETE",
		doc:         b.doc,
	}

	for _, p := range b.doc.Parameters(ref.Item, op) {
		if p.In != "path" && p.In != "query" && p.In != "header" {
			continue
		}
		if p.In == "header" && dispatchHeader(p.Name) {
			continue
		}
		schema := b.argSchema(p.Schema)
		// dispatch refuses null for a path argument, so it never takes the
		// nullable wrapper; argParts and InputSchema drop the null a path
		// schema still carries.
		if p.In == "path" {
			schema = b.schema(p.Schema)
		}
		f.Args = append(f.Args, &Arg{
			Name:        p.Name,
			In:          p.In,
			Required:    p.Required,
			Description: p.Description,
			Schema:      schema,
			param:       p,
		})
		b.index(b.accepted, b.termOf(p.Schema), f.Name)
	}

	b.addBodyArgs(f, op)

	response := b.successResponse(op)
	if response != nil {
		media := preferredMedia(response.Content)
		if media != nil && media.Schema != nil {
			f.Returns = b.refTerm(media.Schema)
			f.ReturnSchema = b.schema(media.Schema)
			b.index(b.returned, b.termOf(media.Schema), f.Name)
		}
	}

	return b.addFunction(f)
}

// dispatchHeader reports header parameters that dispatch sets itself, so
// they are not offered as arguments.
func dispatchHeader(name string) bool {
	switch strings.ToLower(name) {
	case "accept", "content-type", "authorization":
		return true
	}
	return false
}

// jsonBody reports whether a request body can be sent as JSON: it is absent,
// declares no content, or has a media type starting with application/json.
func jsonBody(body *reference.RequestBody) bool {
	if body == nil || body.Content.Len() == 0 {
		return true
	}
	return jsonMedia(body.Content) != nil
}

// jsonMedia returns the application/json media type, else the first whose
// name starts with application/json, else nil.
func jsonMedia(content reference.Ordered[*reference.MediaType]) *reference.MediaType {
	media, ok := content.Get("application/json")
	if ok {
		return media
	}
	for _, key := range content.Keys {
		if strings.HasPrefix(strings.ToLower(key), "application/json") {
			return content.Map[key]
		}
	}
	return nil
}

// operationName returns the snake_case module and function names of an
// operation: from a dotted operationId, else the first tag and the
// operationId, else the first tag and the method and path words, with a
// "{name}" segment as "by name" so GET /users and GET /users/{id} differ.
func operationName(ref reference.OperationRef) (string, string) {
	op := ref.Operation
	module := "default"
	if len(op.Tags) > 0 && snake(op.Tags[0]) != "" {
		module = snake(op.Tags[0])
	}

	id := strings.TrimSpace(op.OperationID)
	if id == "" {
		var words []string
		for _, segment := range strings.Split(ref.Path, "/") {
			if segment == "" {
				continue
			}
			if strings.HasPrefix(segment, "{") {
				segment = "by " + strings.Trim(segment, "{}")
			}
			words = append(words, segment)
		}
		return module, snake(ref.Method + " " + strings.Join(words, " "))
	}

	before, after, dotted := strings.Cut(id, ".")
	if dotted {
		return snake(before), snake(after)
	}
	return module, snake(id)
}

// operationSummary returns the summary, else the first sentence of the
// description, else "METHOD /path".
func operationSummary(ref reference.OperationRef) string {
	summary := strings.TrimSpace(ref.Operation.Summary)
	if summary != "" {
		return summary
	}
	summary = firstSentence(ref.Operation.Description)
	if summary != "" {
		return summary
	}
	return ref.Method + " " + ref.Path
}

// firstSentence returns the first line of text, cut after its first sentence.
func firstSentence(text string) string {
	line := firstLine(text)
	sentence, _, found := strings.Cut(line, ". ")
	if found {
		return sentence + "."
	}
	return line
}

// firstLine returns the first non-empty line of text, trimmed.
func firstLine(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	return strings.TrimSpace(line)
}

// addBodyArgs appends the request body to the arguments: one argument per
// writable property of an object body, or a single "body" argument when the
// body is not a plain object or one of its properties shares a parameter
// name. readOnly properties are left out.
func (b *catalogBuilder) addBodyArgs(f *Function, op *reference.Operation) {
	body := b.doc.RequestBody(op.RequestBody)
	if body == nil {
		return
	}
	media := jsonMedia(body.Content)
	if media == nil || media.Schema == nil {
		return
	}
	raw := media.Schema
	b.index(b.accepted, b.termOf(raw), f.Name)

	single := &Arg{
		Name:        "body",
		In:          "body",
		Required:    body.Required,
		Description: body.Description,
		Schema:      b.argSchema(raw),
	}

	flat := b.doc.Flatten(raw)
	if !plainObject(flat) {
		f.Args = append(f.Args, single)
		f.bodyWhole = true
		return
	}

	for _, arg := range f.Args {
		prop, collides := flat.Properties.Get(arg.Name)
		if collides && !readOnly(b.doc, prop) {
			f.Args = append(f.Args, single)
			f.bodyWhole = true
			return
		}
	}

	for _, name := range flat.Properties.Keys {
		prop := flat.Properties.Map[name]
		if readOnly(b.doc, prop) {
			continue
		}
		arg := &Arg{
			Name:     name,
			In:       "body",
			Required: flat.IsRequired(name),
			Schema:   b.argSchema(prop),
		}
		if prop != nil {
			arg.Description = prop.Description
		}
		f.Args = append(f.Args, arg)
		b.index(b.accepted, b.termOf(prop), f.Name)
	}
}

// readOnly reports whether a property is readOnly, on the property itself or
// on the schema it resolves and flattens to in doc.
func readOnly(doc *reference.Document, prop *reference.Schema) bool {
	if prop == nil {
		return false
	}
	flat := doc.Flatten(prop)
	return prop.ReadOnly || (flat != nil && flat.ReadOnly)
}

// plainObject reports whether s is an object with properties, no
// alternatives and no rule over its properties together, the only body
// shape that is flattened into arguments.
func plainObject(s *reference.Schema) bool {
	if s == nil || s.Properties.Len() == 0 {
		return false
	}
	if len(s.OneOf) > 0 || len(s.AnyOf) > 0 {
		return false
	}
	// A rule over the properties together has no argument to live in.
	if s.Not != nil || s.MinProperties != nil || s.MaxProperties != nil {
		return false
	}
	return len(s.Type) == 0 || s.Type.Has("object")
}

// successResponse returns the response with the lowest 2xx status, else the
// default response, else nil.
func (b *catalogBuilder) successResponse(op *reference.Operation) *reference.Response {
	best := ""
	for _, code := range op.Responses.Keys {
		if len(code) != 3 || code[0] != '2' {
			continue
		}
		if best == "" || code < best {
			best = code
		}
	}
	if best == "" {
		best = "default"
	}
	response, ok := op.Responses.Get(best)
	if !ok {
		return nil
	}
	return b.doc.Response(response)
}

// preferredMedia returns the application/json media type, else the first one.
func preferredMedia(content reference.Ordered[*reference.MediaType]) *reference.MediaType {
	media, ok := content.Get("application/json")
	if ok {
		return media
	}
	if content.Len() == 0 {
		return nil
	}
	return content.Map[content.Keys[0]]
}

// schema returns the schema the catalog stores for s: the shared flattened
// component when s stands for one, else s resolved and flattened.
func (b *catalogBuilder) schema(s *reference.Schema) *reference.Schema {
	if s == nil {
		return nil
	}
	flat, ok := b.flat[b.doc.SchemaName(s)]
	if ok {
		return flat
	}
	return b.doc.Flatten(s)
}

// argSchema returns schema(s) for an argument, except that a nullable
// wrapper of a non-nullable component, {$ref, nullable} or {nullable, allOf:
// [$ref]}, becomes a nullable single-member allOf of a ref to the component,
// so the null reaches describe_function and InputSchema, the term is still
// named, and the cycle guard of InputSchema sees the component name.
func (b *catalogBuilder) argSchema(s *reference.Schema) *reference.Schema {
	flat := b.schema(s)
	name := b.doc.SchemaName(s)
	_, shared := b.flat[name]
	if !shared || !s.Nullable || flat == nil || flat.Nullable {
		return flat
	}
	return &reference.Schema{AllOf: []*reference.Schema{{Ref: schemaRefPrefix + name}}, Nullable: true, NullMember: s.NullMember}
}

// refTerm returns the term name of the component s refers to, or "".
func (b *catalogBuilder) refTerm(s *reference.Schema) string {
	name := b.doc.SchemaName(s)
	if name == "" {
		return ""
	}
	return b.termNames[name]
}

// termOf returns the term s refers to directly or through array items, or "".
func (b *catalogBuilder) termOf(s *reference.Schema) string {
	if s == nil {
		return ""
	}
	term := b.refTerm(s)
	if term != "" {
		return term
	}
	resolved, _ := b.doc.Schema(s)
	if resolved == nil || resolved.Items == nil {
		return ""
	}
	return b.refTerm(resolved.Items)
}

// index records that function uses term in the given reverse index.
func (b *catalogBuilder) index(index map[string]map[string]bool, term, function string) {
	if term == "" {
		return
	}
	if index[term] == nil {
		index[term] = map[string]bool{}
	}
	index[term][function] = true
}

// addFunction registers f and files it under its module.
func (b *catalogBuilder) addFunction(f *Function) error {
	_, exists := b.catalog.functions[f.Name]
	if exists {
		return ez.New(ez.EINVALID, fmt.Sprintf("mcp: duplicate function name %q", f.Name), nil)
	}
	b.catalog.functions[f.Name] = f

	m, ok := b.catalog.modules[f.Module]
	if !ok {
		m = &Module{Name: f.Module, Description: b.tagDescriptions[f.Module]}
		b.catalog.modules[f.Module] = m
		b.catalog.Modules = append(b.catalog.Modules, m)
	}
	m.Functions = append(m.Functions, f)
	return nil
}

// addComposite adds a hand-written function. Its argument and return schemas
// come from the Input and Output types by reflection.
func (b *catalogBuilder) addComposite(c Composite) error {
	if !snakeName.MatchString(c.Module) || !snakeName.MatchString(c.Name) {
		return ez.New(ez.EINVALID, fmt.Sprintf("mcp: composite %q in module %q must have a snake_case module and name", c.Name, c.Module), nil)
	}

	composite := c
	f := &Function{
		Name:        c.Module + "." + c.Name,
		Module:      c.Module,
		Summary:     c.Summary,
		Description: c.Description,
		Composite:   true,
		ReadOnly:    c.ReadOnly,
		composite:   &composite,
	}

	inputType := derefType(reflect.TypeOf(c.Input))
	if inputType != nil && inputType.Kind() == reflect.Struct {
		input, err := reflectSchema(inputType)
		if err != nil {
			return ez.Wrap(err)
		}
		for _, name := range input.Properties.Keys {
			prop := input.Properties.Map[name]
			f.Args = append(f.Args, &Arg{
				Name:        name,
				In:          "body",
				Required:    input.IsRequired(name),
				Description: prop.Description,
				Schema:      prop,
			})
		}
	}

	outputType := reflect.TypeOf(c.Output)
	if outputType != nil {
		output, err := reflectSchema(outputType)
		if err != nil {
			return ez.Wrap(err)
		}
		f.ReturnSchema = output
		named := derefType(outputType)
		if named.Kind() == reflect.Struct && named.Name() != "" {
			term, err := b.outputTerm(named, output)
			if err != nil {
				return ez.Wrap(err)
			}
			f.Returns = term.Name
			f.ReturnSchema = term.Schema
			b.index(b.returned, term.Name, f.Name)
		}
	}

	return b.addFunction(f)
}

// outputTerm returns the term of a composite output type, registering it on
// first use. A name taken by another term is an error.
func (b *catalogBuilder) outputTerm(t reflect.Type, schema *reference.Schema) (*Term, error) {
	registered, ok := b.outputTypes[t.Name()]
	if ok && registered == t {
		existing, _ := b.catalog.Term(t.Name())
		return existing, nil
	}
	term := &Term{Name: t.Name(), Schema: schema}
	_, taken := b.catalog.Term(t.Name())
	if taken {
		return nil, ez.New(ez.EINVALID, fmt.Sprintf("mcp: composite output type %s collides with term %q", t, t.Name()), nil)
	}
	err := b.addTerm(term.Name, term)
	if err != nil {
		return nil, ez.Wrap(err)
	}
	b.outputTypes[t.Name()] = t
	b.catalog.Terms = append(b.catalog.Terms, term)
	return term, nil
}

// derefType follows pointer types. A nil type stays nil.
func derefType(t reflect.Type) reflect.Type {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

// reflectSchema derives the JSON Schema of a Go type and decodes it into the
// document model. The schema is parsed as the only component of a minimal
// document so it gets the same 3.0/3.1 normalization as the spec.
func reflectSchema(t reflect.Type) (*reference.Schema, error) {
	inferred, err := jsonschema.ForType(t, nil)
	if err != nil {
		return nil, ez.New(ez.EINVALID, "mcp: cannot derive a JSON Schema for "+t.String(), err)
	}
	data, err := json.Marshal(inferred)
	if err != nil {
		return nil, ez.New(ez.EINVALID, "mcp: cannot encode the JSON Schema of "+t.String(), err)
	}
	wrapped := `{"openapi":"3.1.0","components":{"schemas":{"type":` + string(data) + `}}}`
	doc, err := reference.Parse([]byte(wrapped))
	if err != nil {
		return nil, ez.Wrap(err)
	}
	return doc.Components.Schemas.Map["type"], nil
}

// applyConcepts sets the description and aliases of existing terms and adds
// concept-only terms, in name order.
func (b *catalogBuilder) applyConcepts(concepts map[string]Concept) error {
	names := make([]string, 0, len(concepts))
	for name := range concepts {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		concept := concepts[name]
		if strings.TrimSpace(name) == "" {
			return ez.New(ez.EINVALID, "mcp: concept with an empty name", nil)
		}

		term, ok := b.catalog.Term(name)
		if !ok {
			term = &Term{Name: strings.TrimSpace(name)}
			err := b.addTerm(name, term)
			if err != nil {
				return ez.Wrap(err)
			}
			b.catalog.Terms = append(b.catalog.Terms, term)
		}
		if concept.Text != "" {
			term.Description = concept.Text
		}

		for _, alias := range concept.Aliases {
			alias = strings.TrimSpace(alias)
			if alias == "" {
				continue
			}
			existing, taken := b.catalog.Term(alias)
			if taken && existing == term {
				continue
			}
			if taken {
				return ez.New(ez.EINVALID, fmt.Sprintf("mcp: alias %q of %q already names %q", alias, term.Name, existing.Name), nil)
			}
			err := b.addTerm(alias, term)
			if err != nil {
				return ez.Wrap(err)
			}
			term.Aliases = append(term.Aliases, alias)
		}
	}
	return nil
}

// finish fills the reverse index and sorts modules, functions and terms.
func (b *catalogBuilder) finish() {
	for _, term := range b.catalog.Terms {
		term.AcceptedBy = sortedKeys(b.accepted[term.Name])
		term.ReturnedBy = sortedKeys(b.returned[term.Name])
	}

	sort.Slice(b.catalog.Modules, func(i, j int) bool {
		return b.catalog.Modules[i].Name < b.catalog.Modules[j].Name
	})
	for _, m := range b.catalog.Modules {
		sort.Slice(m.Functions, func(i, j int) bool {
			return m.Functions[i].Name < m.Functions[j].Name
		})
	}
	sort.Slice(b.catalog.Terms, func(i, j int) bool {
		return b.catalog.Terms[i].Name < b.catalog.Terms[j].Name
	})
}

// sortedKeys returns the keys of set in order, or nil when it is empty.
func sortedKeys(set map[string]bool) []string {
	if len(set) == 0 {
		return nil
	}
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
