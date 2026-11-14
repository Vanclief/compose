package generator

import (
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"go/types"
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

// UnresolvedRoute is a route registration the route finder recognises but cannot resolve, as
// Config.SkipUnresolved receives it. Receiver and Path are the expressions as written, e.g.
// `s.api` and `cfg.HealthPath`, or `"/health"` for a string literal; Path is empty for a route
// method used as a value, `get := e.GET`.
type UnresolvedRoute struct {
	Method   string
	Receiver string
	Path     string
	// File is the route file relative to Config.Root, with forward slashes.
	File string
	Line int
	// Reason says what the finder could not resolve.
	Reason string
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
	var unresolved []string
	for _, abs := range routeFiles {
		built, err := builtGoFile(abs)
		if err != nil {
			return nil, ez.Wrap(err)
		}
		if !built {
			continue
		}

		found, err := parser.parseFile(abs)
		if err != nil {
			return nil, ez.Wrap(err)
		}
		routes = append(routes, found.routes...)
		handlerless = append(handlerless, found.handlerless...)
		unresolved = append(unresolved, found.unresolved...)
	}

	if len(unresolved) > 0 {
		message := "route registrations the generator cannot resolve; make the path a string constant and register the route on a root group, a Group call on one or a variable holding one, or leave them out with SkipUnresolved:\n  " + strings.Join(unresolved, "\n  ")
		return nil, ez.New(ez.EINVALID, message, nil)
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
	// consts holds the constants of the route files' packages, by directory and package name.
	consts map[string]*packageConsts
}

func newRouteParser(cfg Config) *routeParser {
	return &routeParser{
		cfg:    cfg,
		consts: map[string]*packageConsts{},
	}
}

// fileRoutes is what one route file declares: its routes, the "file:line METHOD path" of every
// route with no handler method that Skip does not cover, and the description of every route
// registration that cannot be resolved and that SkipUnresolved does not cover.
type fileRoutes struct {
	routes      []Route
	handlerless []string
	unresolved  []string
}

// parseFile reads the group variables and route calls of one file, statement by statement in
// source order, anywhere in the file, including calls spread over several lines; comments and
// string literals are not code and declare nothing. A route call is
// `group.GET(path, handler, ...)` (or POST, PUT, PATCH, DELETE, HEAD); its path is a string
// constant (see constString) and its group a root group, a group variable or a Group call on a
// group (see groupOf). Group variables follow Go's scopes (see routeWalker).
func (p *routeParser) parseFile(absPath string) (fileRoutes, error) {
	data, err := os.ReadFile(absPath)
	if err != nil {
		return fileRoutes{}, ez.Wrap(err)
	}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, absPath, data, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return fileRoutes{}, ez.New(ez.EINVALID, absPath+": route file is not valid Go", err)
	}

	consts, err := p.packageConsts(filepath.Dir(absPath), file.Name.Name)
	if err != nil {
		return fileRoutes{}, ez.Wrap(err)
	}

	relPath, err := filepath.Rel(p.cfg.Root, absPath)
	if err != nil {
		return fileRoutes{}, ez.Wrap(err)
	}

	w := &routeWalker{
		p:       p,
		fset:    fset,
		absPath: absPath,
		relPath: filepath.ToSlash(relPath),
		consts:  consts,
		imports: map[string]bool{},
	}
	for _, spec := range file.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			return fileRoutes{}, ez.New(ez.EINVALID, absPath+": route file has an invalid import path "+spec.Path.Value, err)
		}
		name := importName(importPath)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		w.imports[name] = true
	}
	for _, decl := range file.Decls {
		w.topLevel(decl)
	}
	if w.err != nil {
		return fileRoutes{}, ez.Wrap(w.err)
	}

	return w.found, nil
}

// groupValue is what a group expression holds: its base path and the base path of the root
// group it descends from, or, when the finder cannot tell, the reason.
type groupValue struct {
	known  bool
	base   string
	root   string
	reason string
}

func rootGroup(base string) groupValue {
	return groupValue{
		known: true,
		base:  base,
		root:  base,
	}
}

func unknownGroup(reason string) groupValue {
	return groupValue{reason: reason}
}

// binding is a name declared in a scope: a variable, with the group it holds, or a constant,
// with its string value.
type binding struct {
	name string
	// key identifies the declaration across the walks of one top-level declaration: its
	// identifier, or the name of a root group.
	key   any
	group groupValue

	isConst bool
	value   string
	valueOK bool

	// frames and funcDepth are the number of frames and of function literals open where the
	// name is declared.
	frames    int
	funcDepth int
	// lost, when not "", is why the variable holds no known group from some point on, whatever is
	// assigned to it later: it may be set through a pointer, or by a function literal, at any
	// time.
	lost string
}

