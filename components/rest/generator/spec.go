package generator

import (
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/vanclief/ez"
)

const cursorRequestSchemaName = "github_com_vanclief_compose_drivers_databases_relational_postgres_pagination_CursorRequest"

func (g *generator) buildOpenAPISpec(routes []Route, facts handlerFacts, handlerSchemas map[string]handlerSchema) map[string]any {
	schemas := g.builder.schemas
	paths := map[string]any{}
	tagsSet := map[string]struct{}{}

	// Every non-HEAD operation's `default` response $refs these.
	registerErrorSchemas(schemas)

	for _, r := range routes {
		oasPath := toOpenAPIPath(r.Path)
		if g.skip(r.Method, oasPath) {
			continue
		}
		if r.Deprecated {
			continue
		}

		tag := g.tagForNamespace(r.Namespace)
		tagsSet[tag] = struct{}{}

		pathItemRaw, ok := paths[oasPath]
		if !ok {
			pathItemRaw = map[string]any{}
			paths[oasPath] = pathItemRaw
		}

		pathItem, ok := pathItemRaw.(map[string]any)
		if !ok {
			continue
		}

		methodKey := strings.ToLower(r.Method)

		handlerName := r.HandlerKey()
		handlerSchema := handlerSchemas[handlerName]
		params := buildPathParameters(oasPath, facts.pathParams[handlerName])
		params = append(params, buildPaginationQueryParameters(r.Method, handlerSchema.Request, schemas)...)
		responses := g.buildResponses(r, handlerSchema.Response)

		operation := map[string]any{
			"summary":     g.summaryForRoute(r, oasPath),
			"operationId": g.operationIDs[r.Method+" "+r.Path],
			"tags":        []string{tag},
			"responses":   responses,
			"x-source":    g.trimRepoPrefix(r.SourceFile) + ":" + strconv.Itoa(r.SourceLine),
		}

		requestBody := g.buildRequestBody(r, handlerSchema)
		if requestBody != nil {
			operation["requestBody"] = requestBody
		}

		if g.cfg.Public != nil && g.cfg.Public(r.Method, oasPath) {
			operation["security"] = []any{}
		}

		operation["parameters"] = params
		if g.cfg.Operation != nil {
			g.cfg.Operation(r, operation)
		}

		params, _ = operation["parameters"].([]any)
		params = append(params, derivedQueryParameters(facts.queryParams[handlerName], params)...)
		if len(params) > 0 {
			operation["parameters"] = params
		} else {
			delete(operation, "parameters")
		}

		pathItem[methodKey] = operation
	}

	components := map[string]any{}
	securitySchemes := map[string]any{}
	requirement := map[string]any{}
	for _, scheme := range g.cfg.Security {
		securitySchemes[scheme.Name] = map[string]any{
			"type": "apiKey",
			"in":   "header",
			"name": scheme.Header,
		}
		requirement[scheme.Name] = []any{}
	}
	if len(securitySchemes) > 0 {
		components["securitySchemes"] = securitySchemes
	}
	if len(schemas) > 0 {
		components["schemas"] = schemas
	}

	info := map[string]any{
		"title":   g.cfg.Info.Title,
		"version": g.cfg.Info.Version,
	}
	if g.cfg.Info.Description != "" {
		info["description"] = g.cfg.Info.Description
	}

	spec := map[string]any{
		"openapi":    "3.0.3",
		"info":       info,
		"paths":      paths,
		"components": components,
	}

	servers := g.servers()
	if len(servers) > 0 {
		spec["servers"] = servers
	}
	if len(requirement) > 0 {
		spec["security"] = []any{requirement}
	}

	tags := sortedTagList(tagsSet)
	if len(tags) > 0 {
		spec["tags"] = tags
	}

	return spec
}

func (g *generator) servers() []any {
	servers := make([]any, 0, len(g.cfg.Servers))
	for _, server := range g.cfg.Servers {
		entry := map[string]any{
			"url": server.URL,
		}
		if len(server.Variables) > 0 {
			variables := map[string]any{}
			for name, value := range server.Variables {
				variables[name] = map[string]any{
					"default": value,
				}
			}
			entry["variables"] = variables
		}
		servers = append(servers, entry)
	}

	return servers
}

func (g *generator) skip(method string, oasPath string) bool {
	if g.cfg.Skip == nil {
		return false
	}

	return g.cfg.Skip(method, oasPath)
}

