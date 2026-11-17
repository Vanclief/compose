package generator

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"mime"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	"github.com/vanclief/compose/components/rest/api/descriptions"
	"github.com/vanclief/ez"
)

type handlerSource struct {
	File      *ast.File
	Namespace string
	Path      string
	// PackagePath is the import path of the file's package (packagePathForFile).
	PackagePath string
}

// handlerFacts is what the handler bodies tell about each handler, keyed by handler key.
type handlerFacts struct {
	bindsBody    map[string]struct{}
	negotiates   map[string]struct{}
	pathParams   map[string]map[string]map[string]any
	queryParams  map[string]map[string]map[string]any
	requestTypes map[string]typeRef
	// pathFields holds, per handler, the request struct fields its composite literal fills
	// from a path parameter getter; they never travel in the body.
	pathFields map[string][]string
	// responses holds, per handler, the content types its compose response calls serve.
	responses map[string]handlerResponse
	// docs holds, per handler, its method's doc comment.
	docs map[string]string
	// namespaceDocs holds, per namespace, its handler package's doc comment (nested mode).
	namespaceDocs map[string]string
}

// handlerResponse is what a handler's compose response calls say about its content types.
type handlerResponse struct {
	// encoded are the content types the module result is encoded as: application/json for
	// JSONResponse and BindedJSONResponse, application/xml for BindedXMLResponse.
	encoded []string
	// blobs are the constant content types BlobResponse serves the result's bytes as.
	blobs []string
	// open reports a BlobResponse whose content type is computed at run time (negotiated).
	open bool
}

// known reports a handler whose content types the generator could pin down.
func (r handlerResponse) known() bool {
	return len(r.encoded) > 0 || len(r.blobs) > 0
}

// loadHandlerSources parses every handler source once. In nested mode each handler package
// must be wired into the Handlers struct, which names its namespace.
func (g *generator) loadHandlerSources() error {
	namespaces := map[string]string{}
	if !g.cfg.flat() {
		wired, err := g.handlerPackageNamespaces()
		if err != nil {
			return ez.Wrap(err)
		}
		namespaces = wired
	}

	files, err := filepath.Glob(filepath.Join(g.cfg.Root, g.cfg.HandlerGlob))
	if err != nil {
		return ez.Wrap(err)
	}

	fset := token.NewFileSet()
	sources := make([]handlerSource, 0, len(files))
	for _, filePath := range files {
		if strings.HasSuffix(filePath, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filePath, nil, parser.ParseComments)
		if err != nil {
			return ez.Wrap(err)
		}

		namespace := ""
		if !g.cfg.flat() {
			wiredNamespace, ok := namespaces[file.Name.Name]
			if !ok {
				if g.fileContainsHandlerMethod(file) {
					err = ez.New(ez.EINTERNAL, "handler package is not wired in "+g.cfg.HandlersStruct, nil).
						AddData("package", file.Name.Name).
						AddData("file", filePath)
					return err
				}
				continue
			}
			namespace = wiredNamespace
		}

		packagePath, err := g.builder.packagePathForFile(filePath)
		if err != nil {
			return ez.Wrap(err)
		}

		sources = append(sources, handlerSource{
			File:        file,
			Namespace:   namespace,
			Path:        filePath,
			PackagePath: packagePath,
		})
	}

	g.sources = sources
	g.aliases = g.collectHandlerAliases()

	return nil
}