type scope struct {
	parent *scope
	names  map[string]*binding
}

// frame is a branch, a loop body or a function literal body being walked. It records the
// bindings declared outside it that it assigns.
type frame struct {
	// funcDepth is the number of function literals open in the frame, the one whose body it
	// is included.
	funcDepth int
	// jumps marks a frame that a break, continue, goto or fallthrough, or a return from a
	// function literal, may leave before its end.
	jumps    bool
	assigned map[*binding]*frameAssignment
}

// frameAssignment is what a frame did to a binding declared outside it: the group it held
// before the frame and at its end, whether the frame may be left early, and where the frame
// first assigned it.
type frameAssignment struct {
	before   groupValue
	after    groupValue
	jumped   bool
	location string
}

// changed reports that the binding may hold another group than before when the frame is left.
func (a *frameAssignment) changed() bool {
	return a.jumped || a.after != a.before
}

// routeWalker reads the group variables and route calls of one route file, one top-level
// declaration at a time.
//
// Group variables follow Go's scopes: a name declared in a block, an if, for, switch or
// select, a case or a function literal is not seen outside it, and an inner declaration
// shadows an outer one. The RootGroups names are known in every declaration, and a parameter
// of such a name is that root group, as is a variable of such a name declared without a value
// or set to the result of a call other than Group, such as echo.New(); a named result, any
// other parameter, and a variable set to anything but a group, holds no group the finder knows.
// In straight-line code `=` sets a variable's group from that statement on, all right-hand
// sides read first. A variable that a branch, a loop or a function literal sets to another
// group holds no known group after it, nor anywhere in a loop or literal that does so, since
// another iteration or call may have done it before, unless every branch of an if with an else,
// or of a switch or select with a default, sets it to the same known group; one that such a body
// assigns at all holds none after it when the body may be left early, by a break, continue, goto
// or fallthrough, or a return from a function literal; and a case reached by fallthrough starts
// with none in what the case before it assigned. A function literal sees no known group in a
// variable of an enclosing function that `=` assigns anywhere in the top-level declaration,
// since the literal may run after any assignment, and neither does any code of a declaration
// that has a goto. A variable of an enclosing function that a function literal assigns, whose
// address is taken, or that is written through as `*g = ...` holds no known group from there on.
type routeWalker struct {
	p       *routeParser
	fset    *token.FileSet
	absPath string
	// relPath is the file relative to Config.Root, with forward slashes.
	relPath string
	consts  *packageConsts

	scope     *scope
	frames    []*frame
	funcDepth int
	// quiet counts the walks that record no route: the first walk of a loop or function
	// literal body, and the first walk of a top-level declaration, which fills reassigned.
	quiet int
	// reassigned holds the keys of the bindings `=` assigns anywhere in the top-level
	// declaration.
	reassigned map[any]bool
	// gotos reports a goto anywhere in the top-level declaration.
	gotos bool
	// imports holds the names the file's imports are referred to by.
	imports map[string]bool

	found fileRoutes
	err   error
}

// topLevel walks a top-level declaration twice: once to learn which variables it reassigns,
// then to read its routes.
func (w *routeWalker) topLevel(decl ast.Decl) {
	w.reassigned = map[any]bool{}
	w.gotos = false
	ast.Inspect(decl, func(node ast.Node) bool {
		branch, ok := node.(*ast.BranchStmt)
		if ok && branch.Tok == token.GOTO {
			w.gotos = true
		}
		return !w.gotos
	})

	w.quiet++
	w.declaration(decl)
	w.quiet--
	w.declaration(decl)
}

func (w *routeWalker) declaration(decl ast.Decl) {
	w.scope = &scope{names: map[string]*binding{}}
	w.frames = nil
	w.funcDepth = 0
	for name, base := range w.p.cfg.RootGroups {
		w.scope.names[name] = &binding{
			name:  name,
			key:   "root " + name,
			group: rootGroup(base),
		}
	}

	switch d := decl.(type) {
	case *ast.FuncDecl:
		if d.Body == nil {
			return
		}
		w.push()
		w.params(d.Recv)
		w.params(d.Type.Params)
		w.results(d.Type.Results)
		w.stmts(d.Body.List)
		w.pop()
	case *ast.GenDecl:
		if d.Tok != token.VAR {
			return
		}
		for _, spec := range d.Specs {
			valueSpec, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, value := range valueSpec.Values {
				w.expr(value)
			}
		}
	}
}

func (w *routeWalker) push() {
	w.scope = &scope{
		parent: w.scope,
		names:  map[string]*binding{},
	}
}

func (w *routeWalker) pop() {
	w.scope = w.scope.parent
}