func (g *generator) summaryForRoute(r Route, oasPath string) string {
	staticSummary, ok := g.cfg.PathSummaries[oasPath]
	if ok {
		return staticSummary
	}

	if r.Handler != "" {
		summary := handlerSummary(r.Handler, r.Namespace, r.Method, g.cfg.SummaryVerbs)
		if summary != "" {
			return summary
		}
	}

	return r.Method + " " + oasPath
}

func handlerSummary(name string, namespace string, method string, verbs []string) string {
	if name == "" {
		return ""
	}

	words := splitCamelWords(name)
	if len(words) == 0 {
		return ""
	}
	if len(words) == 1 {
		namespaceWords := splitCamelWords(namespace)
		for i := range namespaceWords {
			namespaceWords[i] = strings.ToLower(namespaceWords[i])
		}
		words = append(words, namespaceWords...)
	}

	first := words[0]
	if method == "GET" {
		if !containsString(verbs, first) {
			return "Get " + strings.Join(words, " ")
		}
	}
	if method == "POST" {
		if !containsString(verbs, first) {
			return "Create " + strings.Join(words, " ")
		}
	}

	return strings.Join(words, " ")
}

func splitCamelWords(name string) []string {
	if name == "" {
		return nil
	}

	runes := []rune(name)
	if len(runes) == 0 {
		return nil
	}

	var words []string
	start := 0

	for i := 1; i < len(runes); i++ {
		prev := runes[i-1]
		curr := runes[i]
		var next rune
		if i+1 < len(runes) {
			next = runes[i+1]
		}

		boundary := false
		if isLower(prev) && isUpper(curr) {
			boundary = true
		}
		if isUpper(prev) && isUpper(curr) && next != 0 && isLower(next) {
			boundary = true
		}
		if isDigit(curr) && !isDigit(prev) {
			boundary = true
		}
		if !isDigit(curr) && isDigit(prev) {
			boundary = true
		}

		if boundary {
			if start < i {
				words = append(words, string(runes[start:i]))
			}
			start = i
		}
	}

	if start < len(runes) {
		words = append(words, string(runes[start:]))
	}

	return words
}

func isUpper(r rune) bool {
	return r >= 'A' && r <= 'Z'
}

func isLower(r rune) bool {
	return r >= 'a' && r <= 'z'
}

func isDigit(r rune) bool {
	return r >= '0' && r <= '9'
}

func toOpenAPIPath(echoPath string) string {
	if strings.HasSuffix(echoPath, "/*") {
		prefix := strings.TrimSuffix(echoPath, "/*")
		echoPath = prefix + "/{path}"
	}

	return rePathParam.ReplaceAllString(echoPath, `{$1}`)
}

func buildPathParameters(oasPath string, paramSchemas map[string]map[string]any) []any {
	matches := reOASParam.FindAllStringSubmatch(oasPath, -1)

	seen := map[string]struct{}{}
	var params []any

	for _, m := range matches {
		if len(m) < 2 {
			continue
		}

		name := m[1]
		_, ok := seen[name]
		if ok {
			continue
		}
		seen[name] = struct{}{}

		params = append(params, map[string]any{
			"name":     name,
			"in":       "path",
			"required": true,
			"schema":   pathParameterSchema(name, paramSchemas),
		})
	}

	return params
}

// buildPaginationQueryParameters declares limit and cursor on GET operations whose request
// embeds compose's pagination.CursorRequest or carries both fields itself.
func buildPaginationQueryParameters(method string, requestSchema map[string]any, schemas map[string]map[string]any) []any {
	if method != "GET" {
		return nil
	}

	hasCursorRequest := schemaUsesComponent(requestSchema, cursorRequestSchemaName, schemas, map[string]bool{})
	hasCursorProperty := schemaHasProperty(requestSchema, "cursor", schemas, map[string]bool{})
	hasLimitProperty := schemaHasProperty(requestSchema, "limit", schemas, map[string]bool{})
	if !hasCursorRequest && !(hasCursorProperty && hasLimitProperty) {
		return nil
	}

	return []any{
		map[string]any{
			"name":     "limit",
			"in":       "query",
			"required": false,
			"schema": map[string]any{
				"type":   "integer",
				"format": "int32",
			},
		},
		map[string]any{
			"name":     "cursor",
			"in":       "query",
			"required": false,
			"schema": map[string]any{
				"type": "string",
			},
		},
	}
}

