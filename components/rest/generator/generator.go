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

	if cfg.Schemas != nil {
		cfg.Schemas(builder.schemas)
	}
	stripRequestSideRequired(handlerSchemas, builder.schemas, modelsSchemaPrefix(cfg.RequiredPackagePrefix))

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
	for _, r := range routes {
		handler, ok := handlerSchemas[r.HandlerKey()]
		if !ok {
			continue
		}
		roots = append(roots, handler.Request, handler.Response)
	}
	pruneSchemas(document, builder.schemas, roots)

	err = validateRefs(document, builder.schemas)
	if err != nil {
		return nil, ez.Wrap(err)
	}

	return &Spec{
		Document: document,
		Routes:   routes,
	}, nil
}

// Run generates the document and writes it to cfg.OutDir as openapi.json and openapi.yaml.
func Run(cfg Config) error {
	spec, err := Generate(cfg)
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

	jsonBytes, err := spec.JSON()
	if err != nil {
		return ez.Wrap(err)
	}

	err = os.WriteFile(filepath.Join(outDir, "openapi.json"), jsonBytes, 0o644)
	if err != nil {
		return ez.Wrap(err)
	}

	yamlBytes, err := spec.YAML()
	if err != nil {
		return ez.Wrap(err)
	}

	err = os.WriteFile(filepath.Join(outDir, "openapi.yaml"), yamlBytes, 0o644)
	if err != nil {
		return ez.Wrap(err)
	}

	return nil
}

// modelsSchemaPrefix turns RequiredPackagePrefix ("/application/models/") into the component
// name prefix of those packages ("application_models_").
func modelsSchemaPrefix(packagePrefix string) string {
	trimmed := strings.Trim(packagePrefix, "/")
	if trimmed == "" {
		return ""
	}

	return strings.NewReplacer("/", "_", ".", "_", "-", "_").Replace(trimmed) + "_"
}

// pruneSchemas deletes every component schema that neither the paths nor the roots reach.
func pruneSchemas(document map[string]any, schemas map[string]map[string]any, roots []any) {
	pending := map[string]bool{}
	collectRefs(document["paths"], pending)
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

// validateRefs fails when any $ref of the document names no component schema.
func validateRefs(document map[string]any, schemas map[string]map[string]any) error {
	refs := map[string]bool{}
	collectRefs(document, refs)

	var dangling []string
	for name := range refs {
		_, ok := schemas[name]
		if !ok {
			dangling = append(dangling, name)
		}
	}
	if len(dangling) == 0 {
		return nil
	}

	sort.Strings(dangling)
	return ez.New(ez.EINTERNAL, "unresolved $refs, spec not written:\n  "+strings.Join(dangling, "\n  "), nil)
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