// declare adds a name to the innermost scope.
func (w *routeWalker) declare(name *ast.Ident, group groupValue) *binding {
	b := &binding{
		name:      name.Name,
		key:       name,
		group:     group,
		frames:    len(w.frames),
		funcDepth: w.funcDepth,
	}
	if name.Name != "_" {
		w.scope.names[name.Name] = b
	}

	return b
}

func (w *routeWalker) lookup(name string) *binding {
	for s := w.scope; s != nil; s = s.parent {
		b, ok := s.names[name]
		if ok {
			return b
		}
	}

	return nil
}

// assign sets the group a binding holds, recording the assignment in every frame open since
// the binding was declared. A binding of an enclosing function that a function literal assigns
// holds no known group from there on: the literal runs where it is called.
func (w *routeWalker) assign(b *binding, group groupValue, pos token.Pos) {
	w.reassigned[b.key] = true
	for _, f := range w.frames[b.frames:] {
		_, ok := f.assigned[b]
		if !ok {
			f.assigned[b] = &frameAssignment{
				before:   b.group,
				location: w.location(pos),
			}
		}
	}
	b.group = group
	if w.funcDepth > b.funcDepth && b.lost == "" {
		b.lost = strconv.Quote(b.name) + " is assigned at " + w.location(pos) + " in a function literal, which may run at any time, so the group it holds is not known"
	}
}

// lose leaves a binding with no known group from pos on, whatever is assigned to it later.
func (w *routeWalker) lose(b *binding, reason string, pos token.Pos) {
	if b.lost == "" {
		b.lost = reason
	}
	w.assign(b, unknownGroup(reason), pos)
}

// rootFallback is the group a variable named name holds when it is a parameter, is declared
// without a value or is set to the result of a call other than Group: a name in RootGroups is
// that root group, such as e in `e := echo.New()`.
func (w *routeWalker) rootFallback(name string, group groupValue) groupValue {
	base, ok := w.p.cfg.RootGroups[name]
	if !ok || group.known {
		return group
	}

	return rootGroup(base)
}

// isOtherCall reports a call of anything but Group, such as echo.New().
func isOtherCall(expr ast.Expr) bool {
	call, ok := ast.Unparen(expr).(*ast.CallExpr)
	if !ok {
		return false
	}
	selector, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)

	return !ok || selector.Sel.Name != "Group"
}

// identName is the name of an identifier, through parentheses, "" for any other expression.
func identName(expr ast.Expr) string {
	name, ok := ast.Unparen(expr).(*ast.Ident)
	if !ok {
		return ""
	}

	return name.Name
}

// params declares the parameters or receiver of a function.
func (w *routeWalker) params(fields *ast.FieldList) {
	if fields == nil {
		return
	}

	for _, field := range fields.List {
		for _, name := range field.Names {
			group := unknownGroup("unknown route group " + strconv.Quote(name.Name) + ", a function parameter not named in RootGroups")
			w.declare(name, w.rootFallback(name.Name, group))
		}
	}
}

// results declares the named results of a function, which hold no group until assigned.
func (w *routeWalker) results(fields *ast.FieldList) {
	if fields == nil {
		return
	}

	for _, field := range fields.List {
		for _, name := range field.Names {
			w.declare(name, unknownGroup(strconv.Quote(name.Name)+" is a named result"))
		}
	}
}

// jump marks the frames of the current function as possibly left before their end.
func (w *routeWalker) jump() {
	for _, f := range w.frames {
		if f.funcDepth == w.funcDepth {
			f.jumps = true
		}
	}
}

// throughPointer reads `&x` and `*x = ...`: a variable whose address is taken, or that holds a
// group written through, may be set through a pointer at any time, so it holds no known group
// from there on.
func (w *routeWalker) throughPointer(expr ast.Expr, pos token.Pos) {
	name, ok := ast.Unparen(expr).(*ast.Ident)
	if !ok {
		return
	}
	b := w.lookup(name.Name)
	if b == nil || b.isConst {
		return
	}

	w.lose(b, strconv.Quote(b.name)+" may be set through a pointer from "+w.location(pos)+" on, so the group it holds is not known", pos)
}

// inFrame walks body as a frame and returns what it did to the bindings declared outside it,
// each of which holds again the group it held before.
func (w *routeWalker) inFrame(body func()) map[*binding]*frameAssignment {
	f := &frame{
		funcDepth: w.funcDepth,
		assigned:  map[*binding]*frameAssignment{},
	}
	w.frames = append(w.frames, f)
	body()
	w.frames = w.frames[:len(w.frames)-1]

	for b, assignment := range f.assigned {
		assignment.after = b.group
		assignment.jumped = f.jumps
		b.group = assignment.before
	}

	return f.assigned
}