func (g *generator) handlerFacts() handlerFacts {
	facts := handlerFacts{
		bindsBody:     map[string]struct{}{},
		negotiates:    map[string]struct{}{},
		pathParams:    map[string]map[string]map[string]any{},
		queryParams:   map[string]map[string]map[string]any{},
		requestTypes:  map[string]typeRef{},
		pathFields:    map[string][]string{},
		responses:     map[string]handlerResponse{},
		docs:          map[string]string{},
		namespaceDocs: map[string]string{},
	}

	helpersByPackage := g.packageHelpers()

	for _, source := range g.sources {
		imports := g.builder.importMap(source.File)
		pkgPath := source.PackagePath

		packageDoc := descriptions.CommentText(source.File.Doc)
		if !g.cfg.flat() && packageDoc != "" && facts.namespaceDocs[source.Namespace] == "" {
			facts.namespaceDocs[source.Namespace] = packageDoc
		}

		for _, decl := range source.File.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			name, ok := g.handlerMethodKey(fn, source.Namespace)
			if !ok {
				continue
			}
			if fn.Body == nil {
				continue
			}

			doc := descriptions.CommentText(fn.Doc)
			if doc != "" {
				facts.docs[name] = doc
			}

			if functionUsesSelector(fn.Body, g.cfg.BodyBinders) {
				facts.bindsBody[name] = struct{}{}
			}
			if g.cfg.Negotiate != "" && functionUsesSelector(fn.Body, []string{g.cfg.Negotiate}) {
				facts.negotiates[name] = struct{}{}
			}

			pathParams := g.functionPathParameterSchemas(fn.Body, imports, pkgPath)

			response := g.functionResponse(fn.Body, imports, pkgPath)
			if response.known() || response.open {
				facts.responses[name] = response
			}

			queryParams := g.functionQueryParameterSchemas(fn.Body, imports)
			if len(queryParams) > 0 {
				facts.queryParams[name] = queryParams
			}

			ref, ok := g.requestTypeFromHandler(fn.Body, imports, pkgPath, helpersByPackage[pkgPath])
			if ok {
				facts.requestTypes[name] = ref
				fields := g.pathFilledFields(fn.Body, ref, imports, pkgPath, helpersByPackage[pkgPath])
				if len(fields) > 0 {
					facts.pathFields[name] = fields
				}
			}
			// The binder answers 400 for a value its field's type refuses, so a bound field wins
			// over a plain string read in the body; any other read in the body wins over it.
			for _, bound := range g.boundRequestTypes(fn.Body, imports, pkgPath, helpersByPackage[pkgPath]) {
				for param, schema := range g.boundPathParameters(bound) {
					read, isRead := pathParams[param]
					if !isRead || plainString(read) {
						pathParams[param] = schema
					}
				}
			}
			if len(pathParams) > 0 {
				facts.pathParams[name] = pathParams
			}
		}
	}

	fillAliases(facts.bindsBody, g.aliases)
	fillAliases(facts.negotiates, g.aliases)
	fillAliases(facts.pathParams, g.aliases)
	fillAliases(facts.queryParams, g.aliases)
	fillAliases(facts.requestTypes, g.aliases)
	fillAliases(facts.pathFields, g.aliases)
	fillAliases(facts.responses, g.aliases)
	// An alias documented itself keeps its own doc; else it takes the nearest doc down its
	// chain, read from the docs as written so the order the aliases come in does not matter.
	written := maps.Clone(facts.docs)
	for alias := range g.aliases {
		_, documented := written[alias]
		if documented {
			continue
		}
		for _, target := range aliasChain(g.aliases, alias) {
			doc, ok := written[target]
			if ok {
				facts.docs[alias] = doc
				break
			}
		}
	}

	return facts
}

// packageHelpers indexes the package-level functions of every handler package, so
// `body, err := helper(raw)` resolves to the helper's first result instead of leaving the
// variable untyped.
func (g *generator) packageHelpers() map[string]map[string]funcInfo {
	helpersByPackage := map[string]map[string]funcInfo{}

	for _, source := range g.sources {
		imports := g.builder.importMap(source.File)
		pkgPath := source.PackagePath

		for _, decl := range source.File.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Name == nil {
				continue
			}
			if helpersByPackage[pkgPath] == nil {
				helpersByPackage[pkgPath] = map[string]funcInfo{}
			}
			helpersByPackage[pkgPath][fn.Name.Name] = funcInfo{Decl: fn, Imports: imports}
		}
	}

	return helpersByPackage
}

func (g *generator) handlerMethodKey(fn *ast.FuncDecl, namespace string) (string, bool) {
	if fn.Recv == nil {
		return "", false
	}
	if len(fn.Recv.List) == 0 {
		return "", false
	}

	if !g.isHandlerReceiver(fn.Recv.List[0].Type) {
		return "", false
	}

	return handlerKey(namespace, fn.Name.Name), true
}

func (g *generator) isHandlerReceiver(expr ast.Expr) bool {
	for {
		star, ok := expr.(*ast.StarExpr)
		if !ok {
			break
		}
		expr = star.X
	}

	ident, ok := expr.(*ast.Ident)
	if !ok {
		return false
	}
	return ident.Name == g.cfg.HandlerReceiver
}

