package generator

import (
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/vanclief/ez"
)

// paginationImportPath is compose's pagination package, whose CursorRequest and CursorResponse
// the generator describes itself.
const paginationImportPath = "github.com/vanclief/compose/drivers/databases/relational/postgres/pagination"

func (g *generator) buildOpenAPISpec(routes []Route, facts handlerFacts, handlerSchemas map[string]handlerSchema) map[string]any {
	schemas := g.builder.schemas
	paths := map[string]any{}
	// tagsSet maps each operation tag to its namespace.
	tagsSet := map[string]string{}

	// Every non-HEAD operation's `default` response $refs these.
	registerErrorSchemas(schemas)

	limitDefault, hasLimitDefault := g.sharedLimitDefault(routes, facts, handlerSchemas)
	sharedParameters := map[string]any{}

	for _, r := range routes {
		oasPath := toOpenAPIPath(r.Path)
		if g.skip(r.Method, oasPath) {
			continue
		}

		tag := g.tagForNamespace(r.Namespace)
		tagsSet[tag] = r.Namespace

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
		params = append(params, g.paginationParameters(r.Method, handlerSchema.Request, facts.queryParams[handlerName], limitDefault, hasLimitDefault, sharedParameters)...)
		responses := g.buildResponses(r, oasPath, handlerSchema.Response, facts.responses[handlerName])

		operation := map[string]any{
			"summary":     g.summaryForRoute(r, oasPath),
			"operationId": g.operationIDs[r.Method+" "+r.Path],
			"tags":        []string{tag},
			"responses":   responses,
			"x-source":    g.trimRepoPrefix(r.SourceFile) + ":" + strconv.Itoa(r.SourceLine),
		}

		description := g.operationDescription(facts.docs[handlerName], handlerSchema.ModuleDoc)
		if description != "" {
			operation["description"] = description
		}

		requestBody := g.buildRequestBody(r, handlerSchema)
		if requestBody != nil {
			operation["requestBody"] = requestBody
		}

		if g.cfg.Public != nil && g.cfg.Public(r.Method, oasPath) {
			operation["security"] = []any{}
		}
		if g.internal(r.Method, oasPath) {
			operation["x-internal"] = true
		}
		// A deprecated route is documented for the clients still calling it, in the internal
		// spec only: the public one, which SDKs are generated from, leaves it out.
		if r.Deprecated {
			operation["deprecated"] = true
			operation["x-internal"] = true
		}

		operation["parameters"] = params
		if g.cfg.Operation != nil {
			g.cfg.Operation(r, operation)
		}

		params, _ = operation["parameters"].([]any)
		params = append(params, derivedQueryParameters(facts.queryParams[handlerName], params, sharedParameters)...)
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
		securityScheme := map[string]any{
			"type": "apiKey",
			"in":   "header",
			"name": scheme.Header,
		}
		if scheme.Description != "" {
			securityScheme["description"] = scheme.Description
		}
		securitySchemes[scheme.Name] = securityScheme
		requirement[scheme.Name] = []any{}
	}
	if len(securitySchemes) > 0 {
		components["securitySchemes"] = securitySchemes
	}
	if len(schemas) > 0 {
		components["schemas"] = schemas
	}
	components["responses"] = errorResponses()
	if len(sharedParameters) > 0 {
		components["parameters"] = sharedParameters
	}

	info := map[string]any{
		"title":   g.cfg.Info.Title,
		"version": g.cfg.Info.Version,
	}
	if g.cfg.Info.Description != "" {
		info["description"] = g.cfg.Info.Description
	}
	if g.cfg.Info.TermsOfService != "" {
		info["termsOfService"] = g.cfg.Info.TermsOfService
	}
	contact := g.cfg.Info.Contact
	if contact != nil {
		info["contact"] = nonEmptyFields(map[string]string{"name": contact.Name, "url": contact.URL, "email": contact.Email})
	}
	license := g.cfg.Info.License
	if license != nil {
		info["license"] = nonEmptyFields(map[string]string{"name": license.Name, "url": license.URL})
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
	externalDocs := g.cfg.ExternalDocs
	if externalDocs != nil {
		spec["externalDocs"] = nonEmptyFields(map[string]string{"description": externalDocs.Description, "url": externalDocs.URL})
	}
	if len(requirement) > 0 {
		spec["security"] = []any{requirement}
	}

	tags := g.sortedTagList(tagsSet, facts.namespaceDocs)
	if len(tags) > 0 {
		spec["tags"] = tags
	}

	return spec
}

// nonEmptyFields is an object of the fields that are set.
func nonEmptyFields(fields map[string]string) map[string]any {
	object := map[string]any{}
	for key, value := range fields {
		if value != "" {
			object[key] = value
		}
	}

	return object
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

func (g *generator) internal(method string, oasPath string) bool {
	if g.cfg.Internal == nil {
		return false
	}

	return g.cfg.Internal(method, oasPath)
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

const (
	limitParameter      = "limit"
	cursorParameter     = "cursor"
	parameterRefPrefix  = "#/components/parameters/"
	limitParameterName  = "Limit"
	cursorParameterName = "Cursor"
)

// paginates reports a GET operation whose request embeds compose's pagination.CursorRequest or
// carries both its fields itself.
func (g *generator) paginates(method string, requestSchema map[string]any) bool {
	if method != "GET" {
		return false
	}

	schemas := g.builder.schemas
	cursorRequest := g.builder.componentOf(paginationImportPath, "CursorRequest")
	hasCursorRequest := cursorRequest != "" && schemaUsesComponent(requestSchema, cursorRequest, schemas, map[string]bool{})
	hasCursorProperty := schemaHasProperty(requestSchema, cursorParameter, schemas, map[string]bool{})
	hasLimitProperty := schemaHasProperty(requestSchema, limitParameter, schemas, map[string]bool{})

	return hasCursorRequest || (hasCursorProperty && hasLimitProperty)
}

// sharedLimitDefault is the default of the shared Limit parameter: the GetListLimit default
// most paginated handlers use, the smallest on a tie.
func (g *generator) sharedLimitDefault(routes []Route, facts handlerFacts, handlerSchemas map[string]handlerSchema) (int, bool) {
	counts := map[int]int{}
	for _, r := range routes {
		if g.skip(r.Method, toOpenAPIPath(r.Path)) {
			continue
		}
		key := r.HandlerKey()
		if !g.paginates(r.Method, handlerSchemas[key].Request) {
			continue
		}
		value, ok := facts.queryParams[key][limitParameter]["default"].(int)
		if ok {
			counts[value]++
		}
	}

	best := 0
	found := false
	for value, count := range counts {
		if !found || count > counts[best] || (count == counts[best] && value < best) {
			best = value
			found = true
		}
	}

	return best, found
}

// paginationParameters declares limit and cursor on a paginated operation, as references to
// the shared Limit and Cursor parameters. A handler whose GetListLimit default differs from the
// shared one, or that reads no limit itself, declares its own limit.
func (g *generator) paginationParameters(method string, requestSchema map[string]any, queryParams map[string]map[string]any, limitDefault int, hasLimitDefault bool, shared map[string]any) []any {
	if !g.paginates(method, requestSchema) {
		return nil
	}

	shared[cursorParameterName] = map[string]any{
		"name":     cursorParameter,
		"in":       "query",
		"required": false,
		"schema": map[string]any{
			"type": "string",
		},
	}
	cursor := map[string]any{"$ref": parameterRefPrefix + cursorParameterName}

	sharedSchema := g.limitSchema()
	if hasLimitDefault {
		sharedSchema["default"] = limitDefault
	}

	read, readsLimit := queryParams[limitParameter]
	value, hasDefault := read["default"].(int)
	if readsLimit && hasDefault == hasLimitDefault && value == limitDefault {
		shared[limitParameterName] = map[string]any{
			"name":     limitParameter,
			"in":       "query",
			"required": false,
			"schema":   sharedSchema,
		}
		return []any{map[string]any{"$ref": parameterRefPrefix + limitParameterName}, cursor}
	}

	schema := g.limitSchema()
	if hasDefault {
		schema["default"] = value
	}
	limit := map[string]any{
		"name":     limitParameter,
		"in":       "query",
		"required": false,
		"schema":   schema,
	}

	return []any{limit, cursor}
}

// limitSchema is a page size: at least one, at most MaxPageSize.
func (g *generator) limitSchema() map[string]any {
	schema := map[string]any{
		"type":    "integer",
		"format":  "int32",
		"minimum": 1,
	}
	if g.cfg.MaxPageSize > 0 {
		schema["maximum"] = g.cfg.MaxPageSize
	}

	return schema
}

// derivedQueryParameters emits the query parameters harvested from the handler's own query
// reads. Parameters already declared for the operation (pagination, the Operation hook's
// refinements such as required flags or inline enums) win by name; the derived pass fills in
// the rest, sorted for a stable document.
func derivedQueryParameters(schemas map[string]map[string]any, existing []any, shared map[string]any) []any {
	if len(schemas) == 0 {
		return nil
	}

	declared := map[string]bool{}
	for _, param := range existing {
		paramMap, ok := param.(map[string]any)
		if !ok {
			continue
		}
		ref, isRef := paramMap["$ref"].(string)
		if isRef {
			paramMap, _ = shared[strings.TrimPrefix(ref, parameterRefPrefix)].(map[string]any)
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

	// A multipart form's parts are the request's fields minus the path-filled ones (bodySchema),
	// its multipart.FileHeader fields being binary file parts. A request without one gets the
	// default "files" part; the RequestSchema hook names other parts.
	if g.cfg.MultipartPath != nil && g.cfg.MultipartPath(toOpenAPIPath(r.Path)) {
		contentType = "multipart/form-data"
		if !hasFilePart(requestSchema, g.builder.schemas, map[string]bool{}) {
			files := map[string]any{
				"type": "object",
				"properties": map[string]any{
					"files": map[string]any{
						"type":  "array",
						"items": binaryPart(),
					},
				},
			}
			schema = files
			if requestSchema != nil {
				schema = map[string]any{
					"allOf": []any{requestSchema, files},
				}
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
// parameters. Those leave a component untouched: the trimmed copy is inlined (unless
// stripPathFields could trim the component itself).
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
	removeProperties(trimmed, handler.PathFields)

	return trimmed
}

// removeProperties deletes fields from a schema's properties and required lists.
func removeProperties(schema map[string]any, fields []string) {
	for _, member := range schemaMembers(schema) {
		properties, _ := member["properties"].(map[string]any)
		for _, field := range fields {
			delete(properties, field)
		}

		var kept []string
		for _, required := range requiredNames(member["required"]) {
			if !containsString(fields, required) {
				kept = append(kept, required)
			}
		}
		if len(kept) == 0 {
			delete(member, "required")
			continue
		}
		member["required"] = kept
	}
}

// stripPathFields leaves the path-filled fields out of a request component itself when every
// handler taking it fills the same fields from the path and nothing else references it: those
// fields never travel in a body, and the operations keep their $ref instead of an inline copy.
func (g *generator) stripPathFields(handlerSchemas map[string]handlerSchema) {
	schemas := g.builder.schemas

	users := map[string][]string{}
	for _, key := range sortedKeys(handlerSchemas) {
		name := componentNameFromSchema(handlerSchemas[key].Request)
		if name != "" {
			users[name] = append(users[name], key)
		}
	}

	referenced := map[string]bool{}
	for _, schema := range schemas {
		collectRefs(schema, referenced)
	}
	for _, handler := range handlerSchemas {
		collectRefs(handler.Response, referenced)
	}

	for _, name := range sortedKeys(users) {
		keys := users[name]
		schema, ok := schemas[name]
		fields := sortedStrings(handlerSchemas[keys[0]].PathFields)
		if !ok || referenced[name] || len(fields) == 0 {
			continue
		}

		same := true
		for _, key := range keys[1:] {
			if !reflect.DeepEqual(sortedStrings(handlerSchemas[key].PathFields), fields) {
				same = false
				break
			}
		}
		if !same {
			continue
		}

		removeProperties(schema, fields)
		for _, key := range keys {
			handler := handlerSchemas[key]
			handler.PathFields = nil
			handlerSchemas[key] = handler
		}
	}
}

func sortedStrings(values []string) []string {
	sorted := append([]string(nil), values...)
	sort.Strings(sorted)

	return sorted
}

func binaryPart() map[string]any {
	return map[string]any{
		"type":   "string",
		"format": "binary",
	}
}

// hasFilePart reports a form schema with a binary property, or an array of them.
func hasFilePart(schema map[string]any, schemas map[string]map[string]any, visited map[string]bool) bool {
	if schema == nil {
		return false
	}

	name := componentNameFromSchema(schema)
	if name != "" {
		if visited[name] {
			return false
		}
		visited[name] = true
		return hasFilePart(schemas[name], schemas, visited)
	}

	properties, _ := schema["properties"].(map[string]any)
	for _, raw := range properties {
		property, _ := raw.(map[string]any)
		items, isArray := property["items"].(map[string]any)
		if isArray {
			property = items
		}
		if property["type"] == "string" && property["format"] == "binary" {
			return true
		}
	}

	allOf, _ := schema["allOf"].([]any)
	for _, raw := range allOf {
		member, _ := raw.(map[string]any)
		if hasFilePart(member, schemas, visited) {
			return true
		}
	}

	return false
}

func (g *generator) buildResponses(r Route, oasPath string, responseSchema map[string]any, served handlerResponse) map[string]any {
	responses := map[string]any{}
	for _, status := range g.errorStatuses(r, oasPath) {
		if r.Method == "HEAD" {
			// A HEAD response has no body: the status and its meaning only.
			responses[status.code] = map[string]any{
				"description": status.description,
			}
			continue
		}
		responses[status.code] = map[string]any{
			"$ref": responseRefPrefix + status.name,
		}
	}

	if r.Method == "HEAD" {
		responses["200"] = map[string]any{
			"description": "OK",
		}
		return responses
	}

	responses["200"] = map[string]any{
		"description": "OK",
		"content":     g.responseContent(r, responseSchema, served),
	}
	responses["default"] = errorResponse()

	return responses
}

const responseRefPrefix = "#/components/responses/"

// errorStatus is an error status BaseHandler.ManageError answers with, mapped from the ez code
// by ez.ErrorToHTTPStatus.
type errorStatus struct {
	code        string
	name        string
	description string
}

var (
	badRequestStatus      = errorStatus{"400", "BadRequest", "Invalid request: a malformed body or parameter, or a failed validation (code " + ez.EINVALID + ")."}
	unauthorizedStatus    = errorStatus{"401", "Unauthorized", "Missing or invalid credentials (code " + ez.ENOTAUTHENTICATED + ")."}
	forbiddenStatus       = errorStatus{"403", "Forbidden", "The credentials do not allow this operation (code " + ez.ENOTAUTHORIZED + ")."}
	notFoundStatus        = errorStatus{"404", "NotFound", "The resource does not exist (code " + ez.ENOTFOUND + ")."}
	tooManyRequestsStatus = errorStatus{"429", "TooManyRequests", "A rate limit or quota is exhausted (code " + ez.ERESOURCEEXHAUSTED + ")."}
	internalErrorStatus   = errorStatus{"500", "InternalError", "An unexpected server failure (code " + ez.EINTERNAL + ")."}
)

// errorStatuses are the error responses an operation declares besides `default`: 400, 429 and
// 500 on every operation, 401 and 403 on the ones needing authentication, 404 on a path with
// a parameter.
func (g *generator) errorStatuses(r Route, oasPath string) []errorStatus {
	statuses := []errorStatus{badRequestStatus}
	public := g.cfg.Public != nil && g.cfg.Public(r.Method, oasPath)
	if len(g.cfg.Security) > 0 && !public {
		statuses = append(statuses, unauthorizedStatus, forbiddenStatus)
	}
	if strings.Contains(oasPath, "{") {
		statuses = append(statuses, notFoundStatus)
	}

	return append(statuses, tooManyRequestsStatus, internalErrorStatus)
}

// errorResponses are the components/responses the operations reference, each the error
// envelope.
func errorResponses() map[string]any {
	responses := map[string]any{}
	for _, status := range []errorStatus{badRequestStatus, unauthorizedStatus, forbiddenStatus, notFoundStatus, tooManyRequestsStatus, internalErrorStatus} {
		responses[status.name] = map[string]any{
			"description": status.description,
			"content": map[string]any{
				"application/json": map[string]any{
					"schema": map[string]any{
						"$ref": schemaRefPrefix + errorResponseSchemaName,
					},
				},
			},
		}
	}

	return responses
}

// responseContent is the 200 response's content. The compose calls of the handler pin it:
// JSONResponse and BindedJSONResponse serve the module result as application/json,
// BindedXMLResponse as application/xml, and BlobResponse its bytes under a constant content
// type. Only what they leave open, a BlobResponse with a computed content type or a handler
// without a compose call, comes from the ContentTypes hook, else NegotiatedContentTypes for a
// negotiating handler and application/json otherwise.
func (g *generator) responseContent(r Route, responseSchema map[string]any, served handlerResponse) map[string]any {
	content := map[string]any{}
	add := func(contentType string, schema map[string]any) {
		_, exists := content[contentType]
		if !exists {
			content[contentType] = map[string]any{
				"schema": schema,
			}
		}
	}

	for _, contentType := range served.encoded {
		add(contentType, encodedResponseSchema(responseSchema))
	}
	for _, contentType := range served.blobs {
		add(contentType, blobSchema(contentType))
	}
	if served.known() && !served.open {
		return content
	}

	for _, contentType := range g.openContentTypes(r, served) {
		if served.open {
			add(contentType, blobSchema(contentType))
			continue
		}
		add(contentType, responseSchemaForContentType(contentType, responseSchema))
	}

	return content
}

func (g *generator) openContentTypes(r Route, served handlerResponse) []string {
	if g.cfg.ContentTypes != nil {
		contentTypes := g.cfg.ContentTypes(r)
		if len(contentTypes) > 0 {
			return contentTypes
		}
	}

	if r.Negotiates || served.open {
		return append([]string(nil), g.cfg.NegotiatedContentTypes...)
	}

	return []string{"application/json"}
}

// encodedResponseSchema is the schema of a module result a compose call encodes. A missing
// result is an object of unknown shape.
func encodedResponseSchema(responseSchema map[string]any) map[string]any {
	if responseSchema == nil {
		return map[string]any{
			"type": "object",
		}
	}

	return responseSchema
}

// blobSchema is the schema of raw bytes served under contentType: any JSON for a JSON type
// (the bytes are a JSON document, not base64), a string for text, binary otherwise.
func blobSchema(contentType string) map[string]any {
	if contentType == "application/json" || strings.HasSuffix(contentType, "+json") {
		return map[string]any{}
	}
	if strings.HasPrefix(contentType, "text/") {
		return map[string]any{
			"type": "string",
		}
	}

	return map[string]any{
		"type":   "string",
		"format": "binary",
	}
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
				"description": "Machine-readable error code, the ez code of the error.",
				"enum":        errorCodes(),
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

// errorCodes are the ez codes ManageError writes in StandardError.code.
func errorCodes() []any {
	return []any{
		ez.ECONFLICT,
		ez.EINTERNAL,
		ez.EINVALID,
		ez.ENOTFOUND,
		ez.ENOTAUTHORIZED,
		ez.ENOTAUTHENTICATED,
		ez.ERESOURCEEXHAUSTED,
		ez.ENOTIMPLEMENTED,
		ez.EUNAVAILABLE,
		ez.ETIMEOUT,
		ez.ECANCELED,
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
// appended to each id ("projects.updateProjectPatch"); any other repeated id is an error. A
// deprecated route whose id a live route (or another deprecated one) already has gets
// DeprecatedOperationSuffix ("platform.listTutorialsDeprecated"), then the method too.
func (g *generator) assignOperationIDs(routes []Route) (map[string]string, error) {
	var live []Route
	var deprecated []Route
	byID := map[string][]Route{}

	for _, r := range routes {
		oasPath := toOpenAPIPath(r.Path)
		if g.skip(r.Method, oasPath) {
			continue
		}
		if r.Deprecated {
			if strings.TrimSpace(operationIDForRoute(r)) == "" {
				return nil, ez.New(ez.EINVALID, "missing handler operationId for "+r.Method+" "+oasPath, nil)
			}
			deprecated = append(deprecated, r)
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

	for _, r := range deprecated {
		id := operationIDForRoute(r)
		method := strings.ToLower(r.Method)
		candidates := []string{id, id + DeprecatedOperationSuffix, id + DeprecatedOperationSuffix + strings.ToUpper(method[:1]) + method[1:]}

		assigned := false
		for _, candidate := range candidates {
			_, taken := seen[candidate]
			if taken {
				continue
			}
			seen[candidate] = r
			ids[r.Method+" "+r.Path] = candidate
			assigned = true
			break
		}
		if !assigned {
			previous := seen[candidates[len(candidates)-1]]
			message := "duplicate operationId " + strconv.Quote(candidates[len(candidates)-1]) + " for " +
				previous.Method + " " + toOpenAPIPath(previous.Path) + " and " + r.Method + " " + toOpenAPIPath(r.Path)
			return nil, ez.New(ez.EINVALID, message, nil)
		}
	}

	return ids, nil
}

// DeprecatedOperationSuffix ends the operationId of a deprecated route aliasing a handler that
// a live route already names its operation after.
const DeprecatedOperationSuffix = "Deprecated"

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

// sortedTagList is the document's tags, sorted, described by TagDescriptions or, with
// DocComments, by the doc comment of their handler package.
func (g *generator) sortedTagList(tagsSet map[string]string, namespaceDocs map[string]string) []any {
	if len(tagsSet) == 0 {
		return nil
	}

	tags := make([]any, 0, len(tagsSet))
	for _, name := range sortedKeys(tagsSet) {
		tag := map[string]any{
			"name": name,
		}
		description := g.tagDescription(name, namespaceDocs[tagsSet[name]])
		if description != "" {
			tag["description"] = description
		}
		tags = append(tags, tag)
	}

	return tags
}

// tagDescription is TagDescriptions' entry for the tag, else, with DocComments, packageDoc.
func (g *generator) tagDescription(tag string, packageDoc string) string {
	description, ok := g.cfg.TagDescriptions[tag]
	if ok {
		return description
	}
	if g.cfg.DocComments {
		return packageDoc
	}

	return ""
}

// operationDescription is, with DocComments, the handler method's doc comment, else the doc
// comment of the module function handling its request.
func (g *generator) operationDescription(handlerDoc string, moduleDoc string) string {
	if !g.cfg.DocComments {
		return ""
	}
	if handlerDoc != "" {
		return handlerDoc
	}

	return moduleDoc
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