// forget leaves the bindings that a loop or function literal may have left holding another
// group than before it with no known group.
func (w *routeWalker) forget(assigned map[*binding]*frameAssignment) {
	for b, assignment := range assigned {
		if assignment.changed() {
			forgetAt(b, assignment.location)
		}
	}
}

func forgetAt(b *binding, location string) {
	b.group = unknownGroup(strconv.Quote(b.name) + " is reassigned at " + location + " in a branch, a loop or a function literal, so the group it holds here is not known")
}

// branches walks the alternatives of an if, each from the groups held before it; exhaustive
// reports an else.
func (w *routeWalker) branches(exhaustive bool, bodies ...func()) {
	var alternatives []map[*binding]*frameAssignment
	for _, body := range bodies {
		alternatives = append(alternatives, w.inFrame(body))
	}
	w.join(exhaustive, alternatives)
}

// join sets what the alternatives of an if, switch or select, each walked by inFrame, leave in
// the bindings they assign. When they cover every case (exhaustive), none may be left early and
// each leaves a binding holding the same known group, it holds that group after them; otherwise
// one that an alternative may have changed holds no known group.
func (w *routeWalker) join(exhaustive bool, alternatives []map[*binding]*frameAssignment) {
	assigned := map[*binding]bool{}
	for _, alternative := range alternatives {
		for b := range alternative {
			assigned[b] = true
		}
	}

	for b := range assigned {
		group, same := commonGroup(b, alternatives)
		if exhaustive && same {
			b.group = group
			continue
		}
		for _, alternative := range alternatives {
			assignment, ok := alternative[b]
			if ok && assignment.changed() {
				forgetAt(b, assignment.location)
				break
			}
		}
	}
}

// commonGroup is the known group every alternative leaves b holding, if there is one and none
// may be left early. b holds the group from before the alternatives.
func commonGroup(b *binding, alternatives []map[*binding]*frameAssignment) (groupValue, bool) {
	var common groupValue
	for i, alternative := range alternatives {
		end := b.group
		assignment, ok := alternative[b]
		if ok {
			if assignment.jumped {
				return groupValue{}, false
			}
			end = assignment.after
		}
		if !end.known || (i > 0 && end != common) {
			return groupValue{}, false
		}
		common = end
	}

	return common, len(alternatives) > 0
}

// loop walks a body that may run any number of times: first quietly, to learn what it
// assigns, then with everything it assigns holding no known group from its start. A
// variable the body leaves as it found it is forgotten too: a later run may find in it
// what an earlier run left in another variable.
func (w *routeWalker) loop(body func()) {
	w.quiet++
	first := w.inFrame(body)
	w.quiet--

	for b, assignment := range first {
		forgetAt(b, assignment.location)
	}
	second := w.inFrame(body)
	w.forget(second)
}

func (w *routeWalker) stmts(list []ast.Stmt) {
	for _, stmt := range list {
		w.stmt(stmt)
	}
}

func (w *routeWalker) block(list []ast.Stmt) {
	w.push()
	w.stmts(list)
	w.pop()
}

func (w *routeWalker) stmt(stmt ast.Stmt) {
	switch s := stmt.(type) {
	case *ast.BlockStmt:
		w.block(s.List)
	case *ast.AssignStmt:
		w.assignStmt(s)
	case *ast.DeclStmt:
		w.declStmt(s)
	case *ast.ExprStmt:
		w.expr(s.X)
	case *ast.GoStmt:
		w.expr(s.Call)
	case *ast.DeferStmt:
		w.expr(s.Call)
	case *ast.ReturnStmt:
		for _, result := range s.Results {
			w.expr(result)
		}
		if w.funcDepth > 0 {
			w.jump()
		}
	case *ast.BranchStmt:
		w.jump()
	case *ast.IncDecStmt:
		w.expr(s.X)
	case *ast.SendStmt:
		w.expr(s.Chan)
		w.expr(s.Value)
	case *ast.LabeledStmt:
		w.stmt(s.Stmt)
	case *ast.IfStmt:
		w.push()
		if s.Init != nil {
			w.stmt(s.Init)
		}
		w.expr(s.Cond)
		bodies := []func(){
			func() {
				w.block(s.Body.List)
			},
		}
		if s.Else != nil {
			bodies = append(bodies, func() {
				w.stmt(s.Else)
			})
		}
		w.branches(s.Else != nil, bodies...)
		w.pop()
	case *ast.ForStmt:
		w.push()
		if s.Init != nil {
			w.stmt(s.Init)
		}
		w.loop(func() {
			w.expr(s.Cond)
			w.block(s.Body.List)
			if s.Post != nil {
				w.stmt(s.Post)
			}
		})
		w.pop()
	case *ast.RangeStmt:
		w.expr(s.X)
		w.loop(func() {
			w.push()
			for _, expr := range []ast.Expr{s.Key, s.Value} {
				if expr == nil {
					continue
				}
				group := unknownGroup(types.ExprString(expr) + " is a range variable")
				w.setVariable(expr, s.Tok, group, s.Pos())
			}
			w.block(s.Body.List)
			w.pop()
		})
	case *ast.SwitchStmt:
		w.push()
		if s.Init != nil {
			w.stmt(s.Init)
		}
		w.expr(s.Tag)
		w.clauses(s.Body)
		w.pop()
	case *ast.TypeSwitchStmt:
		w.push()
		if s.Init != nil {
			w.stmt(s.Init)
		}
		w.stmt(s.Assign)
		w.clauses(s.Body)
		w.pop()
	case *ast.SelectStmt:
		w.clauses(s.Body)
	}
}

