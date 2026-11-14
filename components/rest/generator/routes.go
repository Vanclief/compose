package generator

import (
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/vanclief/ez"
)

// Route is one route declaration found in the route files.
type Route struct {
	Method string
	// Path is the Echo path, e.g. "/units/:id".
	Path string
	// Namespace is the handler namespace: the Handlers struct field in nested mode, the first
	// path segment in flat mode. It names the operation's tag and prefixes its operationId.
	Namespace string
	// Handler is the handler method name.
	Handler    string
	SourceFile string
	SourceLine int
	// Deprecated routes are wrapped in Config.DeprecatedWrapper; only the full spec documents them.
	Deprecated bool
	// BindsBody reports a handler calling one of Config.BodyBinders.
	BindsBody bool
	// Negotiates reports a handler calling Config.Negotiate.
	Negotiates bool

	flat bool
}

// HandlerKey identifies the route's handler method: "namespace.Method" in nested mode,
// "Method" in flat mode.
func (r Route) HandlerKey() string {
	if r.flat {
		return r.Handler
	}

	return handlerKey(r.Namespace, r.Handler)
}

// OpenAPIPath is the route path in OpenAPI form: ":id" becomes "{id}" and a trailing "/*"
// becomes "/{path}". Like Echo, a parameter's name runs to the next "/": ":user-id" becomes
// "{user-id}" and ":name.ext" becomes "{name.ext}".
func (r Route) OpenAPIPath() string {
	return toOpenAPIPath(r.Path)
}

var (
	rePathParam = regexp.MustCompile(`:([^/]+)`)
	reOASParam  = regexp.MustCompile(`\{([^}]+)\}`)
)

// ParseRoutes returns every route declared in the files matched by cfg.RoutesGlob that a
// default build compiles, sorted by path then method.
func ParseRoutes(cfg Config) ([]Route, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, ez.Wrap(err)
	}

	routes, err := parseRoutes(cfg)
	if err != nil {
		return nil, ez.Wrap(err)
	}

	return routes, nil
}

func parseRoutes(cfg Config) ([]Route, error) {
	routeFiles, err := filepath.Glob(filepath.Join(cfg.Root, cfg.RoutesGlob))
	if err != nil {
		return nil, ez.Wrap(err)
	}
	if len(routeFiles) == 0 {
		return nil, ez.New(ez.ENOTFOUND, "no route files match "+cfg.RoutesGlob, nil)
	}

	parser := newRouteParser(cfg)

	var routes []Route
	var handlerless []string
	for _, abs := range routeFiles {
		built, err := builtGoFile(abs)
		if err != nil {
			return nil, ez.Wrap(err)
		}
		if !built {
			continue
		}

		parsed, missing, err := parser.parseFile(abs)
		if err != nil {
			return nil, ez.Wrap(err)
		}
		routes = append(routes, parsed...)
		handlerless = append(handlerless, missing...)
	}

	if len(handlerless) > 0 {
		message := "routes without a " + cfg.ReceiverVar + " handler method; route them through a handler method or Skip them:\n  " + strings.Join(handlerless, "\n  ")
		return nil, ez.New(ez.EINVALID, message, nil)
	}
	if len(routes) == 0 {
		return nil, ez.New(ez.ENOTFOUND, "no routes found in "+cfg.RoutesGlob, nil)
	}

	sort.SliceStable(routes, func(i int, j int) bool {
		if routes[i].Path == routes[j].Path {
			return routes[i].Method < routes[j].Method
		}
		return routes[i].Path < routes[j].Path
	})

	return routes, nil
}

// builtGoFile reports whether a default build on the platform the generator runs on compiles a
// Go file: a test file, and a file whose name or build constraints select another platform or a
// tag such as ignore, are not. A file whose name does not end in .go is left to the parser.
func builtGoFile(path string) (bool, error) {
	if !strings.HasSuffix(path, ".go") {
		return true, nil
	}
	if strings.HasSuffix(path, "_test.go") {
		return false, nil
	}

	match, err := build.Default.MatchFile(filepath.Dir(path), filepath.Base(path))
	if err != nil {
		return false, ez.New(ez.EINVALID, "cannot evaluate the build constraints of "+path+": "+err.Error(), err)
	}

	return match, nil
}

type routeParser struct {
	cfg Config
}

func newRouteParser(cfg Config) routeParser {
	return routeParser{cfg: cfg}
}