// handlerKey is "namespace.Method", or just "Method" for flat handlers.
func handlerKey(namespace string, method string) string {
	if method == "" {
		return ""
	}
	if namespace == "" {
		return method
	}

	return namespace + "." + method
}

func (g *generator) handlerPackageNamespaces() (map[string]string, error) {
	rootPath := filepath.Join(g.cfg.Root, g.cfg.HandlersFile)
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, rootPath, nil, 0)
	if err != nil {
		return nil, ez.Wrap(err)
	}

	imports := g.builder.importMap(file)
	namespaces := map[string]string{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok || typeSpec.Name.Name != g.cfg.HandlersStruct {
				continue
			}
			structure, ok := typeSpec.Type.(*ast.StructType)
			if !ok {
				continue
			}
			for _, field := range structure.Fields.List {
				if len(field.Names) != 1 {
					continue
				}
				packageName, ok := g.handlerFieldPackage(field.Type)
				if !ok {
					continue
				}
				importPath, ok := imports[packageName]
				if !ok {
					continue
				}
				namespaces[g.builder.packageName(importPath)] = lowerFirst(field.Names[0].Name)
			}
		}
	}

	return namespaces, nil
}

func (g *generator) handlerFieldPackage(expr ast.Expr) (string, bool) {
	star, ok := expr.(*ast.StarExpr)
	if !ok {
		return "", false
	}
	selector, ok := star.X.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != g.cfg.HandlerReceiver {
		return "", false
	}
	packageName, ok := selector.X.(*ast.Ident)
	if !ok {
		return "", false
	}

	return packageName.Name, true
}

func (g *generator) fileContainsHandlerMethod(file *ast.File) bool {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 {
			continue
		}
		if g.isHandlerReceiver(fn.Recv.List[0].Type) {
			return true
		}
	}

	return false
}

// collectHandlerAliases maps every handler method whose whole body is `return h.Other(c)` to
// Other, so the alias inherits everything derived from Other's body.
func (g *generator) collectHandlerAliases() map[string]string {
	aliases := map[string]string{}
	for _, source := range g.sources {
		for _, decl := range source.File.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			aliasName, ok := g.handlerMethodKey(fn, source.Namespace)
			if !ok {
				continue
			}
			targetName, ok := handlerAliasTarget(fn)
			if !ok {
				continue
			}
			aliases[aliasName] = handlerKey(source.Namespace, targetName)
		}
	}

	return aliases
}

// aliasChain is the methods an alias leads to, in order: its target, the target's target if
// that is an alias too, and so on until a method that is not an alias (or one already in the
// chain, for a loop). The last one need not be a handler: a handler whose whole body is
// `return h.BindedJSONResponse(...)` is an alias of BindedJSONResponse.
func aliasChain(aliases map[string]string, alias string) []string {
	seen := map[string]bool{alias: true}
	var chain []string
	target, ok := aliases[alias]
	for ok && !seen[target] {
		seen[target] = true
		chain = append(chain, target)
		target, ok = aliases[target]
	}

	return chain
}

// fillAliases gives each alias the value of the deepest method down its chain that has one,
// as if each alias took its target's value after the target took its own; an alias whose chain
// has none keeps what it has. Values are read from out as it was before any alias was filled,
// so the order the aliases come in does not matter.
func fillAliases[V any](out map[string]V, aliases map[string]string) {
	original := maps.Clone(out)
	for alias := range aliases {
		for _, target := range aliasChain(aliases, alias) {
			value, ok := original[target]
			if ok {
				out[alias] = value
			}
		}
	}
}

func handlerAliasTarget(fn *ast.FuncDecl) (string, bool) {
	if fn.Body == nil || len(fn.Body.List) != 1 {
		return "", false
	}

	receiverName := ""
	names := fn.Recv.List[0].Names
	if len(names) == 1 {
		receiverName = names[0].Name
	}
	if receiverName == "" || receiverName == "_" {
		return "", false
	}

	returnStatement, ok := fn.Body.List[0].(*ast.ReturnStmt)
	if !ok || len(returnStatement.Results) != 1 {
		return "", false
	}

	call, ok := returnStatement.Results[0].(*ast.CallExpr)
	if !ok {
		return "", false
	}

	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}

	receiver, ok := selector.X.(*ast.Ident)
	if !ok || receiver.Name != receiverName {
		return "", false
	}

	return selector.Sel.Name, true
}