// derivedQueryParameters emits the query parameters harvested from the handler's own query
// reads. Parameters already declared for the operation (pagination, the Operation hook's
// refinements such as required flags or inline enums) win by name; the derived pass fills in
// the rest, sorted for a stable document.
func derivedQueryParameters(schemas map[string]map[string]any, existing []any) []any {
	if len(schemas) == 0 {
		return nil
	}

	declared := map[string]bool{}
	for _, param := range existing {
		paramMap, ok := param.(map[string]any)
		if !ok {
			continue
		}
		if paramMap["in"] != "query" {
			continue
		}
		name, ok := paramMap["name"].(string)
		if ok {
			declared[name] = true
		}
	}

	names := make([]string, 0, len(schemas))
	for name := range schemas {
		if declared[name] {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)

	params := make([]any, 0, len(names))
	for _, name := range names {
		params = append(params, map[string]any{
			"name":     name,
			"in":       "query",
			"required": false,
			"schema":   schemas[name],
		})
	}

	return params
}

func schemaUsesComponent(schema map[string]any, componentName string, schemas map[string]map[string]any, visited map[string]bool) bool {
	if len(schema) == 0 {
		return false
	}

	refName := componentNameFromSchema(schema)
	if refName != "" {
		if refName == componentName {
			return true
		}

		if visited[refName] {
			return false
		}
		visited[refName] = true

		resolved, ok := schemas[refName]
		if !ok {
			return false
		}

		return schemaUsesComponent(resolved, componentName, schemas, visited)
	}

	allOfRaw, ok := schema["allOf"]
	if !ok {
		return false
	}

	allOf, ok := allOfRaw.([]any)
	if !ok {
		return false
	}

	for i := range allOf {
		part, ok := allOf[i].(map[string]any)
		if !ok {
			continue
		}

		if schemaUsesComponent(part, componentName, schemas, visited) {
			return true
		}
	}

	return false
}

func schemaHasProperty(schema map[string]any, propertyName string, schemas map[string]map[string]any, visited map[string]bool) bool {
	if len(schema) == 0 {
		return false
	}

	refName := componentNameFromSchema(schema)
	if refName != "" {
		if visited[refName] {
			return false
		}
		visited[refName] = true

		resolved, ok := schemas[refName]
		if !ok {
			return false
		}

		return schemaHasProperty(resolved, propertyName, schemas, visited)
	}

	propertiesRaw, ok := schema["properties"]
	if ok {
		properties, ok := propertiesRaw.(map[string]any)
		if ok {
			_, ok = properties[propertyName]
			if ok {
				return true
			}
		}
	}

	allOfRaw, ok := schema["allOf"]
	if !ok {
		return false
	}

	allOf, ok := allOfRaw.([]any)
	if !ok {
		return false
	}

	for i := range allOf {
		part, ok := allOf[i].(map[string]any)
		if !ok {
			continue
		}

		if schemaHasProperty(part, propertyName, schemas, visited) {
			return true
		}
	}

	return false
}

func componentNameFromSchema(schema map[string]any) string {
	refRaw, ok := schema["$ref"]
	if !ok {
		return ""
	}

	ref, ok := refRaw.(string)
	if !ok {
		return ""
	}

	if !strings.HasPrefix(ref, schemaRefPrefix) {
		return ""
	}

	return strings.TrimPrefix(ref, schemaRefPrefix)
}

func pathParameterSchema(name string, paramSchemas map[string]map[string]any) map[string]any {
	if paramSchemas != nil {
		schema, ok := paramSchemas[name]
		if ok && len(schema) > 0 {
			return schema
		}
	}

	schema := map[string]any{
		"type": "string",
	}

	if isNumericIDParam(name) {
		schema["type"] = "integer"
		schema["format"] = "int64"
	}

	return schema
}

func isNumericIDParam(name string) bool {
	if name == "id" {
		return true
	}
	if strings.HasSuffix(name, "_id") {
		return true
	}
	return false
}

func (g *generator) buildRequestBody(r Route, handler handlerSchema) map[string]any {
	switch r.Method {
	case "POST", "PUT", "PATCH":
	case "DELETE":
		if !r.BindsBody {
			return nil
		}
	default:
		return nil
	}

	requestSchema := g.bodySchema(handler)

	contentType := "application/json"
	schema := map[string]any{
		"type": "object",
	}
	if requestSchema != nil {
		schema = requestSchema
	}

	if g.cfg.MultipartPath != nil && g.cfg.MultipartPath(toOpenAPIPath(r.Path)) {
		contentType = "multipart/form-data"
		schema = map[string]any{
			"type": "object",
			"properties": map[string]any{
				"files": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type":   "string",
						"format": "binary",
					},
				},
			},
		}
		if requestSchema != nil {
			schema = map[string]any{
				"allOf": []any{requestSchema, schema},
			}
		}
	}

	return map[string]any{
		"required": false,
		"content": map[string]any{
			contentType: map[string]any{
				"schema": schema,
			},
		},
	}
}