// parseFile reads the group assignments and route calls of one file, in source order. A
// group assignment is `child := parent.Group("/prefix", ...)`; a route call is
// `group.GET("/path", handler, ...)` (or POST, PUT, PATCH, DELETE, HEAD) with a string literal
// path, anywhere in the file, including calls spread over several lines; comments and string
// literals are not code and declare nothing. It returns the routes and the
// "file:line METHOD path" of every route with no handler method that Skip does not cover.
func (p routeParser) parseFile(absPath string) ([]Route, []string, error) {
	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, nil, ez.Wrap(err)
	}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, absPath, data, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, nil, ez.New(ez.EINVALID, absPath+": route file is not valid Go", err)
	}

	basePaths := map[string]string{}
	rootPaths := map[string]string{}
	for name, base := range p.cfg.RootGroups {
		basePaths[name] = base
		rootPaths[name] = base
	}

	var routes []Route
	var handlerless []string

	ast.Inspect(file, func(node ast.Node) bool {
		if err != nil {
			return false
		}

		assign, ok := node.(*ast.AssignStmt)
		if ok {
			err = p.readGroupAssign(fset, absPath, assign, basePaths, rootPaths)
			if err != nil {
				err = ez.Wrap(err)
				return false
			}

			return true
		}

		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}

		receiver, method, suffix, ok := routeCall(call)
		if !ok {
			return true
		}

		lineNo := fset.Position(call.Pos()).Line
		location := absPath + ":" + strconv.Itoa(lineNo)

		basePath, ok := basePaths[receiver]
		if !ok {
			err = ez.New(ez.EINVALID, location+": unknown route group "+strconv.Quote(receiver), nil)
			return false
		}

		fullPath := basePath + suffix

		r := Route{
			Method:     method,
			Path:       fullPath,
			SourceFile: filepath.ToSlash(absPath),
			SourceLine: lineNo,
			flat:       p.cfg.flat(),
		}

		if p.cfg.DeprecatedWrapper != "" {
			r.Deprecated = callsWrapper(call, p.cfg.DeprecatedWrapper)
		}

		names := handlerReference(call.Args[1], p.cfg.ReceiverVar)
		switch {
		case len(names) > 1:
			r.Namespace = lowerFirst(names[0])
			r.Handler = names[1]
		case len(names) == 1 && !p.cfg.flat():
			err = ez.New(ez.EINVALID, location+": flat handler reference "+p.cfg.ReceiverVar+"."+names[0]+" is not allowed with a HandlersFile", nil)
			return false
		case len(names) == 1:
			r.Namespace = flatNamespace(strings.TrimPrefix(fullPath, rootPaths[receiver]))
			r.Handler = names[0]
		}

		if r.Handler == "" && !p.skip(method, fullPath) {
			handlerless = append(handlerless, location+" "+method+" "+fullPath)
		}

		routes = append(routes, r)

		return true
	})
	if err != nil {
		return nil, nil, err
	}

	return routes, handlerless, nil
}

// readGroupAssign records `child := parent.Group("/prefix", ...)`: child's base path is
// parent's plus the prefix, and its root path is parent's. Any other assignment is ignored.
func (p routeParser) readGroupAssign(fset *token.FileSet, absPath string, assign *ast.AssignStmt, basePaths map[string]string, rootPaths map[string]string) error {
	if assign.Tok != token.DEFINE || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
		return nil
	}

	child, ok := assign.Lhs[0].(*ast.Ident)
	if !ok {
		return nil
	}

	call, ok := assign.Rhs[0].(*ast.CallExpr)
	if !ok {
		return nil
	}

	parent, name, ok := selectorCall(call)
	if !ok || name != "Group" || len(call.Args) == 0 {
		return nil
	}

	groupPath, ok := stringLiteral(call.Args[0])
	if !ok {
		return nil
	}

	parentPath, ok := basePaths[parent]
	if !ok {
		location := absPath + ":" + strconv.Itoa(fset.Position(assign.Pos()).Line)
		return ez.New(ez.EINVALID, location+": unknown parent group "+strconv.Quote(parent), nil)
	}

	basePaths[child.Name] = parentPath + groupPath
	rootPaths[child.Name] = rootPaths[parent]

	return nil
}

// routeCall reports whether call is `group.METHOD("/path", ...)` with an HTTP method Echo
// registers that way, a string literal path and at least one argument after it.
func routeCall(call *ast.CallExpr) (string, string, string, bool) {
	receiver, method, ok := selectorCall(call)
	if !ok || len(call.Args) < 2 {
		return "", "", "", false
	}

	switch method {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD":
	default:
		return "", "", "", false
	}

	path, ok := stringLiteral(call.Args[0])
	if !ok {
		return "", "", "", false
	}

	return receiver, method, path, true
}

// selectorCall returns x and name of a call `x.name(...)` whose x is a plain identifier.
func selectorCall(call *ast.CallExpr) (string, string, bool) {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", "", false
	}

	x, ok := selector.X.(*ast.Ident)
	if !ok {
		return "", "", false
	}

	return x.Name, selector.Sel.Name, true
}

func stringLiteral(expr ast.Expr) (string, bool) {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}

	value, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}

	return value, true
}