// functionUsesSelector reports whether body calls any `x.Name(...)` with Name in names.
func functionUsesSelector(body *ast.BlockStmt, names []string) bool {
	found := false

	ast.Inspect(body, func(node ast.Node) bool {
		if found {
			return false
		}

		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}

		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}

		if selector.Sel == nil {
			return true
		}

		for _, name := range names {
			if selector.Sel.Name == name {
				found = true
				return false
			}
		}

		return true
	})

	return found
}

// functionResponse reads the content types off the compose response calls of a handler body.
// BlobResponse(c, op, request, contentType, body) serves contentType when it is a string
// literal or a constant; any other expression (a negotiated variable) leaves it open.
func (g *generator) functionResponse(body *ast.BlockStmt, imports map[string]string, pkgPath string) handlerResponse {
	var response handlerResponse

	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel == nil {
			return true
		}

		switch selector.Sel.Name {
		case "JSONResponse", "BindedJSONResponse":
			response.encoded = appendUnique(response.encoded, "application/json")
		case "BindedXMLResponse":
			response.encoded = appendUnique(response.encoded, "application/xml")
		case "BlobResponse":
			if len(call.Args) < 2 {
				return true
			}
			contentType, ok := g.constantString(call.Args[len(call.Args)-2], imports, pkgPath)
			if !ok {
				response.open = true
				return true
			}
			response.blobs = appendUnique(response.blobs, mediaType(contentType))
		}

		return true
	})

	return response
}

// constantString resolves a string literal, or a constant of the handler's package or an
// imported one, to its value.
func (g *generator) constantString(expr ast.Expr, imports map[string]string, pkgPath string) (string, bool) {
	literal, ok := stringLiteralValue(expr)
	if ok {
		return literal, true
	}

	importPath := pkgPath
	name := ""
	switch typed := expr.(type) {
	case *ast.Ident:
		name = typed.Name
	case *ast.SelectorExpr:
		pkgIdent, ok := typed.X.(*ast.Ident)
		if !ok {
			return "", false
		}
		importPath = imports[pkgIdent.Name]
		name = typed.Sel.Name
	default:
		return "", false
	}
	if importPath == "" {
		return "", false
	}

	pkg, err := g.builder.loadPackage(importPath)
	if err != nil {
		return "", false
	}
	value, ok := pkg.Consts[name]

	return value, ok
}

// mediaType drops the parameters of a content type: "text/plain; charset=utf-8" is
// "text/plain".
func mediaType(contentType string) string {
	parsed, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return strings.TrimSpace(contentType)
	}

	return parsed
}

func appendUnique(values []string, value string) []string {
	if containsString(values, value) {
		return values
	}

	return append(values, value)
}

// functionQueryParameterSchemas derives a handler's query interface from the query reads in
// its body. Handlers bind query params by hand, so the c.QueryParam and helper calls are the
// source of truth, not the request struct (names diverge: "clusters" binds ClusterIDs, date
// bounds become filters). Reads with a non-literal name cannot be attributed and are skipped,
// as are enum reads whose parse function does not resolve to an imported type. A name read
// several times keeps its first read, except that a typed getter wins over a plain
// c.QueryParam (`if c.QueryParam("unit_id") != "" { h.GetQueryParamInt64(c, "unit_id") }`).
func (g *generator) functionQueryParameterSchemas(body *ast.BlockStmt, imports map[string]string) map[string]map[string]any {
	params := map[string]map[string]any{}
	plain := map[string]bool{}

	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}

		fun := call.Fun
		index, ok := fun.(*ast.IndexExpr)
		if ok {
			fun = index.X
		}
		selector, ok := fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if selector.Sel == nil {
			return true
		}

		getter, ok := g.cfg.QueryGetters[selector.Sel.Name]
		if !ok {
			return true
		}

		name := getter.Name
		if name == "" {
			if len(call.Args) <= getter.NameArg {
				return true
			}
			literal, ok := stringLiteralValue(call.Args[getter.NameArg])
			if !ok || literal == "" {
				return true
			}
			name = literal
		}

		schema, ok := g.queryGetterSchema(getter, call, imports)
		if !ok {
			return true
		}

		isPlain := selector.Sel.Name == plainQueryGetter
		_, exists := params[name]
		if exists && (isPlain || !plain[name]) {
			return true
		}

		params[name] = schema
		plain[name] = isPlain
		return true
	})

	return params
}