// clauses walks the case clauses of a switch or select as branches, which cover every case when
// one is the default. A clause reached by fallthrough starts with what the clause before it
// assigned holding no known group.
func (w *routeWalker) clauses(body *ast.BlockStmt) {
	var alternatives []map[*binding]*frameAssignment
	exhaustive := false
	var fallen map[*binding]*frameAssignment
	for _, stmt := range body.List {
		var list []ast.Expr
		var comm ast.Stmt
		var stmts []ast.Stmt
		switch clause := stmt.(type) {
		case *ast.CaseClause:
			list = clause.List
			stmts = clause.Body
			exhaustive = exhaustive || clause.List == nil
		case *ast.CommClause:
			comm = clause.Comm
			stmts = clause.Body
			exhaustive = exhaustive || clause.Comm == nil
		default:
			continue
		}

		previous := fallen
		assigned := w.inFrame(func() {
			for b, assignment := range previous {
				w.assign(b, unknownGroup(strconv.Quote(b.name)+" is reassigned at "+assignment.location+" in a case that falls through to this one, so the group it holds here is not known"), stmt.Pos())
			}
			w.push()
			for _, expr := range list {
				w.expr(expr)
			}
			if comm != nil {
				w.stmt(comm)
			}
			w.stmts(stmts)
			w.pop()
		})
		alternatives = append(alternatives, assigned)

		fallen = nil
		if fallsThrough(stmts) {
			fallen = assigned
		}
	}
	w.join(exhaustive, alternatives)
}

// fallsThrough reports a case clause body that ends in fallthrough.
func fallsThrough(stmts []ast.Stmt) bool {
	if len(stmts) == 0 {
		return false
	}
	branch, ok := stmts[len(stmts)-1].(*ast.BranchStmt)

	return ok && branch.Tok == token.FALLTHROUGH
}

// assignStmt reads `x := value` and `x = value`: x holds the group value holds, and no known
// group when value is not one or is one of several results of a call. Every value is read
// before any variable is set, as Go evaluates them.
func (w *routeWalker) assignStmt(s *ast.AssignStmt) {
	for _, rhs := range s.Rhs {
		w.expr(rhs)
	}
	for _, lhs := range s.Lhs {
		if identName(lhs) == "" {
			w.expr(lhs)
		}
		star, ok := ast.Unparen(lhs).(*ast.StarExpr)
		if ok {
			w.throughPointer(star.X, s.Pos())
		}
	}

	location := w.location(s.Pos())
	groups := make([]groupValue, len(s.Lhs))
	for i, lhs := range s.Lhs {
		groups[i] = unknownGroup(types.ExprString(lhs) + " is assigned at " + location + " from something other than a group")
		if len(s.Lhs) == len(s.Rhs) && (s.Tok == token.DEFINE || s.Tok == token.ASSIGN) {
			groups[i] = w.groupOf(s.Rhs[i])
			if isOtherCall(s.Rhs[i]) {
				groups[i] = w.rootFallback(identName(lhs), groups[i])
			}
		}
	}
	for i, lhs := range s.Lhs {
		w.setVariable(lhs, s.Tok, groups[i], s.Pos())
	}
}

// setVariable declares or assigns a variable written as an identifier, through parentheses:
// tok is token.DEFINE for `:=`, which assigns a name already declared in the same scope, or any
// assignment operator.
func (w *routeWalker) setVariable(expr ast.Expr, tok token.Token, group groupValue, pos token.Pos) {
	name, ok := ast.Unparen(expr).(*ast.Ident)
	if !ok || name.Name == "_" {
		return
	}

	if tok == token.DEFINE {
		b, ok := w.scope.names[name.Name]
		if !ok {
			w.declare(name, group)
			return
		}
		w.assign(b, group, pos)
		return
	}

	b := w.lookup(name.Name)
	if b != nil {
		w.assign(b, group, pos)
	}
}

