package generator

import (
	"go/ast"
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

// ParseRoutes returns every route declared in the files matched by cfg.RoutesGlob, sorted by
// path then method.
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

type routeParser struct {
	cfg           Config
	nestedHandler *regexp.Regexp
	flatHandler   *regexp.Regexp
}

func newRouteParser(cfg Config) routeParser {
	receiver := regexp.QuoteMeta(cfg.ReceiverVar)

	return routeParser{
		cfg:           cfg,
		nestedHandler: regexp.MustCompile(`\b` + receiver + `\.(\w+)\.(\w+)`),
		flatHandler:   regexp.MustCompile(`\b` + receiver + `\.(\w+)\b`),
	}
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

		handlerRaw := handlerText(fset, file, data, call)

		if p.cfg.DeprecatedWrapper != "" {
			r.Deprecated = callsWrapper(call, p.cfg.DeprecatedWrapper)
		}

		nestedMatch, flatMatch := p.handlerMatches(handlerRaw)
		switch {
		case len(nestedMatch) > 2:
			r.Namespace = lowerFirst(nestedMatch[1])
			r.Handler = nestedMatch[2]
		case len(flatMatch) > 1 && !p.cfg.flat():
			err = ez.New(ez.EINVALID, location+": flat handler reference "+p.cfg.ReceiverVar+"."+flatMatch[1]+" is not allowed with a HandlersFile", nil)
			return false
		case len(flatMatch) > 1:
			r.Namespace = flatNamespace(strings.TrimPrefix(fullPath, rootPaths[receiver]))
			r.Handler = flatMatch[1]
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

// handlerText is the source of a route call's handler argument, the one after the path, for
// the handler lookup. The arguments after it are middleware and are not read, so a route is
// never documented as a middleware method. Comments and the bodies of
// function literals are left out: a function literal is the route's handler itself, so an
// h.Namespace.Method its body calls is not.
func handlerText(fset *token.FileSet, file *ast.File, data []byte, call *ast.CallExpr) string {
	tokenFile := fset.File(call.Pos())
	handler := call.Args[1]
	start := handler.Pos()
	end := handler.End()

	var cuts []ast.Node
	for _, group := range file.Comments {
		for _, comment := range group.List {
			if comment.Pos() >= start && comment.End() <= end {
				cuts = append(cuts, comment)
			}
		}
	}
	ast.Inspect(handler, func(node ast.Node) bool {
		lit, ok := node.(*ast.FuncLit)
		if ok {
			cuts = append(cuts, lit.Body)
			return false
		}
		return true
	})
	sort.Slice(cuts, func(i int, j int) bool {
		return cuts[i].Pos() < cuts[j].Pos()
	})

	var text strings.Builder
	pos := start
	for _, cut := range cuts {
		if cut.Pos() > pos {
			text.Write(data[tokenFile.Offset(pos):tokenFile.Offset(cut.Pos())])
			text.WriteString(" ")
		}
		if cut.End() > pos {
			pos = cut.End()
		}
	}
	text.Write(data[tokenFile.Offset(pos):tokenFile.Offset(end)])

	return text.String()
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

// handlerMatches returns the handler reference of a route call's handler argument as either
// `h.Namespace.Method` (nestedMatch) or `h.Method` (flatMatch). The handler is the first
// reference; a nested one after it in the same argument does not shadow a flat handler
// before it.
func (p routeParser) handlerMatches(handlerRaw string) ([]string, []string) {
	nestedMatch := p.nestedHandler.FindStringSubmatchIndex(handlerRaw)
	flatMatch := p.flatHandler.FindStringSubmatchIndex(handlerRaw)

	if nestedMatch != nil && (flatMatch == nil || nestedMatch[0] <= flatMatch[0]) {
		return []string{
			handlerRaw[nestedMatch[0]:nestedMatch[1]],
			handlerRaw[nestedMatch[2]:nestedMatch[3]],
			handlerRaw[nestedMatch[4]:nestedMatch[5]],
		}, nil
	}
	if flatMatch != nil {
		return nil, []string{
			handlerRaw[flatMatch[0]:flatMatch[1]],
			handlerRaw[flatMatch[2]:flatMatch[3]],
		}
	}

	return nil, nil
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