// plainQueryGetter is echo's untyped query read, which any typed getter of the same name
// refines.
const plainQueryGetter = "QueryParam"

// queryGetterSchema is the getter's fixed schema or, for an enum getter such as
// ParseEnumQueryParams(c, name, pkg.<Type>FromString), an array of $refs to the enum's
// component schema, named by the parse-function argument.
func (g *generator) queryGetterSchema(getter QueryGetter, call *ast.CallExpr, imports map[string]string) (map[string]any, bool) {
	if getter.EnumArg <= 0 {
		schema := cloneSchema(getter.Schema)
		if getter.DefaultArg > 0 && len(call.Args) > getter.DefaultArg {
			value, ok := intLiteralValue(call.Args[getter.DefaultArg])
			if ok {
				schema["default"] = value
			}
		}
		if schema["type"] == "integer" && getter.Name == limitParameter && g.cfg.MaxPageSize > 0 {
			schema["maximum"] = g.cfg.MaxPageSize
		}
		return schema, true
	}

	if len(call.Args) <= getter.EnumArg {
		return nil, false
	}

	parseSelector, ok := call.Args[getter.EnumArg].(*ast.SelectorExpr)
	if !ok {
		return nil, false
	}
	pkgIdent, ok := parseSelector.X.(*ast.Ident)
	if !ok {
		return nil, false
	}
	importPath := imports[pkgIdent.Name]
	if importPath == "" {
		return nil, false
	}

	typeName := strings.TrimSuffix(parseSelector.Sel.Name, "FromString")
	if typeName == parseSelector.Sel.Name || typeName == "" {
		return nil, false
	}

	ref := typeRef{ImportPath: importPath, Name: typeName}
	schema := map[string]any{
		"type": "array",
		"items": map[string]any{
			"$ref": schemaRefPrefix + g.builder.schemaName(ref),
		},
	}

	return schema, true
}

func (g *generator) functionPathParameterSchemas(body *ast.BlockStmt, imports map[string]string, pkgPath string) map[string]map[string]any {
	params := map[string]map[string]any{}

	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}

		name, schema, ok := g.pathParameterCall(call, imports, pkgPath)
		if !ok {
			return true
		}

		// A read that gives a plain string does not undo one that gives another schema, which
		// the server checks; between two others the last one wins.
		read, isRead := params[name]
		if isRead && plainString(schema) && !plainString(read) {
			return true
		}
		params[name] = schema
		return true
	})

	return params
}

// plainString reports the schema {type: string}, which says nothing of a path segment.
func plainString(schema map[string]any) bool {
	return reflect.DeepEqual(schema, map[string]any{"type": "string"})
}

// echoImportPath is the package of echo's PathParam[T] and PathParamOr[T], which read a path
// parameter as T.
const echoImportPath = "github.com/labstack/echo/v5"

// pathParameterCall matches a read of a path parameter whose name is a string literal or a
// constant declared with one (parameterName): a ParamGetters call, with the getter's schema, or
// echo's PathParam[T] or PathParamOr[T], with T's (pathValueSchema). T is read from the type
// argument or, for a PathParamOr without one, from a default written as a conversion, `int64(0)`;
// any other T inferred from the default is not read.
func (g *generator) pathParameterCall(call *ast.CallExpr, imports map[string]string, pkgPath string) (string, map[string]any, bool) {
	fun := call.Fun
	var typeArgument ast.Expr
	index, generic := fun.(*ast.IndexExpr)
	if generic {
		fun = index.X
		typeArgument = index.Index
	}
	selector, ok := fun.(*ast.SelectorExpr)
	if !ok || selector.Sel == nil {
		return "", nil, false
	}

	if isEchoPathRead(selector, imports) {
		if len(call.Args) < 2 {
			return "", nil, false
		}
		name, ok := g.parameterName(call.Args[1], imports, pkgPath)
		if !ok {
			return "", nil, false
		}
		if typeArgument == nil && selector.Sel.Name == "PathParamOr" && len(call.Args) > 2 {
			conversion, isCall := call.Args[2].(*ast.CallExpr)
			if isCall && len(conversion.Args) == 1 {
				typeArgument = conversion.Fun
			}
		}
		return name, pathValueSchema(typeArgument), true
	}
	if generic {
		return "", nil, false
	}

	getter, ok := g.cfg.ParamGetters[selector.Sel.Name]
	if !ok {
		return "", nil, false
	}

	if len(call.Args) <= getter.NameArg {
		return "", nil, false
	}

	name, ok := g.parameterName(call.Args[getter.NameArg], imports, pkgPath)
	if !ok {
		return "", nil, false
	}

	return name, cloneSchema(getter.Schema), true
}

