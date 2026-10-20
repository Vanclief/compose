package generator

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/vanclief/ez"
	"gopkg.in/yaml.v3"
)

// Spec is a generated OpenAPI document.
type Spec struct {
	// Document is the OpenAPI document. Maps are plain map[string]any (components.schemas is a
	// map[string]map[string]any), so encoders sort their keys and the output is deterministic.
	Document map[string]any
	// Routes are all parsed routes, skipped and deprecated ones included.
	Routes []Route

	// publicRoots are the request and response schemas of the routed handlers that are not
	// internal, the roots Public prunes components from.
	publicRoots []any
}

// JSON encodes the document with two-space indentation and a trailing newline.
func (s *Spec) JSON() ([]byte, error) {
	data, err := json.MarshalIndent(s.Document, "", "  ")
	if err != nil {
		return nil, ez.Wrap(err)
	}

	return append(data, '\n'), nil
}

// YAML encodes the document with yaml.v3's default four-space indentation.
func (s *Spec) YAML() ([]byte, error) {
	data, err := yaml.Marshal(s.Document)
	if err != nil {
		return nil, ez.Wrap(err)
	}

	return data, nil
}

type generator struct {
	cfg     Config
	builder *schemaBuilder
	sources []handlerSource
	aliases map[string]string
	// operationIDs maps "METHOD path" to the route's operationId.
	operationIDs map[string]string
}

// Generate builds the OpenAPI document of the application at cfg.Root.
//
// After every hook has run, component schemas nothing reaches are pruned. A schema is
// reached from an operation, or from the request or response type of a routed handler (a
// GET request type travels as parameters, yet SDKs still name it); drop such a schema in the
// Schemas hook to hide it. Generation fails when a $ref does not resolve.
//
// The document is the full spec: Internal operations are in it, marked `x-internal: true`.
// Spec.Public derives the spec without them.
func Generate(cfg Config) (*Spec, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, ez.Wrap(err)
	}

	builder, err := newSchemaBuilder(cfg)
	if err != nil {
		return nil, ez.Wrap(err)
	}

	g := &generator{
		cfg:     cfg,
		builder: builder,
	}

	err = g.loadHandlerSources()
	if err != nil {
		return nil, ez.Wrap(err)
	}

	facts := g.handlerFacts()

	handlerSchemas, err := g.buildHandlerSchemas(facts)
	if err != nil {
		return nil, ez.Wrap(err)
	}

	err = builder.unresolvedError()
	if err != nil {
		return nil, ez.Wrap(err)
	}
	err = builder.collisionError()
	if err != nil {
		return nil, ez.Wrap(err)
	}

	if cfg.Schemas != nil {
		cfg.Schemas(builder.schemas)
	}
	err = builder.splitRequestSide(handlerSchemas)
	if err != nil {
		return nil, ez.Wrap(err)
	}
	g.stripPathFields(handlerSchemas)

	routes, err := parseRoutes(cfg)
	if err != nil {
		return nil, ez.Wrap(err)
	}

	for i := range routes {
		key := routes[i].HandlerKey()
		_, routes[i].BindsBody = facts.bindsBody[key]
		_, routes[i].Negotiates = facts.negotiates[key]
	}

	err = ensureNoDuplicateOperations(routes)
	if err != nil {
		return nil, ez.Wrap(err)
	}
	g.operationIDs, err = g.assignOperationIDs(routes)
	if err != nil {
		return nil, ez.Wrap(err)
	}

	document := g.buildOpenAPISpec(routes, facts, handlerSchemas)

	var roots []any
	var publicRoots []any
	for _, r := range routes {
		handler, ok := handlerSchemas[r.HandlerKey()]
		if !ok {
			continue
		}
		roots = append(roots, handler.Request, handler.Response)
		if !r.Deprecated && !g.internal(r.Method, toOpenAPIPath(r.Path)) {
			publicRoots = append(publicRoots, handler.Request, handler.Response)
		}
	}
	pruneSchemas(document, builder.schemas, roots)
	g.tagSchemas(routes, facts)
	g.tagResources(document, routes, facts, handlerSchemas)
	fillNullableTypes(document, builder.schemas)

	err = validateRefs(document, builder.schemas)
	if err != nil {
		return nil, ez.Wrap(err)
	}

	return &Spec{
		Document:    document,
		Routes:      routes,
		publicRoots: publicRoots,
	}, nil
}

// Public returns a copy of the spec without its internal operations (`x-internal: true`),
// deprecated routes included (see Config.DeprecatedWrapper), and without the x-source of the
// remaining ones.
// Path items left empty and tags no remaining operation uses are dropped, and component
// schemas are pruned with Generate's rule computed over what is left: the schemas the
// remaining operations reach plus the request and response types of every routed handler
// that is not Internal (skipped routes included). That is deliberately not strict pruning,
// SDKs still name those types; for strict pruning, pass nil instead of s.publicRoots to
// pruneSchemas below. Public fails when a $ref of the result does not resolve.
func (s *Spec) Public() (*Spec, error) {
	document := cloneSchema(s.Document)

	used := map[string]bool{}
	paths, _ := document["paths"].(map[string]any)
	for path, itemRaw := range paths {
		item, ok := itemRaw.(map[string]any)
		if !ok {
			continue
		}
		for method, opRaw := range item {
			op, ok := opRaw.(map[string]any)
			if !ok {
				continue
			}
			if op["x-internal"] == true {
				delete(item, method)
				continue
			}
			// Where an operation is declared is for the backend's own readers.
			delete(op, "x-source")
			for _, tag := range operationTags(op) {
				used[tag] = true
			}
		}
		if len(item) == 0 {
			delete(paths, path)
		}
	}

	tags, ok := document["tags"].([]any)
	if ok {
		var kept []any
		for _, tag := range tags {
			entry, _ := tag.(map[string]any)
			name, _ := entry["name"].(string)
			if used[name] {
				kept = append(kept, tag)
			}
		}
		if len(kept) > 0 {
			document["tags"] = kept
		} else {
			delete(document, "tags")
		}
	}

	components, _ := document["components"].(map[string]any)
	schemas, _ := components["schemas"].(map[string]map[string]any)
	pruneSchemas(document, schemas, s.publicRoots)

	err := validateRefs(document, schemas)
	if err != nil {
		return nil, ez.Wrap(err)
	}

	return &Spec{
		Document:    document,
		Routes:      s.Routes,
		publicRoots: s.publicRoots,
	}, nil
}