// handlerReference returns the names after the receiver variable of the first reference to it
// in a route call's handler argument, the one after the path: ["Namespace", "Method"] for
// h.Namespace.Method, ["Method"] for h.Method. It reads the argument's syntax tree, so comments
// and string literals refer to nothing. The arguments after it are middleware and are not read,
// so a route is never documented as a middleware method, and the body of a function literal is
// not read either: the literal is the route's handler itself, not an h.Namespace.Method it calls.
func handlerReference(handler ast.Expr, receiver string) []string {
	var names []string
	ast.Inspect(handler, func(node ast.Node) bool {
		if names != nil {
			return false
		}
		switch n := node.(type) {
		case *ast.FuncLit:
			return false
		case *ast.SelectorExpr:
			chain, ok := receiverChain(n, receiver)
			if ok {
				names = chain
				return false
			}
		}
		return true
	})

	return names
}

// receiverChain returns the names after the receiver variable of a selector written on it,
// [A, B] for h.A.B, through parentheses.
func receiverChain(expr ast.Expr, receiver string) ([]string, bool) {
	var names []string
	for {
		switch x := expr.(type) {
		case *ast.ParenExpr:
			expr = x.X
		case *ast.SelectorExpr:
			names = append([]string{x.Sel.Name}, names...)
			expr = x.X
		case *ast.Ident:
			return names, x.Name == receiver && len(names) > 0
		default:
			return nil, false
		}
	}
}

// callsWrapper reports a call of the Config.DeprecatedWrapper function among a route call's
// arguments after the path: around the handler, among the middleware or inside a function
// literal. The wrapper is a function name, "logDeprecated", which also matches a call written
// with any qualifier, such as mw.logDeprecated(...), or a qualified name, "mw.Deprecated",
// which matches only calls written with exactly that qualifier. Comments and string literals
// call nothing.
func callsWrapper(call *ast.CallExpr, wrapper string) bool {
	qualifier := ""
	name := wrapper
	dot := strings.LastIndex(wrapper, ".")
	if dot >= 0 {
		qualifier = wrapper[:dot]
		name = wrapper[dot+1:]
	}

	found := false
	for _, arg := range call.Args[1:] {
		ast.Inspect(arg, func(node ast.Node) bool {
			inner, ok := node.(*ast.CallExpr)
			if ok && isFunction(inner.Fun, qualifier, name) {
				found = true
			}
			return !found
		})
	}

	return found
}

// isFunction reports an expression that names the function name, through parentheses and type
// arguments: name itself or any x.name when qualifier is empty, and otherwise only x.name
// where x, without parentheses, is written as qualifier ("mw", or "a.mw" for a.mw.name).
func isFunction(expr ast.Expr, qualifier string, name string) bool {
	switch fun := expr.(type) {
	case *ast.ParenExpr:
		return isFunction(fun.X, qualifier, name)
	case *ast.IndexExpr:
		return isFunction(fun.X, qualifier, name)
	case *ast.IndexListExpr:
		return isFunction(fun.X, qualifier, name)
	case *ast.Ident:
		return qualifier == "" && fun.Name == name
	case *ast.SelectorExpr:
		if fun.Sel.Name != name {
			return false
		}
		return qualifier == "" || selectorText(fun.X) == qualifier
	}

	return false
}

// selectorText is a chain of names, a or a.b.c, as written without parentheses; "" for any
// other expression.
func selectorText(expr ast.Expr) string {
	switch x := expr.(type) {
	case *ast.ParenExpr:
		return selectorText(x.X)
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		left := selectorText(x.X)
		if left == "" {
			return ""
		}
		return left + "." + x.Sel.Name
	}

	return ""
}

func (p routeParser) skip(method string, path string) bool {
	if p.cfg.Skip == nil {
		return false
	}

	return p.cfg.Skip(method, toOpenAPIPath(path))
}

// flatNamespace is the first segment of a path relative to its root group:
// "/code_snapshots/:id" is in "code_snapshots". A path starting with a parameter has none.
func flatNamespace(path string) string {
	path = strings.TrimPrefix(path, "/")
	segment, _, _ := strings.Cut(path, "/")
	if strings.HasPrefix(segment, ":") || strings.HasPrefix(segment, "*") {
		return ""
	}

	return segment
}

func lowerFirst(value string) string {
	if value == "" {
		return ""
	}

	runes := []rune(value)
	first := runes[0]
	if first >= 'A' && first <= 'Z' {
		runes[0] = first + ('a' - 'A')
	}

	return string(runes)
}

func ensureNoDuplicateOperations(routes []Route) error {
	seen := map[string]struct{}{}

	for _, r := range routes {
		key := r.Method + " " + r.Path
		_, ok := seen[key]
		if ok {
			return ez.New(ez.EINVALID, "duplicate route: "+key, nil)
		}
		seen[key] = struct{}{}
	}

	return nil
}