// isEchoPathRead reports echo's PathParam or PathParamOr: the selector's qualifier is an import
// of echoImportPath, not a name the file declares.
func isEchoPathRead(selector *ast.SelectorExpr, imports map[string]string) bool {
	if selector.Sel.Name != "PathParam" && selector.Sel.Name != "PathParamOr" {
		return false
	}
	qualifier, ok := selector.X.(*ast.Ident)

	return ok && qualifier.Obj == nil && imports[qualifier.Name] == echoImportPath
}

// parameterName is the parameter name a read passes, when it is a string literal or a string
// constant: one declared in the handler's file, the function included, read from its
// declaration, or one of the handler's package or an imported one (constantString). A
// variable or a parameter is not read, nor a constant declared without a string literal, nor a
// field of a variable named like an import.
func (g *generator) parameterName(expr ast.Expr, imports map[string]string, pkgPath string) (string, bool) {
	selector, ok := expr.(*ast.SelectorExpr)
	if ok {
		qualifier, isIdent := selector.X.(*ast.Ident)
		if isIdent && qualifier.Obj != nil {
			// The file declares the qualifier, so it is not a package.
			return "", false
		}
	}
	ident, ok := expr.(*ast.Ident)
	if ok && ident.Obj != nil {
		// The parser resolved the name in the file, where it may shadow a constant of the
		// package's other files. ast.Object is deprecated in favour of go/types, which the
		// generator does not run; this is the parser's own scope resolution, so the handler
		// files must be parsed without parser.SkipObjectResolution.
		spec, isValue := ident.Obj.Decl.(*ast.ValueSpec)
		if ident.Obj.Kind != ast.Con || !isValue {
			return "", false
		}
		for i, declared := range spec.Names {
			if declared.Name != ident.Name || i >= len(spec.Values) {
				continue
			}
			name, ok := stringLiteralValue(spec.Values[i])
			return name, ok && name != ""
		}
		return "", false
	}

	name, ok := g.constantString(expr, imports, pkgPath)

	return name, ok && name != ""
}

// pathValueKinds are the predeclared types echo parses a path value into, with PathParam[T] and
// its binder alike: bool, string, the floats and the integers but uintptr.
var pathValueKinds = map[string]bool{
	"bool": true, "string": true, "float32": true, "float64": true,
	"int": true, "int8": true, "int16": true, "int32": true, "int64": true, "rune": true,
	"uint": true, "uint8": true, "uint16": true, "uint32": true, "uint64": true, "byte": true,
}

// pathValueSchema is the schema of a path parameter echo parses into the type expr: a
// predeclared type of pathValueKinds has its own (builtinSchema), and any other type is a
// string, as the generator does not resolve it.
func pathValueSchema(expr ast.Expr) map[string]any {
	ident, ok := expr.(*ast.Ident)
	if !ok || !pathValueKinds[ident.Name] {
		return map[string]any{"type": "string"}
	}
	schema, _ := builtinSchema(ident.Name)

	return schema
}

// boundRequestTypes are the types a handler body's BodyBinders calls bind, their last argument,
// read as requestTypeFromHandler reads the request type (requestArgumentType).
func (g *generator) boundRequestTypes(body *ast.BlockStmt, imports map[string]string, pkgPath string, helpers map[string]funcInfo) []typeRef {
	varTypes := localVariableTypes(body, imports, pkgPath, helpers)
	var refs []typeRef
	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel == nil || !containsString(g.cfg.BodyBinders, selector.Sel.Name) {
			return true
		}
		ref, ok := requestArgumentType(call.Args[len(call.Args)-1], varTypes, imports, pkgPath)
		if ok {
			refs = append(refs, ref)
		}
		return true
	})

	return refs
}