// declStmt reads a const or var declaration in a function. A constant's value is read where
// it is declared; a spec without values repeats the values of the one before it.
func (w *routeWalker) declStmt(s *ast.DeclStmt) {
	gen, ok := s.Decl.(*ast.GenDecl)
	if !ok || (gen.Tok != token.CONST && gen.Tok != token.VAR) {
		return
	}

	var constValues []ast.Expr
	for _, spec := range gen.Specs {
		valueSpec, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		for _, value := range valueSpec.Values {
			w.expr(value)
		}

		if gen.Tok == token.CONST {
			if len(valueSpec.Values) > 0 {
				constValues = valueSpec.Values
			}
			values := make([]constValue, len(valueSpec.Names))
			for i := range valueSpec.Names {
				if i < len(constValues) {
					values[i].value, values[i].ok = w.constString(constValues[i])
				}
			}
			for i, name := range valueSpec.Names {
				b := w.declare(name, unknownGroup(strconv.Quote(name.Name)+" is a constant"))
				b.isConst = true
				b.value = values[i].value
				b.valueOK = values[i].ok
			}
			continue
		}

		location := w.location(valueSpec.Pos())
		groups := make([]groupValue, len(valueSpec.Names))
		for i, name := range valueSpec.Names {
			groups[i] = unknownGroup(strconv.Quote(name.Name) + " is declared at " + location + " without a group")
			switch {
			case len(valueSpec.Values) == 0:
				groups[i] = w.rootFallback(name.Name, groups[i])
			case len(valueSpec.Values) == len(valueSpec.Names):
				groups[i] = w.groupOf(valueSpec.Values[i])
				if isOtherCall(valueSpec.Values[i]) {
					groups[i] = w.rootFallback(name.Name, groups[i])
				}
			}
		}
		for i, name := range valueSpec.Names {
			w.declare(name, groups[i])
		}
	}
}

// expr reads the route calls, route methods used as values, addresses taken and function
// literals of an expression.
func (w *routeWalker) expr(expr ast.Expr) {
	if expr == nil {
		return
	}

	called := map[ast.Expr]bool{}
	ast.Inspect(expr, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.FuncLit:
			w.funcLit(n)
			return false
		case *ast.CallExpr:
			called[ast.Unparen(n.Fun)] = true
			w.call(n)
		case *ast.SelectorExpr:
			if !called[n] {
				w.methodValue(n)
			}
		case *ast.UnaryExpr:
			if n.Op == token.AND {
				w.throughPointer(n.X, n.Pos())
			}
		}
		return true
	})
}

// funcLit walks a function literal's body as a loop: it may run any number of times.
func (w *routeWalker) funcLit(lit *ast.FuncLit) {
	w.funcDepth++
	w.loop(func() {
		w.push()
		w.params(lit.Type.Params)
		w.results(lit.Type.Results)
		w.stmts(lit.Body.List)
		w.pop()
	})
	w.funcDepth--
}

// location is where pos is, for a reason: the file relative to Config.Root and the line.
func (w *routeWalker) location(pos token.Pos) string {
	return w.relPath + ":" + strconv.Itoa(w.fset.Position(pos).Line)
}

// groupOf is the group an expression holds: a root group or a group variable, or a call
// `parent.Group(prefix, ...)` on a group, chained as deep as written, whose prefix is a string
// constant.
func (w *routeWalker) groupOf(expr ast.Expr) groupValue {
	switch x := expr.(type) {
	case *ast.ParenExpr:
		return w.groupOf(x.X)
	case *ast.Ident:
		b := w.lookup(x.Name)
		if b == nil {
			return unknownGroup("unknown route group " + strconv.Quote(x.Name))
		}
		if b.lost != "" {
			return unknownGroup(b.lost)
		}
		if w.gotos && w.reassigned[b.key] {
			return unknownGroup(strconv.Quote(x.Name) + " is reassigned in a function that has a goto, so the group it holds here is not known")
		}
		if b.funcDepth < w.funcDepth && w.reassigned[b.key] {
			return unknownGroup(strconv.Quote(x.Name) + " is used in a function literal and reassigned in its function, so the group it holds when the literal runs is not known")
		}
		return b.group
	case *ast.CallExpr:
		selector, ok := x.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "Group" || len(x.Args) == 0 {
			break
		}
		parent := w.groupOf(selector.X)
		if !parent.known {
			return parent
		}
		prefix, ok := w.constString(x.Args[0])
		if !ok {
			return unknownGroup("group prefix " + types.ExprString(x.Args[0]) + " is not a string constant")
		}
		return groupValue{
			known: true,
			base:  parent.base + prefix,
			root:  parent.root,
		}
	}

	return unknownGroup(types.ExprString(expr) + " is not a group the route finder can follow")
}