// bodySchema is the handler's request schema minus the fields its handler fills from path
// parameters. Those leave a component untouched: the trimmed copy is inlined.
func (g *generator) bodySchema(handler handlerSchema) map[string]any {
	if len(handler.PathFields) == 0 {
		return handler.Request
	}

	schema := handler.Request
	name := componentNameFromSchema(schema)
	if name != "" {
		component, ok := g.builder.schemas[name]
		if !ok {
			return handler.Request
		}
		schema = component
	}

	trimmed := cloneSchema(schema)
	for _, member := range schemaMembers(trimmed) {
		properties, _ := member["properties"].(map[string]any)
		for _, field := range handler.PathFields {
			delete(properties, field)
		}

		var kept []string
		for _, required := range requiredNames(member["required"]) {
			if !containsString(handler.PathFields, required) {
				kept = append(kept, required)
			}
		}
		if len(kept) == 0 {
			delete(member, "required")
			continue
		}
		member["required"] = kept
	}

	return trimmed
}

func (g *generator) buildResponses(r Route, responseSchema map[string]any) map[string]any {
	if r.Method == "HEAD" {
		return map[string]any{
			"200": map[string]any{
				"description": "OK",
			},
		}
	}

	contentTypes := g.responseContentTypes(r)
	content := map[string]any{}

	for _, contentType := range contentTypes {
		schema := responseSchemaForContentType(contentType, responseSchema)
		content[contentType] = map[string]any{
			"schema": schema,
		}
	}

	return map[string]any{
		"200": map[string]any{
			"description": "OK",
			"content":     content,
		},
		"default": errorResponse(),
	}
}

func (g *generator) responseContentTypes(r Route) []string {
	if g.cfg.ContentTypes != nil {
		contentTypes := g.cfg.ContentTypes(r)
		if len(contentTypes) > 0 {
			return contentTypes
		}
	}

	if r.Negotiates {
		return append([]string(nil), g.cfg.NegotiatedContentTypes...)
	}

	return []string{"application/json"}
}

// Schema names for the error envelope every handler returns. These mirror compose's
// handler.ErrorResponse / handler.StandardError. The short names keep generated client types
// readable; they cannot collide with reflected schemas because schemaName always prefixes
// those with an import path.
const (
	errorResponseSchemaName = "ErrorResponse"
	standardErrorSchemaName = "StandardError"
)

// errorResponse is the `default` response every non-HEAD operation carries.
func errorResponse() map[string]any {
	return map[string]any{
		"description": "Error",
		"content": map[string]any{
			"application/json": map[string]any{
				"schema": map[string]any{
					"$ref": schemaRefPrefix + errorResponseSchemaName,
				},
			},
		},
	}
}

// registerErrorSchemas adds the error envelope to components.schemas.
//
// BaseHandler.ManageError writes `ErrorResponse{Error: StandardError{...}}`. None of the
// fields carry `omitempty`, so all three are always serialized and are therefore required.
// Declaring it here gives every operation a typed error instead of a bare `type: object`,
// which generators emit as `unknown`.
func registerErrorSchemas(schemas map[string]map[string]any) {
	schemas[standardErrorSchemaName] = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"code": map[string]any{
				"type":        "string",
				"description": "Machine-readable error code, e.g. \"not_authenticated\".",
			},
			"message": map[string]any{
				"type":        "string",
				"description": "Human-readable message, translated when the request carries a locale.",
			},
			"request_id": map[string]any{
				"type":        "string",
				"description": "Correlates this response with the backend logs.",
			},
		},
		"required": []any{"code", "message", "request_id"},
	}

	schemas[errorResponseSchemaName] = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"error": map[string]any{
				"$ref": schemaRefPrefix + standardErrorSchemaName,
			},
		},
		"required": []any{"error"},
	}
}

