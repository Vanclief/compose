package generator

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/vanclief/ez"
)

type handlerSource struct {
	File      *ast.File
	Namespace string
	Path      string
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
		file, err := parser.ParseFile(fset, filePath, nil, 0)
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

		sources = append(sources, handlerSource{
			File:      file,
			Namespace: namespace,
			Path:      filePath,
		})
	}

	g.sources = sources
	g.aliases = g.collectHandlerAliases()

	return nil
}

func (g *generator) handlerFacts() handlerFacts {
	facts := handlerFacts{
		bindsBody:    map[string]struct{}{},
		negotiates:   map[string]struct{}{},
		pathParams:   map[string]map[string]map[string]any{},
		queryParams:  map[string]map[string]map[string]any{},
		requestTypes: map[string]typeRef{},
		pathFields:   map[string][]string{},
	}

	helpersByPackage := g.packageHelpers()

	for _, source := range g.sources {
		imports := importMap(source.File)
		pkgPath := packagePathForFile(g.cfg.Root, g.builder.modulePath, source.Path)

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

			if functionUsesSelector(fn.Body, g.cfg.BodyBinders) {
				facts.bindsBody[name] = struct{}{}
			}
			if g.cfg.Negotiate != "" && functionUsesSelector(fn.Body, []string{g.cfg.Negotiate}) {
				facts.negotiates[name] = struct{}{}
			}

			pathParams := g.functionPathParameterSchemas(fn.Body)
			if len(pathParams) > 0 {
				facts.pathParams[name] = pathParams
			}

			queryParams := g.functionQueryParameterSchemas(fn.Body, imports)
			if len(queryParams) > 0 {
				facts.queryParams[name] = queryParams
			}

			ref, ok := g.requestTypeFromHandler(fn.Body, imports, pkgPath, helpersByPackage[pkgPath])
			if ok {
				facts.requestTypes[name] = ref
				fields := g.pathFilledFields(fn.Body, ref, imports, pkgPath)
				if len(fields) > 0 {
					facts.pathFields[name] = fields
				}
			}
		}
	}

	fillAliases(facts.bindsBody, g.aliases)
	fillAliases(facts.negotiates, g.aliases)
	fillAliases(facts.pathParams, g.aliases)
	fillAliases(facts.queryParams, g.aliases)
	fillAliases(facts.requestTypes, g.aliases)
	fillAliases(facts.pathFields, g.aliases)

	return facts
}

// packageHelpers indexes the package-level functions of every handler package, so
// `body, err := helper(raw)` resolves to the helper's first result instead of leaving the
// variable untyped.
func (g *generator) packageHelpers() map[string]map[string]funcInfo {
	helpersByPackage := map[string]map[string]funcInfo{}

	for _, source := range g.sources {
		imports := importMap(source.File)
		pkgPath := packagePathForFile(g.cfg.Root, g.builder.modulePath, source.Path)

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

	imports := importMap(file)
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
				namespaces[path.Base(importPath)] = lowerFirst(field.Names[0].Name)
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

func fillAliases[V any](out map[string]V, aliases map[string]string) {
	for alias, target := range aliases {
		value, ok := out[target]
		if ok {
			out[alias] = value
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

// functionQueryParameterSchemas derives a handler's query interface from the query reads in
// its body. Handlers bind query params by hand, so the c.QueryParam and helper calls are the
// source of truth, not the request struct (names diverge: "clusters" binds ClusterIDs, date
// bounds become filters). Reads with a non-literal name cannot be attributed and are skipped,
// as are enum reads whose parse function does not resolve to an imported type.
func (g *generator) functionQueryParameterSchemas(body *ast.BlockStmt, imports map[string]string) map[string]map[string]any {
	params := map[string]map[string]any{}

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

		_, exists := params[name]
		if exists {
			return true
		}

		params[name] = schema
		return true
	})

	return params
}

// queryGetterSchema is the getter's fixed schema or, for an enum getter such as
// ParseEnumQueryParams(c, name, pkg.<Type>FromString), an array of $refs to the enum's
// component schema, named by the parse-function argument.
func (g *generator) queryGetterSchema(getter QueryGetter, call *ast.CallExpr, imports map[string]string) (map[string]any, bool) {
	if getter.EnumArg <= 0 {
		return cloneSchema(getter.Schema), true
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

func (g *generator) functionPathParameterSchemas(body *ast.BlockStmt) map[string]map[string]any {
	params := map[string]map[string]any{}

	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}

		name, getter, ok := g.pathParameterCall(call)
		if !ok {
			return true
		}

		params[name] = cloneSchema(getter.Schema)
		return true
	})

	return params
}

// pathParameterCall matches a ParamGetters call with a string-literal parameter name.
func (g *generator) pathParameterCall(call *ast.CallExpr) (string, ParamGetter, bool) {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", ParamGetter{}, false
	}
	if selector.Sel == nil {
		return "", ParamGetter{}, false
	}

	getter, ok := g.cfg.ParamGetters[selector.Sel.Name]
	if !ok {
		return "", ParamGetter{}, false
	}

	if len(call.Args) <= getter.NameArg {
		return "", ParamGetter{}, false
	}

	name, ok := stringLiteralValue(call.Args[getter.NameArg])
	if !ok || name == "" {
		return "", ParamGetter{}, false
	}

	return name, getter, true
}

// pathFilledFields returns the fields of the request type's composite literals that are
// filled straight from a path parameter getter, as in `&Request{ID: c.Param("id")}`.
func (g *generator) pathFilledFields(body *ast.BlockStmt, ref typeRef, imports map[string]string, pkgPath string) []string {
	var fields []string
	seen := map[string]bool{}

	ast.Inspect(body, func(node ast.Node) bool {
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
			if !ok {
				continue
			}
			call, ok := keyValue.Value.(*ast.CallExpr)
			if !ok {
				continue
			}
			_, _, ok = g.pathParameterCall(call)
			if !ok || seen[key.Name] {
				continue
			}
			seen[key.Name] = true
			fields = append(fields, key.Name)
		}

		return true
	})

	return fields
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