// constString is the value of a string constant expression: string literals, constants
// declared in the function or at the top level of the file's package, joined with +. A
// variable or parameter shadows a package constant of the same name.
func (w *routeWalker) constString(expr ast.Expr) (string, bool) {
	return foldString(expr, func(name string) (string, bool) {
		b := w.lookup(name)
		if b != nil {
			return b.value, b.isConst && b.valueOK
		}
		return w.consts.value(name)
	})
}

// call reads a route call: `group.METHOD(path, handler, ...)` with an HTTP method Echo
// registers that way and at least one argument after the path, or with one argument that is
// a call, which is not resolved. A quiet walk records nothing.
func (w *routeWalker) call(call *ast.CallExpr) {
	if w.quiet > 0 || w.err != nil {
		return
	}

	selector, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
	if !ok {
		return
	}

	method := selector.Sel.Name
	switch method {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD":
	default:
		return
	}

	lineNo := w.fset.Position(call.Pos()).Line
	location := w.absPath + ":" + strconv.Itoa(lineNo)

	// A single argument that is a call may return the path and the handler together.
	if len(call.Args) == 1 {
		_, spread := ast.Unparen(call.Args[0]).(*ast.CallExpr)
		if !spread {
			return
		}
		unresolved := UnresolvedRoute{
			Method:   method,
			Receiver: types.ExprString(selector.X),
			Path:     types.ExprString(call.Args[0]),
			File:     w.relPath,
			Line:     lineNo,
			Reason:   "the path and the handler come from the call " + types.ExprString(call.Args[0]) + ", so they are not known",
		}
		w.unresolved(unresolved, unresolved.Receiver+"."+method+"("+unresolved.Path+")")
		return
	}
	if len(call.Args) < 2 {
		return
	}

	group := w.groupOf(selector.X)
	suffix, pathKnown := w.constString(call.Args[0])
	if !group.known || !pathKnown {
		var reasons []string
		if !group.known {
			reasons = append(reasons, group.reason)
		}
		if !pathKnown {
			reasons = append(reasons, "path "+types.ExprString(call.Args[0])+" is not a string constant")
		}
		unresolved := UnresolvedRoute{
			Method:   method,
			Receiver: types.ExprString(selector.X),
			Path:     types.ExprString(call.Args[0]),
			File:     w.relPath,
			Line:     lineNo,
			Reason:   strings.Join(reasons, "; "),
		}
		w.unresolved(unresolved, unresolved.Receiver+"."+method+"("+unresolved.Path+", ...)")
		return
	}

	fullPath := group.base + suffix

	r := Route{
		Method:     method,
		Path:       fullPath,
		SourceFile: filepath.ToSlash(w.absPath),
		SourceLine: lineNo,
		flat:       w.p.cfg.flat(),
	}

	if w.p.cfg.DeprecatedWrapper != "" {
		r.Deprecated = callsWrapper(call, w.p.cfg.DeprecatedWrapper)
	}

	names := handlerReference(call.Args[1], w.p.cfg.ReceiverVar)
	switch {
	case len(names) > 1:
		r.Namespace = lowerFirst(names[0])
		r.Handler = names[1]
	case len(names) == 1 && !w.p.cfg.flat():
		w.err = ez.New(ez.EINVALID, location+": flat handler reference "+w.p.cfg.ReceiverVar+"."+names[0]+" is not allowed with a HandlersFile", nil)
		return
	case len(names) == 1:
		r.Namespace = flatNamespace(strings.TrimPrefix(fullPath, group.root))
		r.Handler = names[0]
	}

	if r.Handler == "" && !w.p.skip(method, fullPath) {
		w.found.handlerless = append(w.found.handlerless, location+" "+method+" "+fullPath)
	}

	w.found.routes = append(w.found.routes, r)
}

// methodValue reads a route method used as a value, `get := e.GET` or `register(e.POST, ...)`:
// the routes registered through it cannot be known. A selector on an import, such as a method
// name constant `echo.GET`, is not one. A quiet walk records nothing.
func (w *routeWalker) methodValue(selector *ast.SelectorExpr) {
	if w.quiet > 0 || w.err != nil {
		return
	}

	method := selector.Sel.Name
	switch method {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD":
	default:
		return
	}

	x, ok := selector.X.(*ast.Ident)
	if ok && w.imports[x.Name] && w.lookup(x.Name) == nil {
		return
	}

	unresolved := UnresolvedRoute{
		Method:   method,
		Receiver: types.ExprString(selector.X),
		File:     w.relPath,
		Line:     w.fset.Position(selector.Pos()).Line,
		Reason:   "the method " + method + " is used as a value, not called, so the routes registered through it are not known",
	}
	w.unresolved(unresolved, unresolved.Receiver+"."+method)
}