func responseSchemaForContentType(contentType string, responseSchema map[string]any) map[string]any {
	if contentType == "application/json" && responseSchema != nil {
		return responseSchema
	}
	return defaultResponseSchema(contentType)
}

func defaultResponseSchema(contentType string) map[string]any {
	if contentType == "application/json" {
		return map[string]any{
			"type": "object",
		}
	}
	return map[string]any{
		"type":   "string",
		"format": "binary",
	}
}

// operationIDForRoute is "namespace.method", or just "method" for a flat route with no
// namespace.
func operationIDForRoute(r Route) string {
	if r.Handler == "" {
		return ""
	}
	if r.Namespace == "" {
		if r.flat {
			return lowerFirst(r.Handler)
		}
		return ""
	}

	return r.Namespace + "." + lowerFirst(r.Handler)
}

// assignOperationIDs gives every documented route its operationId, keyed by "METHOD path".
// One handler routed on one path under several methods (PATCH and PUT) gets the method
// appended to each id ("projects.updateProjectPatch"); any other repeated id is an error.
func (g *generator) assignOperationIDs(routes []Route) (map[string]string, error) {
	var live []Route
	byID := map[string][]Route{}

	for _, r := range routes {
		oasPath := toOpenAPIPath(r.Path)
		if g.skip(r.Method, oasPath) || r.Deprecated {
			continue
		}

		id := operationIDForRoute(r)
		if strings.TrimSpace(id) == "" {
			return nil, ez.New(ez.EINVALID, "missing handler operationId for "+r.Method+" "+oasPath, nil)
		}

		previous := byID[id]
		if len(previous) > 0 && previous[0].Path != r.Path {
			message := "duplicate operationId " + strconv.Quote(id) + " for " +
				previous[0].Method + " " + toOpenAPIPath(previous[0].Path) + " and " + r.Method + " " + oasPath
			return nil, ez.New(ez.EINVALID, message, nil)
		}
		byID[id] = append(previous, r)
		live = append(live, r)
	}

	ids := map[string]string{}
	seen := map[string]Route{}
	for _, r := range live {
		id := operationIDForRoute(r)
		if len(byID[id]) > 1 {
			method := strings.ToLower(r.Method)
			id = id + strings.ToUpper(method[:1]) + method[1:]
		}

		previous, ok := seen[id]
		if ok {
			message := "duplicate operationId " + strconv.Quote(id) + " for " +
				previous.Method + " " + toOpenAPIPath(previous.Path) + " and " + r.Method + " " + toOpenAPIPath(r.Path)
			return nil, ez.New(ez.EINVALID, message, nil)
		}
		seen[id] = r
		ids[r.Method+" "+r.Path] = id
	}

	return ids, nil
}

// tagForNamespace names a namespace's tag: TagNames, else its words capitalized
// ("accessControl" is "Access Control", "code_snapshots" is "Code snapshots").
func (g *generator) tagForNamespace(namespace string) string {
	if namespace == "" {
		return "API"
	}
	tag, ok := g.cfg.TagNames[namespace]
	if ok {
		return tag
	}

	spaced := strings.NewReplacer("_", " ", "-", " ").Replace(namespace)
	words := splitCamelWords(spaced)
	words[0] = strings.ToUpper(words[0][:1]) + words[0][1:]
	return strings.Join(words, " ")
}

func sortedTagList(tagsSet map[string]struct{}) []any {
	if len(tagsSet) == 0 {
		return nil
	}

	names := make([]string, 0, len(tagsSet))
	for name := range tagsSet {
		names = append(names, name)
	}

	sort.Strings(names)

	tags := make([]any, 0, len(names))
	for _, name := range names {
		tags = append(tags, map[string]any{
			"name": name,
		})
	}

	return tags
}

func (g *generator) trimRepoPrefix(path string) string {
	root := filepath.ToSlash(g.cfg.Root)
	path = filepath.ToSlash(path)

	prefix := root + "/"
	if strings.HasPrefix(path, prefix) {
		return strings.TrimPrefix(path, prefix)
	}
	return path
}