// operationTags returns an operation's tags, whether the generator ([]string) or an
// Operation hook ([]any) set them.
func operationTags(op map[string]any) []string {
	switch typed := op["tags"].(type) {
	case []string:
		return typed
	case []any:
		var names []string
		for _, tag := range typed {
			name, ok := tag.(string)
			if ok {
				names = append(names, name)
			}
		}
		return names
	}

	return nil
}

// Run generates the document and writes it to cfg.OutDir: the public spec (Spec.Public) as
// openapi.json and openapi.yaml, the artifact SDKs and releases consume, and the full spec,
// internal operations included, as openapi.internal.json and openapi.internal.yaml.
func Run(cfg Config) error {
	spec, err := Generate(cfg)
	if err != nil {
		return ez.Wrap(err)
	}

	public, err := spec.Public()
	if err != nil {
		return ez.Wrap(err)
	}

	cfg, err = cfg.withDefaults()
	if err != nil {
		return ez.Wrap(err)
	}

	outDir := cfg.OutDir
	if !filepath.IsAbs(outDir) {
		outDir = filepath.Join(cfg.Root, outDir)
	}

	err = writeSpec(public, filepath.Join(outDir, "openapi"))
	if err != nil {
		return ez.Wrap(err)
	}

	err = writeSpec(spec, filepath.Join(outDir, "openapi.internal"))
	if err != nil {
		return ez.Wrap(err)
	}

	return nil
}

// writeSpec writes the spec as base.json and base.yaml.
func writeSpec(spec *Spec, base string) error {
	jsonBytes, err := spec.JSON()
	if err != nil {
		return ez.Wrap(err)
	}

	err = os.WriteFile(base+".json", jsonBytes, 0o644)
	if err != nil {
		return ez.Wrap(err)
	}

	yamlBytes, err := spec.YAML()
	if err != nil {
		return ez.Wrap(err)
	}

	err = os.WriteFile(base+".yaml", yamlBytes, 0o644)
	if err != nil {
		return ez.Wrap(err)
	}

	return nil
}

// pruneSchemas deletes every component schema that neither the paths, the shared responses
// and parameters, nor the roots reach.
func pruneSchemas(document map[string]any, schemas map[string]map[string]any, roots []any) {
	pending := map[string]bool{}
	collectRefs(document["paths"], pending)
	components, _ := document["components"].(map[string]any)
	collectRefs(components["responses"], pending)
	collectRefs(components["parameters"], pending)
	for _, root := range roots {
		collectRefs(root, pending)
	}

	reached := map[string]bool{}
	for len(pending) > 0 {
		var name string
		for candidate := range pending {
			name = candidate
			break
		}
		delete(pending, name)

		if reached[name] {
			continue
		}
		reached[name] = true

		component, ok := schemas[name]
		if !ok {
			continue
		}
		collectRefs(component, pending)
	}

	for name := range schemas {
		if !reached[name] {
			delete(schemas, name)
		}
	}
}

// validateRefs fails when any $ref of the document names no component.
func validateRefs(document map[string]any, schemas map[string]map[string]any) error {
	refs := map[string]bool{}
	collectRefs(document, refs)
	components, _ := document["components"].(map[string]any)

	var dangling []string
	for name := range refs {
		if !refResolves(name, schemas, components) {
			dangling = append(dangling, name)
		}
	}
	if len(dangling) == 0 {
		return nil
	}

	sort.Strings(dangling)
	return ez.New(ez.EINTERNAL, "unresolved $refs, spec not written:\n  "+strings.Join(dangling, "\n  "), nil)
}

// refResolves reports a collected $ref naming an existing component: a schema name, or a whole
// #/components/<section>/<name> reference for the other sections (responses, parameters).
func refResolves(name string, schemas map[string]map[string]any, components map[string]any) bool {
	_, ok := schemas[name]
	if ok {
		return true
	}

	rest, ok := strings.CutPrefix(name, "#/components/")
	if !ok {
		return false
	}
	section, entry, ok := strings.Cut(rest, "/")
	if !ok || section == "schemas" {
		return false
	}
	entries, _ := components[section].(map[string]any)
	_, ok = entries[entry]

	return ok
}

// collectRefs adds the component name of every $ref under node to refs. A $ref outside
// #/components/schemas/ is kept whole, so validateRefs reports it.
func collectRefs(node any, refs map[string]bool) {
	switch typed := node.(type) {
	case map[string]any:
		ref, ok := typed["$ref"].(string)
		if ok {
			refs[strings.TrimPrefix(ref, schemaRefPrefix)] = true
		}
		for _, value := range typed {
			collectRefs(value, refs)
		}
	case map[string]map[string]any:
		for _, value := range typed {
			collectRefs(value, refs)
		}
	case []any:
		for _, value := range typed {
			collectRefs(value, refs)
		}
	case []map[string]any:
		for _, value := range typed {
			collectRefs(value, refs)
		}
	}
}