// unresolved records a route registration that cannot be resolved, written as call, unless
// SkipUnresolved leaves it out.
func (w *routeWalker) unresolved(route UnresolvedRoute, call string) {
	if w.p.cfg.SkipUnresolved != nil && w.p.cfg.SkipUnresolved(route) {
		return
	}

	location := w.absPath + ":" + strconv.Itoa(route.Line)
	w.found.unresolved = append(w.found.unresolved, location+": "+call+": "+route.Reason)
}

// packageConsts are the constants declared at the top level of a package, by name, with their
// string values once read.
type packageConsts struct {
	exprs  map[string]ast.Expr
	values map[string]constValue
	// reading holds the constants whose value is being read, so a cycle ends.
	reading map[string]bool
}

type constValue struct {
	value string
	ok    bool
}

// packageConsts returns the top-level constants of the package name in dir, read from the
// files a default build compiles.
func (p *routeParser) packageConsts(dir string, name string) (*packageConsts, error) {
	key := dir + "\x00" + name
	consts, ok := p.consts[key]
	if ok {
		return consts, nil
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, ez.Wrap(err)
	}

	consts = &packageConsts{
		exprs:   map[string]ast.Expr{},
		values:  map[string]constValue{},
		reading: map[string]bool{},
	}
	fset := token.NewFileSet()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		built, err := builtGoFile(path)
		if err != nil {
			return nil, ez.Wrap(err)
		}
		if !built {
			continue
		}

		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, ez.New(ez.EINVALID, path+": file of a route file's package is not valid Go", err)
		}
		if file.Name.Name != name {
			continue
		}

		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			addConstExprs(gen, consts.exprs)
		}
	}

	p.consts[key] = consts

	return consts, nil
}

// addConstExprs adds the value expression of every constant of a const declaration to exprs. A
// spec without values repeats the values of the one before it.
func addConstExprs(gen *ast.GenDecl, exprs map[string]ast.Expr) {
	var values []ast.Expr
	for _, spec := range gen.Specs {
		valueSpec, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		if len(valueSpec.Values) > 0 {
			values = valueSpec.Values
		}
		for i, ident := range valueSpec.Names {
			if i < len(values) {
				exprs[ident.Name] = values[i]
			}
		}
	}
}

// value is the string value of the package constant name.
func (c *packageConsts) value(name string) (string, bool) {
	known, ok := c.values[name]
	if ok {
		return known.value, known.ok
	}

	expr, ok := c.exprs[name]
	if !ok || c.reading[name] {
		return "", false
	}

	c.reading[name] = true
	value, ok := foldString(expr, c.value)
	delete(c.reading, name)
	c.values[name] = constValue{
		value: value,
		ok:    ok,
	}

	return value, ok
}

// foldString is the value of a string constant expression: string literals and the constants
// lookup knows, in parentheses or joined with +.
func foldString(expr ast.Expr, lookup func(name string) (string, bool)) (string, bool) {
	switch x := expr.(type) {
	case *ast.BasicLit:
		if x.Kind != token.STRING {
			return "", false
		}
		value, err := strconv.Unquote(x.Value)
		if err != nil {
			return "", false
		}
		return value, true
	case *ast.ParenExpr:
		return foldString(x.X, lookup)
	case *ast.BinaryExpr:
		if x.Op != token.ADD {
			return "", false
		}
		left, ok := foldString(x.X, lookup)
		if !ok {
			return "", false
		}
		right, ok := foldString(x.Y, lookup)
		if !ok {
			return "", false
		}
		return left + right, true
	case *ast.Ident:
		return lookup(x.Name)
	}

	return "", false
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

// receiverChain returns the names after the receiver variable in a selector chain, through
// parentheses: [A, B] for h.A.B, and for s.h.A.B, where the receiver is a field. The leftmost
// receiver name with a name after it counts.
func receiverChain(expr ast.Expr, receiver string) ([]string, bool) {
	var names []string
	for {
		switch x := expr.(type) {
		case *ast.ParenExpr:
			expr = x.X
			continue
		case *ast.SelectorExpr:
			names = append([]string{x.Sel.Name}, names...)
			expr = x.X
			continue
		case *ast.Ident:
			names = append([]string{x.Name}, names...)
		}
		break
	}

	for i, name := range names[:len(names)-1] {
		if name == receiver {
			return names[i+1:], true
		}
	}

	return nil, false
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

func (p *routeParser) skip(method string, path string) bool {
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