// boundPathParameters are the path parameters echo's binder parses into the request type ref,
// which a BodyBinders call binds, path parameters first: an exported field of ref's struct
// declaration whose param tag is a parameter's name has the schema of its type
// (pathValueSchema). A field of an embedded struct and a tag naming the parameter in other
// letter case, which the binder also fills, are not read.
func (g *generator) boundPathParameters(ref typeRef) map[string]map[string]any {
	pkg, err := g.builder.loadPackage(ref.ImportPath)
	if err != nil {
		// Safe to drop: its parameters stay strings, and where ref is the handler's request
		// type, building the request's schema reports the package that cannot be read.
		return nil
	}
	info, ok := pkg.Types[ref.Name]
	if !ok {
		return nil
	}
	structure, ok := info.Spec.Type.(*ast.StructType)
	if !ok {
		return nil
	}

	params := map[string]map[string]any{}
	for _, field := range structure.Fields.List {
		name := fieldTag(field, "param")
		if name == "" || len(field.Names) == 0 || !ast.IsExported(field.Names[0].Name) {
			continue
		}
		params[name] = pathValueSchema(field.Type)
	}

	return params
}

// pathFilledFields returns the fields of the request type filled from a path parameter, either
// straight from a read (pathParameterCall) or through a variable assigned from one, in a composite
// literal (`&Request{ID: c.Param("id")}`, `&Request{ComplexID: complexID}`) or an assignment
// to a request variable (`request.ComplexID = complexID`).
func (g *generator) pathFilledFields(body *ast.BlockStmt, ref typeRef, imports map[string]string, pkgPath string, helpers map[string]funcInfo) []string {
	pathVars := map[string]bool{}
	ast.Inspect(body, func(node ast.Node) bool {
		assign, ok := node.(*ast.AssignStmt)
		if !ok {
			return true
		}
		// `id, err := h.GetParameterInt64(c, "id")` and `id := c.Param("id")`.
		if len(assign.Rhs) == 1 && len(assign.Lhs) >= 1 {
			ident, isIdent := assign.Lhs[0].(*ast.Ident)
			if isIdent && g.isPathParameterCall(assign.Rhs[0], imports, pkgPath) {
				pathVars[ident.Name] = true
			}
		}
		return true
	})

	fromPath := func(expr ast.Expr) bool {
		ident, ok := expr.(*ast.Ident)
		if ok {
			return pathVars[ident.Name]
		}
		return g.isPathParameterCall(expr, imports, pkgPath)
	}

	varTypes := localVariableTypes(body, imports, pkgPath, helpers)

	var fields []string
	seen := map[string]bool{}
	add := func(name string) {
		if !seen[name] {
			seen[name] = true
			fields = append(fields, name)
		}
	}

	ast.Inspect(body, func(node ast.Node) bool {
		assign, ok := node.(*ast.AssignStmt)
		if ok && len(assign.Lhs) == len(assign.Rhs) {
			for i, left := range assign.Lhs {
				selector, ok := left.(*ast.SelectorExpr)
				if !ok || !fromPath(assign.Rhs[i]) {
					continue
				}
				target, ok := selector.X.(*ast.Ident)
				if ok && varTypes[target.Name] == ref {
					add(selector.Sel.Name)
				}
			}
			return true
		}

		literal, ok := node.(*ast.CompositeLit)
		if !ok {
			return true
		}

		literalType, ok := typeRefFromExpr(literal.Type, imports, pkgPath)
		if !ok || literalType != ref {
			return true
		}

		for _, element := range literal.Elts {
			keyValue, ok := element.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := keyValue.Key.(*ast.Ident)
			if ok && fromPath(keyValue.Value) {
				add(key.Name)
			}
		}

		return true
	})

	return fields
}

// isPathParameterCall reports a read of a path parameter (pathParameterCall).
func (g *generator) isPathParameterCall(expr ast.Expr, imports map[string]string, pkgPath string) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	_, _, ok = g.pathParameterCall(call, imports, pkgPath)

	return ok
}

func intLiteralValue(expr ast.Expr) (int, bool) {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.INT {
		return 0, false
	}

	value, err := strconv.Atoi(lit.Value)
	if err != nil {
		return 0, false
	}

	return value, true
}

func stringLiteralValue(expr ast.Expr) (string, bool) {
	lit, ok := expr.(*ast.BasicLit)
	if !ok {
		return "", false
	}
	if lit.Kind != token.STRING {
		return "", false
	}

	value, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}

	return value, true
}
