package generator

import (
	"errors"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/vanclief/ez"
)

const schemaRefPrefix = "#/components/schemas/"

type typeRef struct {
	ImportPath string
	Name       string
}

type handlerSchema struct {
	Request  map[string]any
	Response map[string]any
	// PathFields are the request's JSON fields filled from path parameters, left out of the
	// request body.
	PathFields []string
	// ModuleDoc is the doc comment of the module function handling the request.
	ModuleDoc string
}

type packageInfo struct {
	ImportPath string
	Dir        string
	Types      map[string]typeSpecInfo
	Funcs      []funcInfo
	// Enums holds, per closed-set string type (one with an enums.Set declaration),
	// the accepted typed constants in declaration order.
	Enums map[string][]string
	// EnumDocs holds, per type in Enums, each value's description (see constDoc), "" for an
	// undocumented one.
	EnumDocs map[string][]string
	// EnumNames holds, per type in Enums, the name of each value's constant.
	EnumNames map[string][]string
	// Methods holds, per receiver type name, the names of its methods (value or pointer
	// receiver).
	Methods map[string]map[string]bool
	// Consts holds the value of every package-level constant declared with a string literal.
	Consts map[string]string
	// Doc is the package's doc comment.
	Doc string
}

type stringConst struct {
	Name  string
	Value string
	Doc   string
}

type typeSpecInfo struct {
	Spec    *ast.TypeSpec
	Imports map[string]string
	// Doc is the type's doc comment, on the spec or, for a lone `type X ...`, on its declaration.
	Doc *ast.CommentGroup
}

type funcInfo struct {
	Decl    *ast.FuncDecl
	Imports map[string]string
}

type schemaBuilder struct {
	modulePath     string
	moduleVersions map[string]string
	repoRoot       string
	modCache       string
	packages       map[string]*packageInfo
	// packageNames caches, per import path, the name an unnamed import of it is referred to by
	// (see packageName).
	packageNames map[string]string
	schemas      map[string]map[string]any
	building     map[typeRef]bool
	// Types emitted as opaque objects because their source could not be read. Silently
	// dropping their fields would publish a broken contract, so Generate refuses to return a
	// document while this is non-empty.
	unresolved []string

	requiredPackagePrefix string
	typeSchemas           map[string]map[string]any
	prefixCollapse        map[string]string
	nameAliases           map[string]string
	docComments           bool
	// origins maps each component schema to the Go type it was built from.
	origins map[string]typeRef
	// variantOf maps each request variant (RequestVariantSuffix) to the component it copies.
	variantOf map[string]string
	// derivedRequired holds a copy of every required list the value-typed, non-omitempty
	// rule produced, keyed by the address of its first name (see markDerived).
	derivedRequired map[*string][]string
	// namer is Config.SchemaName.
	namer func(importPath string, typeName string) string
	// named maps each component name handed out to the type it names.
	named map[string]typeRef
	// collisions are the names handed out to several types.
	collisions []string
	// promoted memoizes promotedMarshaler.
	promoted map[promotedKey]string
	// standardLibrary caches the standard-library packages read from GOROOT
	// (loadStandardLibrary).
	standardLibrary map[string]*packageInfo
	// goroot is GOROOT once looked up (gorootDir), gorootErr why it could not be.
	goroot    string
	gorootErr error
}

func newSchemaBuilder(cfg Config) (*schemaBuilder, error) {
	modulePath, moduleVersions, err := readModuleInfo(filepath.Join(cfg.Root, "go.mod"))
	if err != nil {
		return nil, ez.Wrap(err)
	}

	modCache := os.Getenv("GOMODCACHE")
	if modCache == "" {
		gopath := os.Getenv("GOPATH")
		if gopath != "" {
			modCache = filepath.Join(gopath, "pkg", "mod")
		}
	}
	if modCache == "" {
		goModCache, err := goEnv("GOMODCACHE")
		if err == nil && goModCache != "" {
			modCache = goModCache
		}
	}
	if modCache == "" {
		goPath, err := goEnv("GOPATH")
		if err == nil && goPath != "" {
			modCache = filepath.Join(goPath, "pkg", "mod")
		}
	}

	return &schemaBuilder{
		modulePath:            modulePath,
		moduleVersions:        moduleVersions,
		repoRoot:              cfg.Root,
		modCache:              modCache,
		packages:              map[string]*packageInfo{},
		packageNames:          map[string]string{},
		schemas:               map[string]map[string]any{},
		building:              map[typeRef]bool{},
		requiredPackagePrefix: cfg.RequiredPackagePrefix,
		typeSchemas:           cfg.TypeSchemas,
		prefixCollapse:        cfg.SchemaPrefixCollapse,
		nameAliases:           cfg.SchemaNameAliases,
		docComments:           cfg.DocComments,
		origins:               map[string]typeRef{},
		variantOf:             map[string]string{},
		derivedRequired:       map[*string][]string{},
		namer:                 cfg.SchemaName,
		named:                 map[string]typeRef{},
	}, nil
}

// goEnv is what `go env name` prints. Its error says why the command failed, with what it wrote
// to standard error.
func goEnv(name string) (string, error) {
	out, err := exec.Command("go", "env", name).Output()
	if err != nil {
		detail := err.Error()
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			detail = strings.TrimSpace(string(exitErr.Stderr))
		}
		return "", ez.New(ez.EINTERNAL, "`go env "+name+"` failed: "+detail, err)
	}

	return strings.TrimSpace(string(out)), nil
}

// localVariableTypes maps each local variable of a function body to the type it is declared
// with or assigned from, so a later reference to the variable resolves back to that type.
func localVariableTypes(body *ast.BlockStmt, imports map[string]string, pkgPath string, helpers map[string]funcInfo) map[string]typeRef {
	varTypes := map[string]typeRef{}

	ast.Inspect(body, func(node ast.Node) bool {
		assign, ok := node.(*ast.AssignStmt)
		if ok {
			// `v, err := helper(...)`: v takes the helper's first result.
			if len(assign.Rhs) == 1 && len(assign.Lhs) > 1 {
				ident, isIdent := assign.Lhs[0].(*ast.Ident)
				call, isCall := assign.Rhs[0].(*ast.CallExpr)
				if isIdent && isCall {
					ref, ok := typeRefFromHelperCall(call, helpers, pkgPath)
					if ok {
						varTypes[ident.Name] = ref
					}
				}
				return true
			}
			if len(assign.Lhs) != len(assign.Rhs) {
				return true
			}
			for i, left := range assign.Lhs {
				ident, ok := left.(*ast.Ident)
				if !ok {
					continue
				}
				ref, ok := typeRefFromExpr(assign.Rhs[i], imports, pkgPath)
				if !ok {
					call, isCall := assign.Rhs[i].(*ast.CallExpr)
					if isCall {
						ref, ok = typeRefFromHelperCall(call, helpers, pkgPath)
					}
				}
				if ok {
					varTypes[ident.Name] = ref
				}
			}
			return true
		}

		declStmt, ok := node.(*ast.DeclStmt)
		if !ok {
			return true
		}

		gen, ok := declStmt.Decl.(*ast.GenDecl)
		if !ok {
			return true
		}
		if gen.Tok != token.VAR {
			return true
		}

		for _, spec := range gen.Specs {
			valueSpec, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			ref, ok := typeRefFromExpr(valueSpec.Type, imports, pkgPath)
			if !ok {
				continue
			}
			for _, name := range valueSpec.Names {
				varTypes[name.Name] = ref
			}
		}

		return true
	})

	return varTypes
}

// requestTypeFromHandler resolves the last argument of the handler's first response call
// (Config.ResponseCalls) to its type, through the local variables when it is one.
func (g *generator) requestTypeFromHandler(body *ast.BlockStmt, imports map[string]string, pkgPath string, helpers map[string]funcInfo) (typeRef, bool) {
	varTypes := localVariableTypes(body, imports, pkgPath, helpers)

	var found typeRef
	ok := false

	ast.Inspect(body, func(node ast.Node) bool {
		if ok {
			return false
		}

		call, isCall := node.(*ast.CallExpr)
		if !isCall {
			return true
		}

		selector, isSelector := call.Fun.(*ast.SelectorExpr)
		if !isSelector {
			return true
		}
		if selector.Sel == nil {
			return true
		}

		if !containsString(g.cfg.ResponseCalls, selector.Sel.Name) {
			return true
		}

		if len(call.Args) == 0 {
			return true
		}

		arg := call.Args[len(call.Args)-1]
		ident, isIdent := arg.(*ast.Ident)
		if isIdent {
			ref, has := varTypes[ident.Name]
			if has {
				found = ref
				ok = true
				return false
			}
		}

		ref, has := typeRefFromExpr(arg, imports, pkgPath)
		if has {
			found = ref
			ok = true
			return false
		}

		return true
	})

	return found, ok
}

func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

// typeRefFromHelperCall resolves `helper(...)` to the first result type of a package-level
// function in the same handler package, using that function's own file imports. A helper
// taking a []byte is a wire decoder instead: what it documents is the payload it unmarshals,
// not the internal type it returns.
func typeRefFromHelperCall(call *ast.CallExpr, helpers map[string]funcInfo, pkgPath string) (typeRef, bool) {
	ident, ok := call.Fun.(*ast.Ident)
	if !ok {
		return typeRef{}, false
	}
	helper, ok := helpers[ident.Name]
	if !ok {
		return typeRef{}, false
	}

	rawParams, isDecoder := byteSliceParams(helper.Decl)
	if isDecoder {
		return unmarshalTargetType(helper, rawParams, helpers, pkgPath)
	}

	return typeRefFromExpr(firstResultType(helper.Decl), helper.Imports, pkgPath)
}

// byteSliceParams reports whether fn takes a raw payload and names those parameters. A
// fixed-length byte array is a digest or a key, never a wire buffer, so it does not count.
func byteSliceParams(fn *ast.FuncDecl) (map[string]bool, bool) {
	names := map[string]bool{}
	found := false

	if fn == nil || fn.Type == nil || fn.Type.Params == nil {
		return names, found
	}

	for _, field := range fn.Type.Params.List {
		if field == nil {
			continue
		}
		array, ok := field.Type.(*ast.ArrayType)
		if !ok || array.Len != nil {
			continue
		}
		if !isByteSlice(array) {
			continue
		}

		found = true
		for _, name := range field.Names {
			names[name.Name] = true
		}
	}

	return names, found
}

// unmarshalTargetType resolves the target of the decoder's first `json.Unmarshal(raw, &target)`,
// which is the shape callers actually put on the wire. A decoder with no such target documents
// nothing, so it stays unresolved rather than falling back to its normalized result type.
func unmarshalTargetType(helper funcInfo, rawParams map[string]bool, helpers map[string]funcInfo, pkgPath string) (typeRef, bool) {
	if helper.Decl == nil || helper.Decl.Body == nil {
		return typeRef{}, false
	}

	var target ast.Expr

	ast.Inspect(helper.Decl.Body, func(node ast.Node) bool {
		if target != nil {
			return false
		}

		call, isCall := node.(*ast.CallExpr)
		if !isCall {
			return true
		}
		if !isJSONUnmarshalCall(call, helper.Imports) {
			return true
		}
		if len(call.Args) != 2 {
			return true
		}

		source, isIdent := call.Args[0].(*ast.Ident)
		if !isIdent || !rawParams[source.Name] {
			return true
		}

		target = call.Args[1]
		return false
	})

	if target == nil {
		return typeRef{}, false
	}

	unary, isUnary := target.(*ast.UnaryExpr)
	if isUnary {
		target = unary.X
	}

	ident, isIdent := target.(*ast.Ident)
	if !isIdent {
		return typeRef{}, false
	}

	varTypes := localVariableTypes(helper.Decl.Body, helper.Imports, pkgPath, helpers)

	ref, ok := varTypes[ident.Name]
	return ref, ok
}

func isJSONUnmarshalCall(call *ast.CallExpr, imports map[string]string) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel == nil || selector.Sel.Name != "Unmarshal" {
		return false
	}

	pkgIdent, ok := selector.X.(*ast.Ident)
	if !ok {
		return false
	}

	return imports[pkgIdent.Name] == "encoding/json"
}

func typeRefFromExpr(expr ast.Expr, imports map[string]string, pkgPath string) (typeRef, bool) {
	if expr == nil {
		return typeRef{}, false
	}

	switch t := expr.(type) {
	case *ast.UnaryExpr:
		return typeRefFromExpr(t.X, imports, pkgPath)
	case *ast.StarExpr:
		return typeRefFromExpr(t.X, imports, pkgPath)
	case *ast.CompositeLit:
		return typeRefFromExpr(t.Type, imports, pkgPath)
	case *ast.CallExpr:
		ident, ok := t.Fun.(*ast.Ident)
		if ok && ident.Name == "new" && len(t.Args) == 1 {
			return typeRefFromExpr(t.Args[0], imports, pkgPath)
		}
	case *ast.SelectorExpr:
		pkgIdent, ok := t.X.(*ast.Ident)
		if !ok {
			return typeRef{}, false
		}
		importPath := imports[pkgIdent.Name]
		if importPath == "" {
			return typeRef{}, false
		}
		return typeRef{ImportPath: importPath, Name: t.Sel.Name}, true
	case *ast.Ident:
		if t.Name == "" {
			return typeRef{}, false
		}
		return typeRef{ImportPath: pkgPath, Name: t.Name}, true
	}

	return typeRef{}, false
}

func packagePathForFile(repoRoot string, modulePath string, filePath string) string {
	rel, err := filepath.Rel(repoRoot, filePath)
	if err != nil {
		return ""
	}
	dir := filepath.Dir(rel)
	if dir == "." {
		return modulePath
	}
	return modulePath + "/" + filepath.ToSlash(dir)
}

// importMap maps each name a file refers to an import by to its import path.
func (b *schemaBuilder) importMap(file *ast.File) map[string]string {
	imports := map[string]string{}

	for _, spec := range file.Imports {
		pathValue := strings.Trim(spec.Path.Value, "\"")
		if pathValue == "" {
			continue
		}

		name := ""
		if spec.Name != nil {
			name = spec.Name.Name
		}
		if name == "" {
			name = b.packageName(pathValue)
		}
		if name == "" || name == "_" || name == "." {
			continue
		}

		imports[name] = pathValue
	}

	return imports
}

// packageName is the name an unnamed import is referred to by: the one in the package clause of
// its source (aws-sdk-go-v2's signer/v4 is v4), or importName's guess for the standard library
// and for a package whose source cannot be read.
func (b *schemaBuilder) packageName(importPath string) string {
	name, ok := b.packageNames[importPath]
	if ok {
		return name
	}

	name = b.declaredPackageName(importPath)
	if name == "" {
		name = importName(importPath)
	}
	if b.packageNames == nil {
		b.packageNames = map[string]string{}
	}
	b.packageNames[importPath] = name

	return name
}

// declaredPackageName reads the package clause of a non-standard-library package, "" when its
// source cannot be read. Only the clause is parsed, so naming an import loads nothing else.
func (b *schemaBuilder) declaredPackageName(importPath string) string {
	if b.isStandardLibraryImport(importPath) {
		return ""
	}
	dir, err := b.resolveImportPath(importPath)
	if err != nil {
		return ""
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return ""
	}

	for _, filePath := range files {
		if strings.HasSuffix(filePath, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), filePath, nil, parser.PackageClauseOnly)
		if err != nil {
			continue
		}
		// An importable package is never main; a main file beside it is a go:build ignore tool.
		if file.Name.Name == "main" {
			continue
		}
		return file.Name.Name
	}

	return ""
}

// importName is the name an unnamed import is referred to by, assumed the way goimports does: the
// last path element, or the one before a major version element (/v5), without a go- prefix and
// cut at its first character that cannot be in an identifier (gopkg.in's null.v4 is null).
func importName(importPath string) string {
	base := path.Base(importPath)
	if isMajorVersion(base) {
		dir := path.Dir(importPath)
		if dir != "." {
			base = path.Base(dir)
		}
	}
	base = strings.TrimPrefix(base, "go-")
	cut := strings.IndexFunc(base, func(r rune) bool {
		return r != '_' && !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	if cut >= 0 {
		base = base[:cut]
	}

	return base
}

// isMajorVersion reports a major version path element such as v2.
func isMajorVersion(element string) bool {
	version, ok := strings.CutPrefix(element, "v")
	if !ok || version == "" {
		return false
	}
	_, err := strconv.Atoi(version)

	return err == nil
}

func (g *generator) buildHandlerSchemas(facts handlerFacts) (map[string]handlerSchema, error) {
	builder := g.builder
	out := map[string]handlerSchema{}

	for name, reqType := range facts.requestTypes {
		var reqSchema map[string]any
		ok := false
		if g.cfg.RequestSchema != nil {
			reqSchema, ok = g.cfg.RequestSchema(name)
		}
		if !ok {
			reqSchema = builder.schemaRef(reqType)
		}

		respSchema, moduleDoc, err := responseSchemaForRequest(builder, reqType)
		if err != nil {
			return nil, ez.Wrap(err)
		}

		out[name] = handlerSchema{
			Request:    reqSchema,
			Response:   respSchema,
			PathFields: builder.jsonFieldNames(reqType, facts.pathFields[name]),
			ModuleDoc:  commentText(moduleDoc),
		}
	}

	return out, nil
}

// jsonFieldNames maps Go field names of a struct type to their JSON names.
func (b *schemaBuilder) jsonFieldNames(ref typeRef, goNames []string) []string {
	if len(goNames) == 0 {
		return nil
	}

	pkg, err := b.loadPackage(ref.ImportPath)
	if err != nil {
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

	wanted := map[string]bool{}
	for _, name := range goNames {
		wanted[name] = true
	}

	var names []string
	for _, field := range structure.Fields.List {
		for _, ident := range field.Names {
			if !wanted[ident.Name] || !ast.IsExported(ident.Name) {
				continue
			}
			name := jsonFieldName(ident.Name, field)
			if name != "" {
				names = append(names, name)
			}
		}
	}

	return names
}

// responseSchemaForRequest is the schema of the result of the module function taking the
// request type (see moduleFunction), with that function's doc comment.
func responseSchemaForRequest(builder *schemaBuilder, reqType typeRef) (map[string]any, *ast.CommentGroup, error) {
	pkg, err := builder.loadPackage(reqType.ImportPath)
	if err != nil {
		return nil, nil, ez.Wrap(err)
	}

	fn, ok := moduleFunction(pkg, reqType)
	if ok {
		resultType := firstResultType(fn.Decl)
		if resultType == nil {
			return nil, fn.Decl.Doc, nil
		}

		// A pointer result is the response body itself, never null on success.
		schema := builder.schemaForExpr(derefExpr(resultType), fn.Imports, pkg.ImportPath)
		return schema, fn.Decl.Doc, nil
	}

	// A handler that decodes the raw body documents the wire payload, but the module function
	// is keyed on the request type the decoder normalizes it into, so follow the decoder.
	moduleType, ok := decodedRequestType(pkg, reqType)
	if ok {
		return responseSchemaForRequest(builder, moduleType)
	}

	return nil, nil, nil
}

// moduleFunction picks the function of the request type's package that handles it: among the
// functions taking the request type, an exported one returning (T, error) first, then any
// exported one, then any, each in declaration order. A package also holds helpers taking the
// request (SignatureVerified(*WebhookRequest) bool) besides the operation itself.
func moduleFunction(pkg *packageInfo, reqType typeRef) (funcInfo, bool) {
	best := funcInfo{}
	bestRank := 3
	for _, fn := range pkg.Funcs {
		if fn.Decl == nil || !funcHasRequestParam(fn.Decl, reqType) {
			continue
		}

		rank := 2
		if fn.Decl.Name.IsExported() {
			rank = 1
			if returnsValueAndError(fn.Decl) {
				rank = 0
			}
		}
		if rank < bestRank {
			best = fn
			bestRank = rank
		}
	}

	return best, bestRank < 3
}

// returnsValueAndError reports a function returning exactly (T, error).
func returnsValueAndError(fn *ast.FuncDecl) bool {
	if fn.Type == nil || fn.Type.Results == nil {
		return false
	}

	var results []ast.Expr
	for _, field := range fn.Type.Results.List {
		count := len(field.Names)
		if count == 0 {
			count = 1
		}
		for i := 0; i < count; i++ {
			results = append(results, field.Type)
		}
	}
	if len(results) != 2 {
		return false
	}
	last, ok := results[1].(*ast.Ident)

	return ok && last.Name == "error"
}

// decodedRequestType returns the type a decoder in pkg produces from the wire payload wireType.
func decodedRequestType(pkg *packageInfo, wireType typeRef) (typeRef, bool) {
	for _, fn := range pkg.Funcs {
		rawParams, isDecoder := byteSliceParams(fn.Decl)
		if !isDecoder {
			continue
		}

		target, ok := unmarshalTargetType(fn, rawParams, nil, pkg.ImportPath)
		if !ok || target != wireType {
			continue
		}

		moduleType, ok := typeRefFromExpr(firstResultType(fn.Decl), fn.Imports, pkg.ImportPath)
		if !ok || moduleType == wireType {
			continue
		}

		return moduleType, true
	}

	return typeRef{}, false
}

func funcHasRequestParam(fn *ast.FuncDecl, reqType typeRef) bool {
	if fn.Type == nil || fn.Type.Params == nil {
		return false
	}

	for _, field := range fn.Type.Params.List {
		if field == nil {
			continue
		}
		if matchesRequestType(field.Type, reqType) {
			return true
		}
	}

	return false
}

func matchesRequestType(expr ast.Expr, reqType typeRef) bool {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return matchesRequestType(t.X, reqType)
	case *ast.Ident:
		return t.Name == reqType.Name
	case *ast.SelectorExpr:
		return t.Sel != nil && t.Sel.Name == reqType.Name
	}
	return false
}

func firstResultType(fn *ast.FuncDecl) ast.Expr {
	if fn.Type == nil || fn.Type.Results == nil {
		return nil
	}
	if len(fn.Type.Results.List) == 0 {
		return nil
	}
	return fn.Type.Results.List[0].Type
}

func (b *schemaBuilder) schemaRef(ref typeRef) map[string]any {
	_, overridden := b.typeSchemas[typeKey(ref)]
	if overridden {
		b.ensureSchema(ref)

		return map[string]any{
			"$ref": schemaRefPrefix + b.schemaName(ref),
		}
	}

	componentSchema, ok := b.wellKnownComponentSchema(ref)
	if ok {
		name := b.schemaName(ref)
		_, exists := b.schemas[name]
		if !exists {
			required, _ := componentSchema["required"].([]string)
			b.markDerived(required)
			b.schemas[name] = componentSchema
			b.recordOrigin(name, ref)
		}

		return map[string]any{
			"$ref": schemaRefPrefix + name,
		}
	}

	schema, ok := b.wellKnownSchema(ref)
	if ok {
		return schema
	}

	if ref.ImportPath == "" || ref.Name == "" {
		b.recordUnresolved(ref, "no import path or type name")
		return map[string]any{
			"type": "object",
		}
	}

	b.ensureSchema(ref)

	return map[string]any{
		"$ref": schemaRefPrefix + b.schemaName(ref),
	}
}

// schemaName is the component name of a type: Config.SchemaName's, else defaultSchemaName's.
// Two types given one name are recorded as a collision, which fails generation.
func (b *schemaBuilder) schemaName(ref typeRef) string {
	name := ""
	if b.namer != nil {
		name = b.namer(ref.ImportPath, ref.Name)
	} else {
		name = b.defaultSchemaName(ref)
	}

	if b.named == nil {
		b.named = map[string]typeRef{}
	}
	previous, ok := b.named[name]
	if !ok {
		b.named[name] = ref
	} else if previous != ref {
		b.collisions = appendUnique(b.collisions, "component "+name+" names both "+typeKey(previous)+" and "+typeKey(ref))
	}

	return name
}

// collisionError lists the component names given to several types, and those given to a type
// and to one of the generator's own components (the error envelope).
func (b *schemaBuilder) collisionError() error {
	collisions := append([]string(nil), b.collisions...)
	for _, reserved := range []string{errorResponseSchemaName, standardErrorSchemaName} {
		ref, ok := b.named[reserved]
		if ok {
			collisions = append(collisions, "component "+reserved+" names both "+typeKey(ref)+" and compose's error envelope")
		}
	}
	if len(collisions) == 0 {
		return nil
	}

	sort.Strings(collisions)
	return ez.New(ez.EINVALID, "schema name collisions, spec not written:\n  "+strings.Join(collisions, "\n  "), nil)
}

// defaultSchemaName is a type's import path relative to the module, with SchemaNameAliases and
// SchemaPrefixCollapse applied, `/ . -` turned into `_`, then the type name.
func (b *schemaBuilder) defaultSchemaName(ref typeRef) string {
	pathPart := strings.TrimPrefix(ref.ImportPath, b.modulePath)
	pathPart = strings.TrimPrefix(pathPart, "/")
	pathPart = aliasedSchemaPath(pathPart, b.nameAliases)
	for _, prefix := range sortedKeys(b.prefixCollapse) {
		if strings.HasPrefix(pathPart, prefix) {
			pathPart = b.prefixCollapse[prefix]
			break
		}
	}
	if pathPart == "" {
		pathPart = "root"
	}
	pathPart = strings.ReplaceAll(pathPart, "/", "_")
	pathPart = strings.ReplaceAll(pathPart, ".", "_")
	pathPart = strings.ReplaceAll(pathPart, "-", "_")

	return pathPart + "_" + ref.Name
}

// aliasedSchemaPath swaps a moved package path, or the moved prefix of a subpackage path, for
// the schema name prefix it keeps.
func aliasedSchemaPath(pathPart string, aliases map[string]string) string {
	for _, currentPrefix := range sortedKeys(aliases) {
		stablePrefix := aliases[currentPrefix]
		if pathPart == currentPrefix {
			return stablePrefix
		}

		subpackagePrefix := currentPrefix + "/"
		if strings.HasPrefix(pathPart, subpackagePrefix) {
			return stablePrefix + strings.TrimPrefix(pathPart, currentPrefix)
		}
	}

	return pathPart
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	return keys
}

func (b *schemaBuilder) ensureSchema(ref typeRef) {
	if b.building[ref] {
		return
	}

	name := b.schemaName(ref)
	_, ok := b.schemas[name]
	if ok {
		return
	}

	b.building[ref] = true

	schema := b.buildSchema(ref)
	b.schemas[name] = schema
	b.recordOrigin(name, ref)

	b.building[ref] = false
}

func (b *schemaBuilder) recordOrigin(name string, ref typeRef) {
	if b.origins == nil {
		b.origins = map[string]typeRef{}
	}
	b.origins[name] = ref
}

func (b *schemaBuilder) buildSchema(ref typeRef) map[string]any {
	override, overridden := b.typeSchemas[typeKey(ref)]
	if overridden && !isExtension(override) {
		return cloneSchema(override)
	}

	// A standard-library type the generator lists without a schema of its own, url.URL or
	// http.Header, stays an opaque object, a marshaler interface admits anything, and any other
	// is read from GOROOT's source (standardLibrarySchema).
	if b.isStandardLibraryImport(ref.ImportPath) {
		schema := map[string]any{"type": "object"}
		_, iface := marshalerInterfaces[typeKey(ref)]
		if iface {
			schema = map[string]any{}
		} else if !b.isListedStandardLibrary(ref) {
			schema = b.standardLibrarySchema(ref)
		}
		return extendedSchema(schema, override, b)
	}

	pkg, err := b.loadPackage(ref.ImportPath)
	if err != nil {
		b.recordUnresolved(ref, err.Error())
		return map[string]any{
			"type": "object",
		}
	}

	info, ok := pkg.Types[ref.Name]
	if !ok {
		b.recordUnresolved(ref, "type not declared in "+pkg.Dir)
		return map[string]any{
			"type": "object",
		}
	}

	// An alias's component references the type it names; a chain of them that does not end
	// within maxAliasHops fails generation here.
	if info.Spec.Assign.IsValid() {
		b.aliasTarget(ref)
	}
	declared, declaredImports, declaredPkgPath := b.declaredType(info, pkg.ImportPath)

	// A struct that marshals itself has whatever shape its MarshalJSON writes; its fields say
	// nothing reliable about it, so the generator refuses to guess. Neither can it describe the
	// fields of a generic type, which a type declared from one with a marshaler is written as.
	hint := "; describe it with Config.TypeSchemas[" + strconv.Quote(typeKey(ref)) + "]"
	if !overridden {
		reason := b.genericDeclarationReason(info, ref)
		if reason != "" {
			b.recordUnresolved(ref, "type "+ref.Name+" "+reason+hint)
			return map[string]any{
				"type": "object",
			}
		}
		reason = b.customMarshalReason(pkg.Methods[ref.Name], declared, declaredImports, declaredPkgPath, ref)
		if reason != "" {
			b.recordUnresolved(ref, reason)
			return map[string]any{
				"type": "object",
			}
		}
	}

	// A type with MarshalText and without MarshalJSON is written as the JSON string its
	// MarshalText returns, whatever its kind; its fields or underlying type say nothing about it.
	// MarshalJSON wins over MarshalText, as in encoding/json: a struct with it was refused above,
	// any other kind with it is still described by its kind.
	textual := !overridden && marshalsAsText(pkg.Methods[ref.Name])

	// A named struct goes to schemaForStruct directly: it was checked for a promoted marshaler
	// above, unless Config.TypeSchemas extends it, and schemaForExpr refuses an anonymous struct
	// with one.
	declaredStruct, isStruct := declared.(*ast.StructType)
	var schema map[string]any
	if textual {
		schema = map[string]any{"type": "string"}
	} else if isStruct {
		reason := ""
		schema, reason = b.schemaForStruct(declaredStruct, declaredImports, declaredPkgPath)
		if reason != "" {
			b.recordUnresolved(ref, "type "+ref.Name+" "+reason+hint)
		}
	} else {
		schema = b.schemaForExpr(declared, declaredImports, declaredPkgPath)
	}

	// The constants of a closed set are not what a MarshalText writes, so a textual type gets
	// no enum.
	values, isEnum := pkg.Enums[ref.Name]
	if isEnum && !textual && schema["type"] == "string" {
		enum := make([]any, 0, len(values))
		for _, value := range values {
			enum = append(enum, value)
		}
		schema["enum"] = enum

		// The constants' names, aligned with enum, for generators naming the members
		// (openapi-generator's x-enum-varnames).
		names := make([]any, 0, len(values))
		for _, name := range pkg.EnumNames[ref.Name] {
			names = append(names, name)
		}
		schema["x-enum-varnames"] = names

		// The descriptions twice: aligned with enum for openapi-generator
		// (x-enum-descriptions), keyed by value for Redocly and Scalar (x-enumDescriptions).
		docs := pkg.EnumDocs[ref.Name]
		if b.docComments && anyNonEmpty(docs) {
			descriptions := make([]any, 0, len(docs))
			byValue := map[string]any{}
			for i, doc := range docs {
				descriptions = append(descriptions, doc)
				if doc != "" {
					byValue[values[i]] = doc
				}
			}
			schema["x-enum-descriptions"] = descriptions
			schema["x-enumDescriptions"] = byValue
		}
	}

	if b.docComments {
		doc := commentText(info.Doc)
		_, isRef := schema["$ref"]
		if doc != "" && !isRef {
			schema["description"] = doc
		}
	}

	return extendedSchema(schema, override, b)
}

// typeKey is a type's Config.TypeSchemas key, "importpath.TypeName".
func typeKey(ref typeRef) string {
	return ref.ImportPath + "." + ref.Name
}

// declaredType is the type expression a type is described by, with the imports and package it
// is read in: its declaration, unless it is declared from another named type whose own marshal
// methods, or whose exact type, make that type's component differ from what a type declared from
// it is written as. A defined type does not inherit methods, so `type Priority Level`, with Level
// a MarshalText int, is written as an int, and `type Area Point`, with Point a struct with
// MarshalText or MarshalJSON, as Point's fields; such a type is described by the other type's
// underlying type rather than a reference to it. So is one declared from a type described in
// Config.TypeSchemas whose source declares MarshalJSON or MarshalText (describedMarshaler), as
// the description is of that method's output; a described type without either keeps the
// reference, its description refining its kind. One declared from a standard-library or
// well-known type that knownUnderlying lists, `type Stamp time.Time` or `type Amount
// json.Number`, is described by the type listed there, `struct{}` or `string`, and one declared
// from a standard-library type it does not list by declaredFromStandardLibrary. A type declared
// from an instantiation of a generic type with either method is refused before it is described
// (genericDeclarationReason). An alias,
// `type A = B`, is B itself and keeps the reference; the type declared from is resolved through
// its aliases (aliasTarget).
func (b *schemaBuilder) declaredType(info typeSpecInfo, pkgPath string) (ast.Expr, map[string]string, string) {
	if info.Spec.Assign.IsValid() {
		return info.Spec.Type, info.Imports, pkgPath
	}
	from, ok := namedTypeRef(info.Spec.Type, info.Imports, pkgPath)
	if !ok {
		return info.Spec.Type, info.Imports, pkgPath
	}
	// The methods of an alias are those of the type it names.
	from = b.aliasTarget(from)
	listed, ok := knownUnderlyingExpr(from)
	if ok {
		return listed, nil, ""
	}
	if b.isStandardLibraryImport(from.ImportPath) && !b.isListedStandardLibrary(from) {
		return b.declaredFromStandardLibrary(from, info, pkgPath)
	}
	methods, ok := b.sourceMethods(from)
	if !ok {
		return info.Spec.Type, info.Imports, pkgPath
	}
	underlying, imports, underlyingPkgPath, ok := b.underlyingType(from, 1, false)
	if !ok {
		return info.Spec.Type, info.Imports, pkgPath
	}

	_, isStruct := underlying.(*ast.StructType)
	marshals := marshalsAsText(methods) || (methods["MarshalJSON"] && isStruct)
	_, described := b.typeSchemas[typeKey(from)]
	if described {
		marshals = methods["MarshalJSON"] || methods["MarshalText"]
	}
	if marshals {
		return underlying, imports, underlyingPkgPath
	}

	return info.Spec.Type, info.Imports, pkgPath
}

// declaredFromStandardLibrary is declaredType for a type declared from a standard-library type
// the generator does not list: the reference to that type, unless its MarshalJSON or MarshalText,
// which the declared type does not get, makes its schema differ from its kind
// (standardLibrarySchema). Then it is the type that one is declared as, or for a struct an object
// whose fields are not described, as a standard-library struct's are not.
func (b *schemaBuilder) declaredFromStandardLibrary(from typeRef, info typeSpecInfo, pkgPath string) (ast.Expr, map[string]string, string) {
	_, pkg, ok := b.standardLibraryDeclaration(from)
	if !ok {
		return info.Spec.Type, info.Imports, pkgPath
	}
	methods := pkg.Methods[from.Name]
	if !methods["MarshalJSON"] && !methods["MarshalText"] {
		return info.Spec.Type, info.Imports, pkgPath
	}
	expr, imports, underlyingPkgPath, ok := b.standardLibraryUnderlying(from, 1)
	if !ok {
		return info.Spec.Type, info.Imports, pkgPath
	}
	_, isStruct := expr.(*ast.StructType)
	if isStruct {
		return &ast.StructType{Fields: &ast.FieldList{}}, nil, ""
	}

	return expr, imports, underlyingPkgPath
}

// underlyingType is the type expression a named type read from source is declared with,
// following declarations from other named types read from source, with the imports and package
// it is read in; hops counts the named types before ref in the chain, which fails generation
// past maxDeclarationHops. Aliases are resolved (aliasTarget) and do not count. A type described
// in Config.TypeSchemas is followed into its source when it has a marshaler (describedMarshaler),
// whose output its description is, or when intoDescribed asks for its fields whatever its
// description says; otherwise it ends the chain, as does any other named type not read from
// source (the standard library, a well-known type), returned as a reference, except one
// knownUnderlying lists, which gives the type listed there. ref itself may be a described type.
func (b *schemaBuilder) underlyingType(ref typeRef, hops int, intoDescribed bool) (ast.Expr, map[string]string, string, bool) {
	ref = b.aliasTarget(ref)
	if hops >= maxDeclarationHops {
		b.recordLongChain(ref)
		return nil, nil, "", false
	}
	pkg, err := b.loadPackage(ref.ImportPath)
	if err != nil {
		return nil, nil, "", false
	}
	info, ok := pkg.Types[ref.Name]
	if !ok || info.Spec.TypeParams != nil {
		return nil, nil, "", false
	}

	next, ok := namedTypeRef(info.Spec.Type, info.Imports, pkg.ImportPath)
	if !ok {
		return info.Spec.Type, info.Imports, pkg.ImportPath, true
	}
	next = b.aliasTarget(next)
	listed, ok := knownUnderlyingExpr(next)
	if ok {
		return listed, nil, "", true
	}
	_, described := b.typeSchemas[typeKey(next)]
	if b.readsFromSource(next) || (described && (intoDescribed || b.describedMarshaler(next))) {
		return b.underlyingType(next, hops+1, intoDescribed)
	}

	return info.Spec.Type, info.Imports, pkg.ImportPath, true
}

// namedTypeRef is the type a type expression names, other than a predeclared one.
func namedTypeRef(expr ast.Expr, imports map[string]string, pkgPath string) (typeRef, bool) {
	switch t := expr.(type) {
	case *ast.Ident:
		_, builtin := builtinSchema(t.Name)
		if builtin || t.Name == "error" {
			return typeRef{}, false
		}
	case *ast.SelectorExpr:
	default:
		return typeRef{}, false
	}

	return typeRefFromExpr(expr, imports, pkgPath)
}

// maxAliasHops is the longest chain of aliases, `type A = B`, `type B = C`, ..., the generator
// follows: 8 hops. Every walk over named types resolves aliases through aliasTarget, so an alias
// never counts as a step of its own depth limit.
const maxAliasHops = 8

// aliasTarget is the type an alias read from source names, `type A = B` gives B, following
// aliases of aliases into any package; ref itself when it is not an alias. An alias has the
// fields and methods of the type it names. The chain ends at a type not read from source, a
// described one included, and at an alias of anything but a named type, such as a generic
// instantiation, which is returned. A chain longer than maxAliasHops, such as a cycle in source
// that does not compile, fails generation, and the last alias reached is returned.
func (b *schemaBuilder) aliasTarget(ref typeRef) typeRef {
	start := ref
	for hops := 0; ; hops++ {
		target, ok := b.aliasOf(ref)
		if !ok {
			return ref
		}
		if hops == maxAliasHops {
			b.recordUnresolved(start, "alias chain longer than "+strconv.Itoa(maxAliasHops)+" aliases, or a cycle; the generator follows "+strconv.Itoa(maxAliasHops))
			return ref
		}
		ref = target
	}
}

// aliasOf is the named type an alias names, ok false for any other type: one read from source,
// or a standard-library one the generator does not list, `os.FileMode = fs.FileMode`, read from
// GOROOT's source when it can be (a reader of its declaration fails generation when it cannot).
func (b *schemaBuilder) aliasOf(ref typeRef) (typeRef, bool) {
	standardLibrary := b.isStandardLibraryImport(ref.ImportPath) && !b.isListedStandardLibrary(ref)
	if !b.readsFromSource(ref) && !standardLibrary {
		return typeRef{}, false
	}
	info, pkg, found, err := b.declarationOf(ref)
	if err != nil || !found || !info.Spec.Assign.IsValid() {
		return typeRef{}, false
	}

	return namedTypeRef(info.Spec.Type, info.Imports, pkg.ImportPath)
}

// sourceMethods is the method set the generator reads from source for a named type, also for
// one described in Config.TypeSchemas: a description says what the type's JSON is, not which
// methods it has. ok is false for a standard-library or well-known type and one whose package
// cannot be loaded.
func (b *schemaBuilder) sourceMethods(ref typeRef) (map[string]bool, bool) {
	if b.isStandardLibraryImport(ref.ImportPath) {
		return nil, false
	}
	_, wellKnown := b.wellKnownSchema(ref)
	if wellKnown {
		return nil, false
	}
	pkg, err := b.loadPackage(ref.ImportPath)
	if err != nil {
		return nil, false
	}

	return pkg.Methods[ref.Name], true
}

// methodsOf is a named type's method set: knownMarshalers' for a type whose source the generator
// does not read, sourceMethods' for any other. ok is false when neither knows it.
func (b *schemaBuilder) methodsOf(ref typeRef) (map[string]bool, bool) {
	listed, ok := knownMarshalers[typeKey(ref)]
	if ok {
		return listed, true
	}

	return b.sourceMethods(ref)
}

// describedMarshaler reports a type described in Config.TypeSchemas that declares MarshalJSON
// or MarshalText (methodsOf): the description is of what that method writes, which neither a
// type declared from it nor a struct that embeds it without getting the method writes.
func (b *schemaBuilder) describedMarshaler(ref typeRef) bool {
	_, described := b.typeSchemas[typeKey(ref)]
	if !described {
		return false
	}
	methods, _ := b.methodsOf(ref)

	return methods["MarshalJSON"] || methods["MarshalText"]
}

// readsFromSource reports a named type whose declaration and methods the generator reads from
// source: not one described in Config.TypeSchemas, a well-known type or one from the standard
// library.
func (b *schemaBuilder) readsFromSource(ref typeRef) bool {
	_, described := b.typeSchemas[typeKey(ref)]
	if described || b.isStandardLibraryImport(ref.ImportPath) {
		return false
	}
	_, wellKnown := b.wellKnownSchema(ref)

	return !wellKnown
}

// customMarshalReason explains why the fields of a struct type, declared as expr, do not describe
// its JSON: methods, its own, has MarshalJSON, or an embedded type promotes MarshalJSON or
// MarshalText into its method set, so it marshals as that type alone, or the generator cannot
// tell whether one does (promotedMarshaler). "" for any other struct, which encoding/json writes
// as its fields unless its own MarshalText makes it a string (buildSchema), and for any other
// kind.
func (b *schemaBuilder) customMarshalReason(methods map[string]bool, expr ast.Expr, imports map[string]string, pkgPath string, ref typeRef) string {
	structure, ok := expr.(*ast.StructType)
	if !ok {
		return ""
	}

	hint := "; describe it with Config.TypeSchemas[" + strconv.Quote(typeKey(ref)) + "]"
	if methods["MarshalJSON"] {
		return "type " + ref.Name + " defines MarshalJSON" + hint
	}

	reason := b.promotedMarshaler(structure, imports, pkgPath, methods["MarshalText"])
	if reason != "" {
		return "type " + ref.Name + " " + reason + hint
	}

	return ""
}

// sourceStruct is a struct declaration read from source, and the named type it declares.
type sourceStruct struct {
	ref     typeRef
	st      *ast.StructType
	imports map[string]string
	pkgPath string
	// generic is the generic type whose declaration st is, for an instantiation, an alias of one
	// or a type declared from one (genericStruct): its fields' names are read, not their types.
	generic typeRef
}

// structFromSource is the struct declaration a named type read from source has, following
// declarations from other named types, whose fields, not methods, it has (underlyingType): those
// read from source, described ones whatever their description says, as it is of them and not
// of a type declared from them, and the struct{} of a listed type such as time.Time
// (knownUnderlying). One declared from database/sql's Null types has that struct (sqlNullDecl).
// A described type itself is read when its source declares a marshaler (describedMarshaler): a
// struct that embeds it without getting the method gets its fields, which its description does
// not say.
func (b *schemaBuilder) structFromSource(ref typeRef) (sourceStruct, bool) {
	if !b.readsFromSource(ref) && !b.describedMarshaler(ref) {
		return sourceStruct{}, false
	}
	expr, imports, pkgPath, ok := b.underlyingType(ref, 0, true)
	if !ok {
		return sourceStruct{}, false
	}
	generic, ok := b.genericStruct(expr, imports, pkgPath)
	if ok {
		// An alias of an instantiation has the generic type's methods; a type declared from one
		// has its own.
		info, _, _, _ := b.declarationOf(ref)
		if info.Spec == nil || !info.Spec.Assign.IsValid() {
			generic.ref = ref
		}
		return generic, true
	}
	structure, ok := expr.(*ast.StructType)
	if !ok {
		null, ok := sqlNullDecl(expr, imports, pkgPath)
		if !ok {
			return sourceStruct{}, false
		}
		null.ref = ref
		return null, true
	}

	return sourceStruct{ref: ref, st: structure, imports: imports, pkgPath: pkgPath}, true
}

// genericStruct is the declaration of the generic struct an instantiation, Box[int], instantiates,
// read from source with its type parameters as they are: the generator reads the names of its
// fields, which do not depend on the type arguments, but does not instantiate their types. Its
// ref is the generic type, whose methods an instantiation has. ok is false for database/sql's
// Null[T], read as the struct it is (sqlNullDecl), for any other expression and for a generic
// type not read from source.
func (b *schemaBuilder) genericStruct(expr ast.Expr, imports map[string]string, pkgPath string) (sourceStruct, bool) {
	generic := genericType(expr)
	if generic == expr {
		return sourceStruct{}, false
	}
	ref, ok := typeRefFromExpr(generic, imports, pkgPath)
	if !ok {
		return sourceStruct{}, false
	}
	ref = b.aliasTarget(ref)
	if isSQLNull(ref) || b.isStandardLibraryImport(ref.ImportPath) {
		return sourceStruct{}, false
	}
	pkg, err := b.loadPackage(ref.ImportPath)
	if err != nil {
		return sourceStruct{}, false
	}
	info, ok := pkg.Types[ref.Name]
	if !ok || info.Spec.TypeParams == nil {
		return sourceStruct{}, false
	}
	structure, ok := info.Spec.Type.(*ast.StructType)
	if !ok {
		return sourceStruct{}, false
	}

	return sourceStruct{ref: ref, st: structure, imports: info.Imports, pkgPath: pkg.ImportPath, generic: ref}, true
}

// genericDeclarationReason explains why a type declared from an instantiation of a generic type
// with MarshalJSON or MarshalText, `type BoxD Box[int]`, cannot be described: it gets none of
// those methods, so encoding/json writes it as the generic type's fields, which the generator
// does not instantiate, while the generic type's description in Config.TypeSchemas is of the
// method's output. "" for any other type.
func (b *schemaBuilder) genericDeclarationReason(info typeSpecInfo, ref typeRef) string {
	if info.Spec.Assign.IsValid() {
		return ""
	}
	expr, imports, pkgPath, ok := b.underlyingType(ref, 0, false)
	if !ok {
		return ""
	}
	generic := genericType(expr)
	if generic == expr {
		return ""
	}
	genericRef, ok := typeRefFromExpr(generic, imports, pkgPath)
	if !ok {
		return ""
	}
	genericRef = b.aliasTarget(genericRef)
	methods, _ := b.methodsOf(genericRef)
	method := "MarshalText"
	if methods["MarshalJSON"] {
		method = "MarshalJSON"
	}
	if isSQLNull(genericRef) || !methods[method] {
		return ""
	}

	return "is declared from " + types.ExprString(expr) + " and does not get its " + method + ", so encoding/json writes it as the fields of " + genericRef.Name + ", whose types the generator does not instantiate"
}

// promotedKey memoizes promotedMarshaler by a struct's AST node, which the cached packages keep
// for the builder's life, and its own MarshalText. The answer depends on source alone, which
// does not change while the builder runs.
type promotedKey struct {
	st      *ast.StructType
	ownText bool
}

// promotedMarshaler is "embeds X, which ..." when an embedded type promotes MarshalJSON or
// MarshalText into a struct's method set, or when the generator cannot tell whether one does;
// "" when none does. It follows Go's selector rule for each of the two methods: depth by depth,
// every path to an embedded type counts, two paths to one type at one depth included, and a
// type already reached at a shallower depth is not looked at again. The method is promoted from
// the shallowest depth where an embedded type declares it, when exactly one path reaches such a
// type there; two make it ambiguous, and it is not promoted, nor from deeper. ownText is a
// MarshalText the struct declares, which shadows a promoted one. An embedded alias has the
// methods of the type it names (aliasTarget, and embeddedTypeOf for an alias of a generic
// instantiation) and is reported by the name it is embedded by; an interface has the methods it
// lists and embeds (embeddedTypeOf). A type described in Config.TypeSchemas counts with the
// methods its source declares, and a standard-library type with those GOROOT's source declares.
//
// The generator cannot tell which methods a type has whose source it cannot read: one whose
// package is not in the module cache, or a standard-library one it does not list when GOROOT's
// source cannot be read. When such a type is reached at a depth where a method is still
// undecided and fewer than two paths there reach a type known to declare it, the type could
// change the answer: the reason names it and the embedded types the struct reaches it through,
// and the struct is refused rather than described by fields Go may not write. A standard-library
// type fails generation also where both methods are already ambiguous, as reading it does
// wherever it is needed (standardLibraryDeclaration). Two limits remain: the instantiations of a
// generic type count as one type, so G[string] is not looked at below a G[int] reached at a
// shallower depth, and a field named MarshalJSON or MarshalText, which in Go hides a deeper
// method of that name, is not looked for.
func (b *schemaBuilder) promotedMarshaler(structure *ast.StructType, imports map[string]string, pkgPath string, ownText bool) string {
	key := promotedKey{st: structure, ownText: ownText}
	reason, ok := b.promoted[key]
	if ok {
		return reason
	}
	reason = b.findPromotedMarshaler(structure, imports, pkgPath, ownText)
	if b.promoted == nil {
		b.promoted = map[promotedKey]string{}
	}
	b.promoted[key] = reason

	return reason
}

// embeddedPath is a type promotedMarshaler reaches at one depth, with the number of paths that
// reach it there and the first of them, chain, written as the embedded types along it.
type embeddedPath struct {
	ref       typeRef
	name      string
	qualified string
	chain     string
	paths     int
	decl      sourceStruct
}

func (b *schemaBuilder) findPromotedMarshaler(structure *ast.StructType, imports map[string]string, pkgPath string, ownText bool) string {
	level := []embeddedPath{{decl: sourceStruct{st: structure, imports: imports, pkgPath: pkgPath}, paths: 1}}
	seen := map[typeRef]bool{}
	jsonDecided := false
	textDecided := ownText

	for len(level) > 0 && !(jsonDecided && textDecided) {
		// The types one depth further, the paths to one type added up.
		var reached []embeddedPath
		position := map[typeRef]int{}
		for _, current := range level {
			for _, field := range current.decl.st.Fields.List {
				if len(field.Names) > 0 {
					continue
				}
				expr := derefExpr(field.Type)
				written, ok := typeRefFromExpr(genericType(expr), current.decl.imports, current.decl.pkgPath)
				if !ok {
					continue
				}
				ref := b.aliasTarget(written)
				at, found := position[ref]
				if found {
					reached[at].paths += current.paths
					continue
				}
				position[ref] = len(reached)
				qualified := types.ExprString(expr)
				chain := qualified
				if current.chain != "" {
					chain = current.chain + ", which embeds " + qualified
				}
				reached = append(reached, embeddedPath{ref: ref, name: written.Name, qualified: qualified, chain: chain, paths: current.paths})
			}
		}

		jsonOwners := 0
		textOwners := 0
		jsonReason := ""
		textReason := ""
		unknown := ""
		var next []embeddedPath
		for _, path := range reached {
			if seen[path.ref] {
				continue
			}
			seen[path.ref] = true

			embedded := b.embeddedTypeOf(path.ref, 0)
			if !embedded.known {
				from := ""
				if embedded.unreadable != path.ref {
					from = ", which comes from " + typeKey(embedded.unreadable)
				}
				unknown = "embeds " + path.chain + from + ", whose source the generator cannot read (" + embedded.cause + "), so it cannot tell whether it marshals as " + path.name + " alone"
				continue
			}
			note := ""
			_, described := b.typeSchemas[typeKey(path.ref)]
			if described {
				note = "; Config.TypeSchemas describes " + path.name + ", not the types that embed it"
			}
			if embedded.methods["MarshalJSON"] {
				jsonOwners += path.paths
				jsonReason = "embeds " + path.name + ", which defines MarshalJSON, so it marshals as " + path.name + " alone" + note
			}
			if embedded.methods["MarshalText"] {
				textOwners += path.paths
				textReason = "embeds " + path.name + ", which defines MarshalText, so it marshals as " + path.name + "'s text alone" + note
			}
			if embedded.isStruct {
				next = append(next, embeddedPath{ref: path.ref, chain: path.chain, paths: path.paths, decl: embedded.decl})
			}
		}

		if !jsonDecided {
			if jsonOwners < 2 && unknown != "" {
				return unknown
			}
			if jsonOwners == 1 {
				return jsonReason
			}
			jsonDecided = jsonOwners > 1
		}
		if !textDecided {
			if textOwners < 2 && unknown != "" {
				return unknown
			}
			if textOwners == 1 {
				return textReason
			}
			textDecided = textOwners > 1
		}
		level = next
	}

	return ""
}

// embeddedType is what promotedMarshaler knows of an embedded type.
type embeddedType struct {
	// methods has its MarshalJSON and MarshalText, among others.
	methods map[string]bool
	// known is false when the generator cannot tell which marshal methods the type has or which
	// types it embeds: it cannot read the source of unreadable, which is the type or one it is
	// declared from or embeds, for cause.
	known      bool
	unreadable typeRef
	cause      string
	// iface is an interface type, whose methods a type declared from it keeps.
	iface bool
	// decl is its struct, whose embedded types are one depth further, when isStruct.
	decl     sourceStruct
	isStruct bool
}

// embeddedTypeOf is what an embedded type, its aliases resolved, has: the marshal methods
// knownMarshalers or marshalerInterfaces lists, else those its source declares, a described
// type's included, read from GOROOT for the standard library (declarationOf). An interface has
// the methods it lists and those of the interfaces it embeds: whatever value it holds, those are
// the methods Go promotes from it. A type declared from another named type has that type's
// fields, from a described type's source too, and its methods only when that type is an
// interface; an alias has the methods and fields of the type it names, a generic one's for an
// alias of an instantiation, `type B = Box[int]`, and a type declared from an instantiation has
// the generic type's fields. hops counts the declarations followed, at most
// maxDeclarationHops.
func (b *schemaBuilder) embeddedTypeOf(ref typeRef, hops int) embeddedType {
	listed, ok := knownMarshalers[typeKey(ref)]
	if ok {
		return embeddedType{methods: listed, known: true}
	}
	listed, ok = marshalerInterfaces[typeKey(ref)]
	if ok {
		return embeddedType{methods: listed, known: true, iface: true}
	}
	if hops >= maxDeclarationHops {
		b.recordLongChain(ref)
		return embeddedType{known: true}
	}
	info, pkg, found, err := b.declarationOf(ref)
	if err != nil {
		return embeddedType{unreadable: ref, cause: ez.ErrorMessage(err)}
	}
	if !found {
		// A predeclared type, error, any or uintptr, has neither marshaler; any is an interface.
		_, builtin := builtinSchema(ref.Name)
		if builtin || ref.Name == "error" || !b.isStandardLibraryImport(ref.ImportPath) {
			return embeddedType{known: true, iface: ref.Name == "any"}
		}
		return embeddedType{unreadable: ref, cause: ref.Name + " is not declared in " + pkg.Dir + platformNote()}
	}
	methods := pkg.Methods[ref.Name]

	switch t := info.Spec.Type.(type) {
	case *ast.InterfaceType:
		return b.interfaceTypeOf(t, info.Imports, pkg.ImportPath, hops)
	case *ast.Ident:
		// `type Token any` is an interface without methods, as interface{} is.
		if t.Name == "any" {
			return embeddedType{methods: map[string]bool{}, known: true, iface: true}
		}
	case *ast.StructType:
		// A generic struct counts once for all its instantiations: Go does not let a struct
		// embed a type parameter, so its embedded types and their methods do not depend on the
		// type arguments.
		decl := sourceStruct{ref: ref, st: t, imports: info.Imports, pkgPath: pkg.ImportPath}
		return embeddedType{methods: methods, known: true, decl: decl, isStruct: true}
	}
	null, ok := sqlNullDecl(info.Spec.Type, info.Imports, pkg.ImportPath)
	if ok {
		null.ref = ref
		return embeddedType{methods: methods, known: true, decl: null, isStruct: true}
	}
	from, ok := namedTypeRef(genericType(info.Spec.Type), info.Imports, pkg.ImportPath)
	if !ok {
		return embeddedType{methods: methods, known: true}
	}
	declared := b.embeddedTypeOf(b.aliasTarget(from), hops+1)
	if !declared.iface && !info.Spec.Assign.IsValid() {
		declared.methods = methods
	}

	return declared
}

// interfaceTypeOf is an interface's embeddedType: the methods it lists and those of the
// interfaces it embeds.
func (b *schemaBuilder) interfaceTypeOf(iface *ast.InterfaceType, imports map[string]string, pkgPath string, hops int) embeddedType {
	methods := map[string]bool{}
	for _, field := range iface.Methods.List {
		for _, name := range field.Names {
			methods[name.Name] = true
		}
		if len(field.Names) > 0 {
			continue
		}
		// error, a predeclared interface, has neither marshaler; a type constraint is no
		// method set.
		embedded, ok := namedTypeRef(field.Type, imports, pkgPath)
		if !ok {
			continue
		}
		inner := b.embeddedTypeOf(b.aliasTarget(embedded), hops+1)
		if !inner.known {
			inner.iface = true
			return inner
		}
		maps.Copy(methods, inner.methods)
	}

	return embeddedType{methods: methods, known: true, iface: true}
}

// declarationOf is a named type's declaration and package, read from GOROOT for a
// standard-library type (loadStandardLibrary) and by loadPackage for any other. found is false
// when the package does not declare it.
func (b *schemaBuilder) declarationOf(ref typeRef) (typeSpecInfo, *packageInfo, bool, error) {
	var pkg *packageInfo
	var err error
	if b.isStandardLibraryImport(ref.ImportPath) {
		pkg, err = b.loadStandardLibrary(ref.ImportPath)
	} else {
		pkg, err = b.loadPackage(ref.ImportPath)
	}
	if err != nil {
		return typeSpecInfo{}, nil, false, ez.Wrap(err)
	}
	info, found := pkg.Types[ref.Name]

	return info, pkg, found, nil
}

// platformNote says which files of a standard-library package the generator reads, for a reason
// that a type is not declared there: a type that exists on other platforms only is not.
func platformNote() string {
	return ", whose files the generator selects by their build constraints for " + build.Default.GOOS + "/" + build.Default.GOARCH + ", the platform it runs on"
}

// isListedStandardLibrary reports a standard-library type whose JSON the generator lists rather
// than reads from GOROOT: knownMarshalers, marshalerInterfaces, knownUnderlying, wellKnownSchema
// and wellKnownComponentSchema. An app using only these generates without GOROOT's source.
func (b *schemaBuilder) isListedStandardLibrary(ref typeRef) bool {
	if !b.isStandardLibraryImport(ref.ImportPath) {
		return false
	}
	key := typeKey(ref)
	_, known := knownMarshalers[key]
	_, iface := marshalerInterfaces[key]
	_, underlying := knownUnderlying[key]
	_, wellKnown := b.wellKnownSchema(ref)
	_, component := b.wellKnownComponentSchema(ref)

	return known || iface || underlying || wellKnown || component
}

// namedDeclaration is the declaration a walk over named types follows: loadPackage's for a type
// of a module, GOROOT's for a standard-library one (standardLibraryDeclaration), which the
// caller has checked is not listed (isListedStandardLibrary). ok is false when it cannot be read.
func (b *schemaBuilder) namedDeclaration(ref typeRef) (typeSpecInfo, *packageInfo, bool) {
	if b.isStandardLibraryImport(ref.ImportPath) {
		return b.standardLibraryDeclaration(ref)
	}
	pkg, err := b.loadPackage(ref.ImportPath)
	if err != nil {
		return typeSpecInfo{}, nil, false
	}
	info, ok := pkg.Types[ref.Name]

	return info, pkg, ok
}

// standardLibraryDeclaration is a standard-library type's declaration in GOROOT's source. When
// that source cannot be read, or does not declare the type, generation fails: the type would
// otherwise be described one way on a machine with GOROOT's source and another way without it.
func (b *schemaBuilder) standardLibraryDeclaration(ref typeRef) (typeSpecInfo, *packageInfo, bool) {
	info, pkg, found, err := b.declarationOf(ref)
	if err != nil {
		b.recordUnresolved(ref, "the generator reads this standard-library type from GOROOT's source, which it cannot read: "+ez.ErrorMessage(err))
		return typeSpecInfo{}, nil, false
	}
	if !found {
		b.recordUnresolved(ref, "not declared in GOROOT's source at "+pkg.Dir+platformNote())
		return typeSpecInfo{}, nil, false
	}

	return info, pkg, true
}

// standardLibrarySchema is the schema of a standard-library type the generator does not list,
// read from GOROOT's source by the rules a type read from source follows: an interface is written
// as the value it holds, so it admits anything; a type with MarshalText and without MarshalJSON
// is a string; any other is its kind, `time.Month` an integer, `template.HTML` a string,
// `net.IPMask` a base64 string, following the named types it is declared from, whose methods it
// does not get. Two cases are not read: a type with MarshalJSON, whose output its declaration does
// not say, admits anything, and a struct stays an object whose fields are not described. An
// alias is the type it names.
func (b *schemaBuilder) standardLibrarySchema(ref typeRef) map[string]any {
	info, pkg, ok := b.standardLibraryDeclaration(ref)
	if !ok {
		return map[string]any{"type": "object"}
	}
	if b.isInterface(ref) {
		return map[string]any{}
	}
	if info.Spec.Assign.IsValid() {
		return b.schemaForExpr(info.Spec.Type, info.Imports, pkg.ImportPath)
	}
	methods := pkg.Methods[ref.Name]
	if methods["MarshalJSON"] {
		return map[string]any{}
	}
	if methods["MarshalText"] {
		return map[string]any{"type": "string"}
	}

	expr, imports, pkgPath, ok := b.standardLibraryUnderlying(ref, 0)
	if !ok {
		return map[string]any{"type": "object"}
	}
	_, isStruct := expr.(*ast.StructType)
	if isStruct {
		return map[string]any{"type": "object"}
	}

	return b.schemaForExpr(expr, imports, pkgPath)
}

// standardLibraryUnderlying is the type expression a standard-library type is declared as,
// following the standard-library types it is declared from (an alias of one too) and ending at
// a type knownUnderlying lists, with the imports and package it is read in.
func (b *schemaBuilder) standardLibraryUnderlying(ref typeRef, hops int) (ast.Expr, map[string]string, string, bool) {
	listed, ok := knownUnderlyingExpr(ref)
	if ok {
		return listed, nil, "", true
	}
	if hops >= maxDeclarationHops {
		b.recordLongChain(ref)
		return nil, nil, "", false
	}
	info, pkg, ok := b.standardLibraryDeclaration(ref)
	if !ok {
		return nil, nil, "", false
	}
	next, ok := namedTypeRef(info.Spec.Type, info.Imports, pkg.ImportPath)
	if !ok || !b.isStandardLibraryImport(next.ImportPath) {
		return info.Spec.Type, info.Imports, pkg.ImportPath, true
	}

	return b.standardLibraryUnderlying(next, hops+1)
}

// isInterface reports a named type that is an interface (embeddedTypeOf), such as
// context.Context, `type Token any` or one declared in source: encoding/json writes it as the
// value it holds, or null. A type whose source cannot be read is not reported as one.
func (b *schemaBuilder) isInterface(ref typeRef) bool {
	embedded := b.embeddedTypeOf(ref, 0)

	return embedded.known && embedded.iface
}

// maxDeclarationHops is the longest chain of named types declared from one another, `type A B`,
// `type B C`, ..., the generator follows: 8 types. Source that compiles has no cycle, so a longer
// chain fails generation (recordLongChain) rather than being described by a guess.
const maxDeclarationHops = 8

// recordLongChain fails generation for a type reached as the ninth of a chain of declarations.
func (b *schemaBuilder) recordLongChain(ref typeRef) {
	b.recordUnresolved(ref, "reached through a chain of more than "+strconv.Itoa(maxDeclarationHops)+" types declared from one another, or a cycle; the generator follows "+strconv.Itoa(maxDeclarationHops))
}

// marshalsAsText reports a method set with MarshalText and without MarshalJSON: encoding/json
// writes such a value as the string MarshalText returns.
//
// Methods does not tell value receivers from pointer receivers, and a pointer-receiver
// MarshalText counts too. encoding/json calls it on every addressable value: each field of a
// value marshaled through a pointer (a module function's *Response), each slice element, and
// what a pointer field points to. A value it cannot address, such as a map value or a struct
// marshaled by value, is written by its kind instead; the schema does not describe that case.
func marshalsAsText(methods map[string]bool) bool {
	return methods["MarshalText"] && !methods["MarshalJSON"]
}

// knownMarshalers are the marshal methods of the concrete types whose source the generator does
// not read or need not read (common standard-library types, the well-known types of
// wellKnownSchema and wellKnownComponentSchema, and bun.BaseModel, which the walk skips), as
// their Go source declares them, with none for those known to have neither. The receivers are
// values except for big.Int, big.Float, big.Rat and slog.LevelVar, whose methods have pointer
// receivers and count all the same (marshalsAsText). No listed type embeds a type, and the
// structs with a marshal method have unexported fields only. The methods of a standard-library
// type not listed here are read from GOROOT (embeddedTypeOf).
var knownMarshalers = map[string]map[string]bool{
	"time.Time":                              {"MarshalJSON": true, "MarshalText": true},
	"time.Duration":                          {},
	"encoding/json.RawMessage":               {"MarshalJSON": true},
	"encoding/json.Number":                   {},
	"net.IP":                                 {"MarshalText": true},
	"net/netip.Addr":                         {"MarshalText": true},
	"net/netip.AddrPort":                     {"MarshalText": true},
	"net/netip.Prefix":                       {"MarshalText": true},
	"math/big.Int":                           {"MarshalJSON": true, "MarshalText": true},
	"math/big.Float":                         {"MarshalText": true},
	"math/big.Rat":                           {"MarshalText": true},
	"log/slog.Level":                         {"MarshalJSON": true, "MarshalText": true},
	"log/slog.LevelVar":                      {"MarshalText": true},
	"github.com/google/uuid.UUID":            {"MarshalText": true},
	paginationImportPath + ".CursorRequest":  {},
	paginationImportPath + ".CursorResponse": {},
	"github.com/vanclief/compose/drivers/databases/relational/postgres/query.Filter": {},
	"github.com/uptrace/bun.BaseModel":                                               {},
	"github.com/uptrace/bun/schema.BaseModel":                                        {},
	"mime/multipart.FileHeader":                                                      {},
	"net/url.URL":                                                                    {},
	"net/url.Values":                                                                 {},
	"net/http.Header":                                                                {},
	"net/textproto.MIMEHeader":                                                       {},
	"sync.Mutex":                                                                     {},
	"sync.RWMutex":                                                                   {},
	"database/sql.Null":                                                              {},
	"database/sql.NullBool":                                                          {},
	"database/sql.NullByte":                                                          {},
	"database/sql.NullFloat64":                                                       {},
	"database/sql.NullInt16":                                                         {},
	"database/sql.NullInt32":                                                         {},
	"database/sql.NullInt64":                                                         {},
	"database/sql.NullString":                                                        {},
	"database/sql.NullTime":                                                          {},
}

// knownUnderlying is the type each listed type whose marshal method makes it differ from its
// kind is declared as, and json.Number's, which encoding/json writes as a number by its exact
// type alone: a type declared from one of them gets none of its methods, nor that exception, and
// is written by this type. A struct whose fields are all unexported is written as {}, so it is
// struct{} here.
var knownUnderlying = map[string]string{
	"time.Time":                   "struct{}",
	"encoding/json.RawMessage":    "[]byte",
	"encoding/json.Number":        "string",
	"net.IP":                      "[]byte",
	"net/netip.Addr":              "struct{}",
	"net/netip.AddrPort":          "struct{}",
	"net/netip.Prefix":            "struct{}",
	"math/big.Int":                "struct{}",
	"math/big.Float":              "struct{}",
	"math/big.Rat":                "struct{}",
	"log/slog.Level":              "int",
	"log/slog.LevelVar":           "struct{}",
	"github.com/google/uuid.UUID": "[16]byte",
}

// knownUnderlyingExpr is knownUnderlying's type for ref, parsed; it names no package.
func knownUnderlyingExpr(ref typeRef) (ast.Expr, bool) {
	source, ok := knownUnderlying[typeKey(ref)]
	if !ok {
		return nil, false
	}
	expr, err := parser.ParseExpr(source)
	if err != nil {
		return nil, false
	}

	return expr, true
}

// marshalerInterfaces are the standard library's interfaces made of a marshal method: a struct
// that embeds one gets that method.
var marshalerInterfaces = map[string]map[string]bool{
	"encoding/json.Marshaler": {"MarshalJSON": true},
	"encoding.TextMarshaler":  {"MarshalText": true},
}

func anyNonEmpty(values []string) bool {
	for _, value := range values {
		if value != "" {
			return true
		}
	}

	return false
}

// recordUnresolved records a type generation fails for, once per reason.
func (b *schemaBuilder) recordUnresolved(ref typeRef, reason string) {
	line := ref.ImportPath + "." + ref.Name + ": " + reason
	if slices.Contains(b.unresolved, line) {
		return
	}
	b.unresolved = append(b.unresolved, line)
}

// unresolvedError lists every type that came out as an opaque object; generation must
// stop rather than commit a spec missing their fields.
func (b *schemaBuilder) unresolvedError() error {
	if len(b.unresolved) == 0 {
		return nil
	}

	sort.Strings(b.unresolved)
	return ez.New(ez.EINTERNAL, "unresolved types, spec not written:\n  "+strings.Join(b.unresolved, "\n  "), nil)
}

// isStandardLibraryImport follows Go's rule: the first path element of a module path
// carries a dot, the standard library's never does. The module's own packages and those of the
// modules it requires are never the standard library, dot or not (module app, a replaced local
// module).
func (b *schemaBuilder) isStandardLibraryImport(importPath string) bool {
	if importPath == "" {
		return false
	}
	if withinModule(importPath, b.modulePath) {
		return false
	}
	for modulePath := range b.moduleVersions {
		if withinModule(importPath, modulePath) {
			return false
		}
	}

	first, _, _ := strings.Cut(importPath, "/")
	return !strings.Contains(first, ".")
}

// withinModule reports an import path of the module modulePath or one of its packages.
func withinModule(importPath string, modulePath string) bool {
	if modulePath == "" {
		return false
	}

	return importPath == modulePath || strings.HasPrefix(importPath, modulePath+"/")
}

func (b *schemaBuilder) schemaForExpr(expr ast.Expr, imports map[string]string, pkgPath string) map[string]any {
	if expr == nil {
		return map[string]any{
			"type": "object",
		}
	}

	switch t := expr.(type) {
	case *ast.Ident:
		return b.schemaForIdent(t.Name, pkgPath)
	case *ast.StarExpr:
		// A pointer field marshals as null when nil. Array items, map values, embedded
		// structs and results are dereferenced by their callers: those are not nullable.
		return nullableSchema(b.schemaForExpr(t.X, imports, pkgPath))
	case *ast.SelectorExpr:
		pkgIdent, ok := t.X.(*ast.Ident)
		if !ok {
			return map[string]any{"type": "object"}
		}
		importPath := imports[pkgIdent.Name]
		if importPath == "" {
			b.recordUnresolved(typeRef{ImportPath: pkgPath, Name: types.ExprString(t)}, "package "+pkgIdent.Name+" is not among its file's imports")
			return map[string]any{
				"type": "object",
			}
		}
		ref := typeRef{ImportPath: importPath, Name: t.Sel.Name}
		return b.schemaRef(ref)
	case *ast.ArrayType:
		// encoding/json writes a byte slice as base64, a byte array as an array of numbers.
		if t.Len == nil && b.isBase64Element(t.Elt, imports, pkgPath, 0) {
			return map[string]any{
				"type":   "string",
				"format": "byte",
			}
		}
		schema := map[string]any{
			"type":  "array",
			"items": b.schemaForExpr(derefExpr(t.Elt), imports, pkgPath),
		}
		length, ok := arrayLength(t)
		if ok {
			schema["minItems"] = length
			schema["maxItems"] = length
		}
		return schema
	case *ast.MapType:
		return map[string]any{
			"type":                 "object",
			"additionalProperties": b.schemaForExpr(derefExpr(t.Value), imports, pkgPath),
		}
	case *ast.StructType:
		// An anonymous struct into which an embedded type promotes MarshalJSON or MarshalText
		// is written as that type alone. A named one is refused when its component is built
		// (customMarshalReason); an anonymous one has no component, so it is refused here,
		// named by its fields.
		reason := b.promotedMarshaler(t, imports, pkgPath, false)
		if reason != "" {
			anonymous := typeRef{ImportPath: pkgPath, Name: types.ExprString(t)}
			b.recordUnresolved(anonymous, "anonymous struct "+reason+"; declare it as a named type and describe that type with Config.TypeSchemas")
			return map[string]any{
				"type": "object",
			}
		}
		schema, reason := b.schemaForStruct(t, imports, pkgPath)
		if reason != "" {
			anonymous := typeRef{ImportPath: pkgPath, Name: types.ExprString(t)}
			b.recordUnresolved(anonymous, "anonymous struct "+reason+"; declare it as a named type and describe that type with Config.TypeSchemas")
		}
		return schema
	case *ast.InterfaceType:
		// interface{} carries arbitrary JSON, like any.
		return map[string]any{}
	case *ast.IndexExpr:
		return b.schemaForInstantiation(t.X, expr, imports, pkgPath)
	case *ast.IndexListExpr:
		return b.schemaForInstantiation(t.X, expr, imports, pkgPath)
	default:
		return map[string]any{
			"type": "object",
		}
	}
}

// schemaForInstantiation is the schema of a generic type instantiation, Page[Item]. The
// generator does not substitute type parameters, so the generic type must be described with
// Config.TypeSchemas; every instantiation then shares that schema.
func (b *schemaBuilder) schemaForInstantiation(generic ast.Expr, expr ast.Expr, imports map[string]string, pkgPath string) map[string]any {
	ref, ok := typeRefFromExpr(generic, imports, pkgPath)
	if ok {
		_, described := b.typeSchemas[typeKey(ref)]
		if described {
			return b.schemaRef(ref)
		}
		// database/sql's Null[T] is the struct {V T, Valid bool}, inline since every
		// instantiation differs.
		index, isIndex := expr.(*ast.IndexExpr)
		if isSQLNull(ref) && isIndex {
			schema := sqlNullObject("V", b.schemaForExpr(index.Index, imports, pkgPath))
			required, _ := schema["required"].([]string)
			b.markDerived(required)
			return schema
		}
	} else {
		ref = typeRef{ImportPath: pkgPath, Name: types.ExprString(generic)}
	}

	b.recordUnresolved(ref, "generic instantiation "+types.ExprString(expr)+" is not supported; describe it with Config.TypeSchemas["+strconv.Quote(typeKey(ref))+"]")
	return map[string]any{
		"type": "object",
	}
}

func (b *schemaBuilder) schemaForIdent(name string, pkgPath string) map[string]any {
	if name == "complex64" || name == "complex128" {
		b.recordUnresolved(typeRef{ImportPath: pkgPath, Name: name}, "encoding/json cannot marshal a "+name+" (json: unsupported type), so a response holding one fails; leave the field out with `json:\"-\"`")
		return map[string]any{"type": "object"}
	}
	schema, ok := builtinSchema(name)
	if ok {
		return schema
	}
	// error, the predeclared interface, is written as the value it holds, or null.
	if name == "error" {
		return map[string]any{}
	}

	ref := typeRef{ImportPath: pkgPath, Name: name}
	return b.schemaRef(ref)
}

func builtinSchema(name string) (map[string]any, bool) {
	switch name {
	case "string":
		return map[string]any{"type": "string"}, true
	case "bool":
		return map[string]any{"type": "boolean"}, true
	case "int8", "int16", "int32":
		return map[string]any{"type": "integer", "format": "int32"}, true
	case "uint8", "uint16", "uint32":
		return map[string]any{"type": "integer", "format": "int32"}, true
	case "int", "int64":
		// Go's int is 64 bits on every platform these servers run on.
		return map[string]any{"type": "integer", "format": "int64"}, true
	case "uint", "uint64", "uintptr":
		return map[string]any{"type": "integer", "format": "int64"}, true
	case "float32":
		return map[string]any{"type": "number", "format": "float"}, true
	case "float64":
		return map[string]any{"type": "number", "format": "double"}, true
	case "byte":
		// A lone byte is a number; only a []byte is base64 (schemaForExpr).
		return map[string]any{"type": "integer", "format": "int32"}, true
	case "rune":
		return map[string]any{"type": "integer", "format": "int32"}, true
	case "any":
		// `any` carries arbitrary JSON, so it must not be constrained.
		return map[string]any{}, true
	case "complex64", "complex128":
		// Predeclared, but encoding/json cannot marshal them: schemaForIdent refuses them before
		// asking for a schema.
		return nil, true
	}
	return nil, false
}

func (b *schemaBuilder) wellKnownComponentSchema(ref typeRef) (map[string]any, bool) {
	if ref.ImportPath == paginationImportPath {
		switch ref.Name {
		case "CursorRequest":
			return map[string]any{
				"type": "object",
				"properties": map[string]any{
					"limit": map[string]any{
						"type":   "integer",
						"format": "int32",
					},
					"cursor": map[string]any{
						"type": "string",
					},
					"sort": map[string]any{
						"type": "string",
					},
					"include_count": map[string]any{
						"type": "boolean",
					},
					// Filter is `json:"-"`: server-side only, never read from the client.
				},
			}, true
		case "CursorResponse":
			return map[string]any{
				"type": "object",
				"properties": map[string]any{
					"next_cursor": map[string]any{
						"type": "string",
					},
					"has_next_page": map[string]any{
						"type": "boolean",
					},
					"total_count": map[string]any{
						"type":   "integer",
						"format": "int64",
					},
				},
				// HasNextPage has no omitempty; NextCursor and TotalCount do.
				"required": []string{"has_next_page"},
			}, true
		}
	}
	if ref.ImportPath == "github.com/vanclief/compose/drivers/databases/relational/postgres/query" && ref.Name == "Filter" {
		return map[string]any{
			"type": "object",
			"properties": map[string]any{
				"Field": map[string]any{
					"type": "string",
				},
				"Value": map[string]any{
					"type": "object",
				},
				"Comparison": map[string]any{
					"type": "string",
				},
			},
		}, true
	}
	if ref.ImportPath == "database/sql" {
		return sqlNullSchema(ref.Name)
	}

	return nil, false
}

// sqlNullValues are the value fields of database/sql's Null types, which define no
// MarshalJSON: encoding/json writes them as the struct they are, {"String": "", "Valid": false}.
var sqlNullValues = map[string]struct {
	Field  string
	Schema map[string]any
}{
	"NullBool":    {"Bool", map[string]any{"type": "boolean"}},
	"NullByte":    {"Byte", map[string]any{"type": "integer", "format": "int32"}},
	"NullFloat64": {"Float64", map[string]any{"type": "number", "format": "double"}},
	"NullInt16":   {"Int16", map[string]any{"type": "integer", "format": "int32"}},
	"NullInt32":   {"Int32", map[string]any{"type": "integer", "format": "int32"}},
	"NullInt64":   {"Int64", map[string]any{"type": "integer", "format": "int64"}},
	"NullString":  {"String", map[string]any{"type": "string"}},
	"NullTime":    {"Time", map[string]any{"type": "string", "format": "date-time"}},
}

// sqlNullSchema is the object encoding/json writes for a database/sql Null type: its value
// field and Valid, both always present.
func sqlNullSchema(name string) (map[string]any, bool) {
	value, ok := sqlNullValues[name]
	if !ok {
		return nil, false
	}

	return sqlNullObject(value.Field, cloneSchema(value.Schema)), true
}

func sqlNullObject(field string, value map[string]any) map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			field: value,
			"Valid": map[string]any{
				"type": "boolean",
			},
		},
		"required": []string{field, "Valid"},
	}
}

// isSQLNull reports database/sql's generic Null[T].
func isSQLNull(ref typeRef) bool {
	return ref.ImportPath == "database/sql" && ref.Name == "Null"
}

// componentOf is the component built from a Go type, "" when none is.
func (b *schemaBuilder) componentOf(importPath string, name string) string {
	ref := typeRef{ImportPath: importPath, Name: name}
	for _, component := range sortedKeys(b.origins) {
		_, isVariant := b.variantOf[component]
		if b.origins[component] == ref && !isVariant {
			return component
		}
	}

	return ""
}

func (b *schemaBuilder) wellKnownSchema(ref typeRef) (map[string]any, bool) {
	if ref.ImportPath == "time" && ref.Name == "Time" {
		return map[string]any{"type": "string", "format": "date-time"}, true
	}
	if ref.ImportPath == "github.com/google/uuid" && ref.Name == "UUID" {
		return map[string]any{"type": "string", "format": "uuid"}, true
	}
	if ref.ImportPath == "encoding/json" && ref.Name == "RawMessage" {
		// Raw JSON of any shape.
		return map[string]any{}, true
	}
	if ref.ImportPath == "encoding/json" && ref.Name == "Number" {
		// encoding/json writes a Number's text as a JSON number, an empty one as 0.
		return map[string]any{"type": "number"}, true
	}
	if ref.ImportPath == "mime/multipart" && ref.Name == "FileHeader" {
		// A file part of a multipart form.
		return map[string]any{"type": "string", "format": "binary"}, true
	}
	if ref.ImportPath == "time" && ref.Name == "Duration" {
		return map[string]any{"type": "integer", "format": "int64", "description": "nanoseconds"}, true
	}
	if ref.ImportPath == "net" && ref.Name == "IP" {
		// IP implements MarshalText.
		return map[string]any{"type": "string"}, true
	}
	if ref.ImportPath == "math/big" && ref.Name == "Int" {
		// Int's MarshalJSON writes a JSON number of any size.
		return map[string]any{"type": "integer"}, true
	}
	if ref.ImportPath == "log/slog" && ref.Name == "Level" {
		// Level's MarshalJSON writes its name as a JSON string, "INFO".
		return map[string]any{"type": "string"}, true
	}
	// Any other listed type with MarshalText and without MarshalJSON, netip.Addr or big.Float,
	// is the string MarshalText returns, as a type read from source is (buildSchema).
	if marshalsAsText(knownMarshalers[typeKey(ref)]) {
		return map[string]any{"type": "string"}, true
	}
	return nil, false
}

// isByteSlice reports a slice written with the predeclared byte or uint8 element, the raw body a
// decoder takes (byteSliceParams). A schema follows isBase64Element instead.
func isByteSlice(array *ast.ArrayType) bool {
	if array == nil {
		return false
	}
	ident, ok := array.Elt.(*ast.Ident)
	if !ok {
		return false
	}
	return ident.Name == "byte" || ident.Name == "uint8"
}

// isBase64Element reports a slice element encoding/json writes the slice of as one base64 string
// (newSliceEncoder): its kind is uint8 and a pointer to it has neither MarshalJSON nor
// MarshalText, as with byte, `type Level uint8`, an alias of byte, or a standard-library one such
// as elf.Class. With either method, a pointer receiver included, the slice is an array of what
// the method writes. Only the element type's own methods count: one declared from a byte type
// with a marshaler does not get them and is base64 too. Aliases are resolved (aliasTarget) and
// take no step of the chain of named types, which fails generation past maxDeclarationHops. A
// type whose source cannot be read and an instantiation of a generic type are not reported, so
// their slices stay arrays of them.
func (b *schemaBuilder) isBase64Element(expr ast.Expr, imports map[string]string, pkgPath string, depth int) bool {
	var ref typeRef
	switch t := expr.(type) {
	case *ast.Ident:
		_, builtin := builtinSchema(t.Name)
		if builtin {
			return t.Name == "byte" || t.Name == "uint8"
		}
		ref = typeRef{ImportPath: pkgPath, Name: t.Name}
	case *ast.SelectorExpr:
		pkgIdent, ok := t.X.(*ast.Ident)
		if !ok {
			return false
		}
		ref = typeRef{ImportPath: imports[pkgIdent.Name], Name: t.Sel.Name}
	default:
		return false
	}

	ref = b.aliasTarget(ref)
	listed, ok := knownUnderlyingExpr(ref)
	if ok {
		return depth > 0 && b.isBase64Element(listed, nil, "", depth+1)
	}
	if b.isListedStandardLibrary(ref) {
		return false
	}
	if depth >= maxDeclarationHops {
		b.recordLongChain(ref)
		return false
	}
	info, pkg, ok := b.namedDeclaration(ref)
	if !ok {
		return false
	}
	methods := pkg.Methods[ref.Name]
	if depth == 0 && (methods["MarshalJSON"] || methods["MarshalText"]) {
		return false
	}

	return b.isBase64Element(info.Spec.Type, info.Imports, pkg.ImportPath, depth+1)
}

// arrayLength is the length of a fixed-size array type written as an integer literal, [32]T.
func arrayLength(array *ast.ArrayType) (int, bool) {
	lit, ok := array.Len.(*ast.BasicLit)
	if !ok || lit.Kind != token.INT {
		return 0, false
	}
	length, err := strconv.ParseInt(lit.Value, 0, 64)
	if err != nil {
		return 0, false
	}

	return int(length), true
}

// schemaMembers returns the object members that carry a schema's properties:
// the schema itself, or its allOf members when the builder split an embedded
// type out.
func schemaMembers(schema map[string]any) []map[string]any {
	allOf, ok := schema["allOf"].([]any)
	if !ok {
		return []map[string]any{schema}
	}

	var members []map[string]any
	for _, entry := range allOf {
		member, memberOK := entry.(map[string]any)
		if memberOK {
			members = append(members, member)
		}
	}
	return members
}

func cloneSchema(schema map[string]any) map[string]any {
	value := cloneSchemaValue(schema)
	cloned, ok := value.(map[string]any)
	if !ok {
		return map[string]any{}
	}

	return cloned
}

func cloneSchemaValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		cloned := make(map[string]any, len(typed))
		for key, nested := range typed {
			cloned[key] = cloneSchemaValue(nested)
		}
		return cloned
	case map[string]map[string]any:
		cloned := make(map[string]map[string]any, len(typed))
		for key, nested := range typed {
			cloned[key] = cloneSchema(nested)
		}
		return cloned
	case []any:
		cloned := make([]any, len(typed))
		for i := range typed {
			cloned[i] = cloneSchemaValue(typed[i])
		}
		return cloned
	default:
		return value
	}
}

// schemaForStruct is the schema of a struct declaration, and a reason, "embeds ...", when it
// cannot be described (mergedStructSchema); the caller fails generation with it.
func (b *schemaBuilder) schemaForStruct(st *ast.StructType, imports map[string]string, pkgPath string) (map[string]any, string) {
	// Embedded structs are allOf members, unless a name collides, a pointer is embedded or an
	// embedded struct marshals itself: then the struct gets the one property set encoding/json
	// resolves (mergedStructSchema).
	merged, ok, reason := b.mergedStructSchema(st, imports, pkgPath)
	if reason != "" {
		return map[string]any{"type": "object"}, reason
	}
	if ok {
		return merged, ""
	}

	props := map[string]any{}
	var required []string
	var allOf []any

	// A value-typed field without omitempty always has its key in the serialized payload, so
	// it is required. That holds for what the server sends, not for what a request may leave
	// out (Go reads a missing key as the zero value): splitRequestSide strips the list again on
	// the request side once the request graph is known.
	table := ""

	for _, field := range st.Fields.List {
		if field == nil {
			continue
		}
		flaggedDeprecated := hasDeprecatedParagraph(field)

		var names []string
		if len(field.Names) == 0 {
			// bun.BaseModel marshals nothing; its tag names the model's table.
			if isBunBaseModel(field.Type, imports) {
				table = bunTable(field)
				continue
			}
			// encoding/json inlines an embedded struct's fields unless the tag names it; any
			// other embedded type is a field named after the type.
			tagName, skipped := jsonTagName(field)
			if skipped {
				continue
			}
			isStruct := b.isStructType(derefExpr(field.Type), imports, pkgPath, 0)
			if isStruct && tagName == "" {
				embedded := b.schemaForExpr(derefExpr(field.Type), imports, pkgPath)
				allOf = append(allOf, embedded)
				continue
			}
			typeName := embeddedTypeName(field.Type)
			// An embedded unexported non-struct type is ignored.
			if typeName == "" || (!isStruct && !ast.IsExported(typeName)) {
				continue
			}
			names = append(names, typeName)
		} else {
			for _, ident := range field.Names {
				if ast.IsExported(ident.Name) {
					names = append(names, ident.Name)
				}
			}
		}

		for _, goName := range names {
			name := jsonFieldName(goName, field)
			if name == "" {
				continue
			}

			schema := b.fieldSchema(field, imports, pkgPath)
			if flaggedDeprecated {
				schema = deprecatedSchema(schema)
			}
			if b.docComments {
				schema = describedSchema(schema, fieldDoc(field))
			}
			props[name] = schema

			// Nil-able fields (pointers, slices, maps — directly or through a named
			// type) marshal as null and stay optional; when in doubt the field stays
			// optional, never falsely required.
			if !jsonFieldOmitsEmpty(field) && !b.isNilableFieldType(field.Type, imports, pkgPath, 0) {
				required = append(required, name)
			}
		}
	}

	b.markDerived(required)

	var schema map[string]any
	if len(allOf) > 0 {
		if len(props) > 0 {
			member := map[string]any{
				"type":       "object",
				"properties": props,
			}
			if len(required) > 0 {
				member["required"] = required
			}
			allOf = append(allOf, member)
		}
		schema = map[string]any{
			"allOf": allOf,
		}
	} else {
		schema = map[string]any{
			"type":       "object",
			"properties": props,
		}
		if len(required) > 0 {
			schema["required"] = required
		}
	}
	if table != "" {
		schema["x-table"] = table
	}

	return schema, ""
}

// isNilableFieldType reports whether a struct field's type can marshal as JSON
// null: pointers, slices, maps, and interfaces, either directly or through a
// named type (resolved via the package loader; a chain of more than
// maxDeclarationHops named types fails generation). Unresolvable types count as
// nil-able so the field degrades to optional rather than being falsely required.
func (b *schemaBuilder) isNilableFieldType(expr ast.Expr, imports map[string]string, pkgPath string, depth int) bool {
	if expr == nil {
		return true
	}

	switch t := expr.(type) {
	case *ast.StarExpr, *ast.MapType, *ast.InterfaceType, *ast.ChanType, *ast.FuncType, *ast.Ellipsis:
		return true
	case *ast.ArrayType:
		// A slice can be nil; a fixed-size array never is.
		return t.Len == nil
	case *ast.StructType:
		return false
	case *ast.IndexExpr:
		// database/sql's Null[T] is a struct; any other instantiation is not resolved.
		ref, ok := typeRefFromExpr(t.X, imports, pkgPath)
		return !ok || !isSQLNull(ref)
	case *ast.Ident:
		_, builtin := builtinSchema(t.Name)
		if builtin {
			return false
		}
		return b.isNilableNamedType(typeRef{ImportPath: pkgPath, Name: t.Name}, depth)
	case *ast.SelectorExpr:
		pkgIdent, ok := t.X.(*ast.Ident)
		if !ok {
			return true
		}
		return b.isNilableNamedType(typeRef{ImportPath: imports[pkgIdent.Name], Name: t.Sel.Name}, depth)
	default:
		return true
	}
}

func (b *schemaBuilder) isNilableNamedType(ref typeRef, depth int) bool {
	// An alias is the type it names and takes no step of the depth limit.
	ref = b.aliasTarget(ref)
	// A type declared from a listed type is written by knownUnderlying's type.
	listed, ok := knownUnderlyingExpr(ref)
	if ok && depth > 0 {
		return b.isNilableFieldType(listed, nil, "", depth+1)
	}
	override, ok := b.typeSchemas[typeKey(ref)]
	if ok && !isExtension(override) {
		return override["nullable"] == true
	}

	// Well-known mapped types (time.Time, uuid.UUID, enum overrides) are value
	// types that always serialize, as are database/sql's Null structs.
	schema, ok := b.wellKnownSchema(ref)
	if ok {
		return schema["nullable"] == true
	}
	_, ok = sqlNullValues[ref.Name]
	if ok && ref.ImportPath == "database/sql" {
		return false
	}
	// The other listed standard-library types, url.URL or http.Header, stay optional.
	if b.isListedStandardLibrary(ref) {
		return true
	}
	if depth >= maxDeclarationHops {
		b.recordLongChain(ref)
		return true
	}
	info, pkg, ok := b.namedDeclaration(ref)
	if !ok {
		return true
	}

	return b.isNilableFieldType(info.Spec.Type, info.Imports, pkg.ImportPath, depth+1)
}

// fieldSchema is a struct field's schema. The `,string` option puts a boolean, number or string
// (or a pointer to one) inside a JSON string.
func (b *schemaBuilder) fieldSchema(field *ast.Field, imports map[string]string, pkgPath string) map[string]any {
	if containsString(jsonTagOptions(field), "string") {
		elem := field.Type
		star, isPointer := elem.(*ast.StarExpr)
		if isPointer {
			elem = star.X
		}
		if b.isQuotableType(elem, imports, pkgPath, 0) {
			schema := map[string]any{"type": "string"}
			if isPointer {
				schema = nullableSchema(schema)
			}
			return schema
		}
	}

	return b.schemaForExpr(field.Type, imports, pkgPath)
}

// standardLibraryNilCollections are the standard library's named maps without a marshaler
// that requests and responses commonly carry. The generator does not model the standard
// library's types, so these are listed.
var standardLibraryNilCollections = map[string]bool{
	"net/url.Values":           true,
	"net/http.Header":          true,
	"net/textproto.MIMEHeader": true,
}

// quotableKinds are the predeclared types the `,string` option applies to.
var quotableKinds = map[string]bool{
	"bool": true, "string": true,
	"int": true, "int8": true, "int16": true, "int32": true, "int64": true,
	"uint": true, "uint8": true, "uint16": true, "uint32": true, "uint64": true, "uintptr": true,
	"byte": true, "rune": true, "float32": true, "float64": true,
}

// isQuotableType reports a type whose kind the `,string` option applies to: a boolean, number
// or string, directly or through a named type without a JSON or text marshaler (one with a
// marshaler writes its own value). A type it cannot resolve is not, nor one described in
// Config.TypeSchemas, unless it is declared from, not as, a described type with a marshaler
// (describedMarshaler); one declared from a type knownUnderlying lists is that type,
// `type Severity slog.Level` an int, and a standard-library type the generator does not list is
// read from GOROOT (namedDeclaration), `time.Month` an int. Aliases are resolved (aliasTarget)
// and take no step of the chain of named types, which fails generation past maxDeclarationHops.
func (b *schemaBuilder) isQuotableType(expr ast.Expr, imports map[string]string, pkgPath string, depth int) bool {
	var ref typeRef
	switch t := expr.(type) {
	case *ast.Ident:
		_, builtin := builtinSchema(t.Name)
		if builtin {
			return quotableKinds[t.Name]
		}
		ref = typeRef{ImportPath: pkgPath, Name: t.Name}
	case *ast.SelectorExpr:
		pkgIdent, ok := t.X.(*ast.Ident)
		if !ok {
			return false
		}
		ref = typeRef{ImportPath: imports[pkgIdent.Name], Name: t.Sel.Name}
	default:
		return false
	}

	ref = b.aliasTarget(ref)
	if ref == (typeRef{ImportPath: "time", Name: "Duration"}) || ref == (typeRef{ImportPath: "encoding/json", Name: "Number"}) {
		return true
	}
	_, described := b.typeSchemas[typeKey(ref)]
	if described && (depth == 0 || !b.describedMarshaler(ref)) {
		return false
	}
	listed, ok := knownUnderlyingExpr(ref)
	if ok {
		return depth > 0 && b.isQuotableType(listed, nil, "", depth+1)
	}
	if b.isListedStandardLibrary(ref) {
		return false
	}
	if depth >= maxDeclarationHops {
		b.recordLongChain(ref)
		return false
	}
	info, pkg, ok := b.namedDeclaration(ref)
	if !ok {
		return false
	}
	// Only the field's own type's methods count: a type declared from another named type does
	// not inherit that type's marshalers. An alias is that type, so its methods count.
	methods := pkg.Methods[ref.Name]
	if depth == 0 && (methods["MarshalJSON"] || methods["MarshalText"]) {
		return false
	}

	return b.isQuotableType(info.Spec.Type, info.Imports, pkg.ImportPath, depth+1)
}

// isStructType reports a struct type, directly or through a named type, whose fields encoding/json
// promotes when it is embedded. A type it cannot resolve counts as one, as embedded types always
// did. A standard-library type is read from GOROOT (namedDeclaration) unless the generator lists
// it: knownUnderlying gives the kind of uuid.UUID, a [16]byte, and of its siblings, and of the
// other listed types time.Duration, the maps standardLibraryNilCollections lists and the marshaler
// interfaces are not structs. Aliases are
// resolved (aliasTarget) and take no step of the chain of named types, which fails generation
// past maxDeclarationHops.
func (b *schemaBuilder) isStructType(expr ast.Expr, imports map[string]string, pkgPath string, depth int) bool {
	var ref typeRef
	switch t := expr.(type) {
	case *ast.StructType:
		return true
	case *ast.Ident:
		_, builtin := builtinSchema(t.Name)
		if builtin || t.Name == "error" {
			return false
		}
		ref = typeRef{ImportPath: pkgPath, Name: t.Name}
	case *ast.SelectorExpr:
		pkgIdent, ok := t.X.(*ast.Ident)
		if !ok {
			return true
		}
		ref = typeRef{ImportPath: imports[pkgIdent.Name], Name: t.Sel.Name}
	case *ast.IndexExpr, *ast.IndexListExpr:
		return true
	default:
		return false
	}

	// A described type is the kind its description says, unless the description is of a
	// marshaler's output (describedMarshaler).
	ref = b.aliasTarget(ref)
	override, described := b.typeSchemas[typeKey(ref)]
	if described && !isExtension(override) && !b.describedMarshaler(ref) {
		kind, typed := override["type"]
		return !typed || kind == "object"
	}
	listed, ok := knownUnderlyingExpr(ref)
	if ok {
		_, isStruct := listed.(*ast.StructType)
		return isStruct
	}
	if b.isListedStandardLibrary(ref) {
		_, iface := marshalerInterfaces[typeKey(ref)]
		duration := ref == typeRef{ImportPath: "time", Name: "Duration"}
		return !iface && !duration && !standardLibraryNilCollections[typeKey(ref)]
	}
	if depth >= maxDeclarationHops {
		b.recordLongChain(ref)
		return true
	}
	info, pkg, ok := b.namedDeclaration(ref)
	if !ok {
		return true
	}

	return b.isStructType(info.Spec.Type, info.Imports, pkg.ImportPath, depth+1)
}

// embeddedTypeName is the field name Go gives an embedded field: its type's name.
func embeddedTypeName(expr ast.Expr) string {
	switch t := derefExpr(expr).(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return t.Sel.Name
	case *ast.IndexExpr:
		return embeddedTypeName(t.X)
	case *ast.IndexListExpr:
		return embeddedTypeName(t.X)
	}

	return ""
}

func jsonFieldOmitsEmpty(field *ast.Field) bool {
	for _, option := range jsonTagOptions(field) {
		if option == "omitempty" || option == "omitzero" {
			return true
		}
	}

	return false
}

// jsonFieldName is the JSON key of the field goName of a declaration: its json tag's name, else
// goName; "" for a `json:"-"` field.
func jsonFieldName(goName string, field *ast.Field) string {
	name, skipped := jsonTagName(field)
	if skipped {
		return ""
	}
	if name == "" {
		return goName
	}

	return name
}

// jsonTagName is the name a field's json tag gives, "" when it names none, and whether the tag
// is `json:"-"`, which drops the field (`json:"-,"` names it "-" instead).
func jsonTagName(field *ast.Field) (string, bool) {
	jsonTag := fieldJSONTag(field)
	if jsonTag == "-" {
		return "", true
	}

	name, _, _ := strings.Cut(jsonTag, ",")
	return name, false
}

// jsonTagOptions are the options after the name of a field's json tag.
func jsonTagOptions(field *ast.Field) []string {
	parts := strings.Split(fieldJSONTag(field), ",")

	return parts[1:]
}

func fieldJSONTag(field *ast.Field) string {
	if field == nil || field.Tag == nil {
		return ""
	}

	tag := strings.Trim(field.Tag.Value, "`")
	return reflect.StructTag(tag).Get("json")
}

// hasDeprecatedParagraph reports a field documented the Go way, with a paragraph that starts
// "Deprecated:". Such a field is still served and stays in the contract, flagged deprecated, so
// clients keep compiling while they move off it. Any other mention of deprecation (a TODO, a
// note) changes nothing: the field is served, so it is documented like any other.
func hasDeprecatedParagraph(field *ast.Field) bool {
	if field == nil || field.Doc == nil {
		return false
	}

	for _, line := range strings.Split(field.Doc.Text(), "\n") {
		if strings.HasPrefix(line, "Deprecated: ") {
			return true
		}
	}

	return false
}

// deprecatedSchema flags a property schema deprecated.
func deprecatedSchema(schema map[string]any) map[string]any {
	flagged := siblingSafe(schema)
	flagged["deprecated"] = true

	return flagged
}

// nullableSchema marks a pointer field's schema nullable. An empty schema already admits null.
func nullableSchema(schema map[string]any) map[string]any {
	if len(schema) == 0 {
		return schema
	}

	flagged := siblingSafe(schema)
	flagged["nullable"] = true

	return flagged
}

// siblingSafe returns a copy of schema that sibling keywords (nullable, description,
// deprecated) can be set on. OpenAPI 3.0 ignores every keyword beside a $ref, so a bare
// reference is wrapped in allOf first; fillNullableTypes later gives a nullable wrapper the
// type 3.0 requires beside nullable.
func siblingSafe(schema map[string]any) map[string]any {
	_, isRef := schema["$ref"]
	if isRef {
		return map[string]any{
			"allOf": []any{schema},
		}
	}

	copied := make(map[string]any, len(schema)+1)
	for key, value := range schema {
		copied[key] = value
	}

	return copied
}

// derefExpr strips the pointers off a type expression.
func derefExpr(expr ast.Expr) ast.Expr {
	for {
		star, ok := expr.(*ast.StarExpr)
		if !ok {
			return expr
		}
		expr = star.X
	}
}

// fillNullableTypes gives every nullable allOf wrapper of a single $ref the type of the
// component it references: OpenAPI 3.0 only honours nullable beside a type. Wrappers whose
// component has no single type (a oneOf of mixed types) are left as they are.
func fillNullableTypes(node any, schemas map[string]map[string]any) {
	switch typed := node.(type) {
	case map[string]any:
		_, hasType := typed["type"]
		allOf, isAllOf := typed["allOf"].([]any)
		if typed["nullable"] == true && !hasType && isAllOf && len(allOf) == 1 {
			member, _ := allOf[0].(map[string]any)
			name := componentNameFromSchema(member)
			if name != "" {
				kind := componentType(name, schemas, map[string]bool{})
				if kind != "" {
					typed["type"] = kind
				}
			}
		}
		for _, value := range typed {
			fillNullableTypes(value, schemas)
		}
	case map[string]map[string]any:
		for _, value := range typed {
			fillNullableTypes(value, schemas)
		}
	case []any:
		for _, value := range typed {
			fillNullableTypes(value, schemas)
		}
	}
}

// componentType is the JSON type a component's values have, following references and
// compositions whose members agree, or "" when it has none.
func componentType(name string, schemas map[string]map[string]any, seen map[string]bool) string {
	if seen[name] {
		return ""
	}
	seen[name] = true

	return schemaType(schemas[name], schemas, seen)
}

func schemaType(schema map[string]any, schemas map[string]map[string]any, seen map[string]bool) string {
	if schema == nil {
		return ""
	}
	kind, ok := schema["type"].(string)
	if ok {
		return kind
	}
	name := componentNameFromSchema(schema)
	if name != "" {
		return componentType(name, schemas, seen)
	}

	for _, keyword := range []string{"allOf", "oneOf", "anyOf"} {
		members, ok := schema[keyword].([]any)
		if !ok || len(members) == 0 {
			continue
		}
		agreed := ""
		for _, entry := range members {
			member, _ := entry.(map[string]any)
			memberType := schemaType(member, schemas, seen)
			if memberType == "" || (agreed != "" && memberType != agreed) {
				return ""
			}
			agreed = memberType
		}
		return agreed
	}

	return ""
}

func readModuleInfo(path string) (string, map[string]string, error) {

	data, err := os.ReadFile(path)
	if err != nil {
		return "", nil, ez.Wrap(err)
	}

	lines := strings.Split(string(data), "\n")
	modulePath := ""
	requires := map[string]string{}
	inRequireBlock := false

	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "//") {
			continue
		}

		if strings.HasPrefix(line, "module ") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				modulePath = fields[1]
			}
			continue
		}

		if strings.HasPrefix(line, "require (") {
			inRequireBlock = true
			continue
		}

		if inRequireBlock {
			if line == ")" {
				inRequireBlock = false
				continue
			}
			line = stripInlineComment(line)
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				requires[fields[0]] = fields[1]
			}
			continue
		}

		if strings.HasPrefix(line, "require ") {
			line = strings.TrimPrefix(line, "require ")
			line = stripInlineComment(line)
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				requires[fields[0]] = fields[1]
			}
		}
	}

	if modulePath == "" {
		return "", nil, ez.New(ez.EINTERNAL, "module path not found", nil)
	}

	return modulePath, requires, nil
}

func stripInlineComment(line string) string {
	idx := strings.Index(line, "//")
	if idx < 0 {
		return line
	}
	return strings.TrimSpace(line[:idx])
}

func (b *schemaBuilder) loadPackage(importPath string) (*packageInfo, error) {

	if importPath == "" {
		return nil, ez.New(ez.EINTERNAL, "empty import path", nil)
	}

	pkg, ok := b.packages[importPath]
	if ok {
		return pkg, nil
	}

	dir, err := b.resolveImportPath(importPath)
	if err != nil {
		return nil, ez.Wrap(err)
	}

	_, err = os.Stat(dir)
	if err != nil {
		// No cause: ez renders the cause instead of the message, and the hint matters more.
		return nil, ez.New(ez.ENOTFOUND, "package source not found at "+dir+" (run `go mod download` for dependencies)", nil)
	}

	pkg, err = b.parsePackage(importPath, dir, false)
	if err != nil {
		return nil, ez.Wrap(err)
	}
	b.packages[importPath] = pkg

	return pkg, nil
}

// loadStandardLibrary reads a standard-library package from GOROOT's source, which is how every
// standard-library type the generator does not list is described (standardLibrarySchema) and
// walked: its kind, whether it is an interface or nil, which methods it has and which types it
// embeds. Its files are those the build constraints select for the platform the generator runs
// on (parsePackage). It is cached apart from loadPackage's packages, which the generator does
// not read the standard library from. Its error's message says what could not be read and why.
func (b *schemaBuilder) loadStandardLibrary(importPath string) (*packageInfo, error) {
	pkg, ok := b.standardLibrary[importPath]
	if ok {
		return pkg, nil
	}

	goroot, err := b.gorootDir()
	if err != nil {
		return nil, ez.Wrap(err)
	}
	dir := filepath.Join(goroot, "src", filepath.FromSlash(importPath))
	_, err = os.Stat(dir)
	if err != nil {
		return nil, ez.New(ez.ENOTFOUND, "GOROOT's source of "+importPath+" not found at "+dir+" (GOROOT is "+goroot+")", nil)
	}

	pkg, err = b.parsePackage(importPath, dir, true)
	if err != nil {
		return nil, ez.Wrap(err)
	}
	if b.standardLibrary == nil {
		b.standardLibrary = map[string]*packageInfo{}
	}
	b.standardLibrary[importPath] = pkg

	return pkg, nil
}

// gorootDir is GOROOT: the GOROOT environment variable, else what `go env GOROOT` prints. Its
// error says which of the two failed and why.
func (b *schemaBuilder) gorootDir() (string, error) {
	if b.goroot == "" && b.gorootErr == nil {
		b.goroot = os.Getenv("GOROOT")
		if b.goroot == "" {
			goroot, err := goEnv("GOROOT")
			if err != nil {
				b.gorootErr = ez.New(ez.ENOTFOUND, "GOROOT is not set and "+ez.ErrorMessage(err), nil)
			}
			b.goroot = goroot
		}
		if b.goroot == "" && b.gorootErr == nil {
			b.gorootErr = ez.New(ez.ENOTFOUND, "GOROOT is not set and `go env GOROOT` printed nothing", nil)
		}
	}
	if b.gorootErr != nil {
		return "", ez.Wrap(b.gorootErr)
	}

	return b.goroot, nil
}

// parsePackage reads the Go files of the package importPath in dir, leaving out its tests. For
// the standard library only the files its build constraints select for the platform the
// generator runs on (GOOS/GOARCH) are read, without comments. A file that cannot be read, whose
// build constraints cannot be evaluated or that does not parse fails it, with an error whose
// message names the file and the cause, rather than being left out: a left-out file could hold a
// method that changes a type's schema.
func (b *schemaBuilder) parsePackage(importPath string, dir string, standardLibrary bool) (*packageInfo, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, ez.New(ez.EINTERNAL, "cannot list the Go files in "+dir+": "+err.Error(), err)
	}
	if len(files) == 0 {
		return nil, ez.New(ez.ENOTFOUND, "no Go files in "+dir, nil)
	}

	mode := parser.ParseComments
	if standardLibrary {
		mode = parser.SkipObjectResolution
	}

	fset := token.NewFileSet()
	types := map[string]typeSpecInfo{}
	consts := map[string][]stringConst{}
	closedSets := map[string]bool{}
	closedSetMembers := map[string]bool{}
	methods := map[string]map[string]bool{}
	stringConsts := map[string]string{}
	packageDoc := ""
	var funcs []funcInfo

	for _, filePath := range files {
		if strings.HasSuffix(filePath, "_test.go") {
			continue
		}
		if standardLibrary {
			match, err := build.Default.MatchFile(dir, filepath.Base(filePath))
			if err != nil {
				return nil, ez.New(ez.EINVALID, "cannot evaluate the build constraints of "+filePath+": "+err.Error(), err)
			}
			if !match {
				continue
			}
		}
		file, err := parser.ParseFile(fset, filePath, nil, mode)
		if err != nil {
			// The parser's error starts with the file, line and column.
			return nil, ez.New(ez.EINVALID, "cannot parse "+err.Error(), err)
		}

		imports := b.importMap(file)
		if packageDoc == "" {
			packageDoc = commentText(file.Doc)
		}

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if ok {
				funcs = append(funcs, funcInfo{
					Decl:    fn,
					Imports: imports,
				})
				receiver := receiverTypeName(fn)
				if receiver != "" {
					if methods[receiver] == nil {
						methods[receiver] = map[string]bool{}
					}
					methods[receiver][fn.Name.Name] = true
				}
				continue
			}

			gen, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}

			switch gen.Tok {
			case token.TYPE:
				for _, spec := range gen.Specs {
					typeSpec, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					doc := typeSpec.Doc
					if doc == nil && len(gen.Specs) == 1 {
						doc = gen.Doc
					}
					types[typeSpec.Name.Name] = typeSpecInfo{
						Spec:    typeSpec,
						Imports: imports,
						Doc:     doc,
					}
				}
			case token.CONST:
				collectTypedStringConsts(gen, consts)
				collectStringConsts(gen, stringConsts)
			case token.VAR:
				collectClosedSetTypes(gen, closedSets, closedSetMembers)
			}
		}
	}

	// A method declared on an alias, `func (a A) M()` with `type A = T`, is T's: it is recorded
	// under T too. Methods are declared in the package of their type, so the chain stays in it.
	for _, name := range sortedKeys(methods) {
		target := name
		for hops := 0; hops < maxAliasHops; hops++ {
			info, ok := types[target]
			if !ok || !info.Spec.Assign.IsValid() {
				break
			}
			ident, ok := info.Spec.Type.(*ast.Ident)
			if !ok {
				break
			}
			target = ident.Name
		}
		if target == name {
			continue
		}
		if methods[target] == nil {
			methods[target] = map[string]bool{}
		}
		for method := range methods[name] {
			methods[target][method] = true
		}
	}

	enums := map[string][]string{}
	enumDocs := map[string][]string{}
	enumNames := map[string][]string{}
	for name := range closedSets {
		seen := map[string]bool{}
		for _, constant := range consts[name] {
			if constant.Value == "" && !closedSetMembers[constant.Name] {
				continue
			}
			if seen[constant.Value] {
				continue
			}
			seen[constant.Value] = true
			enums[name] = append(enums[name], constant.Value)
			enumDocs[name] = append(enumDocs[name], constant.Doc)
			enumNames[name] = append(enumNames[name], constant.Name)
		}
	}

	return &packageInfo{
		ImportPath: importPath,
		Dir:        dir,
		Types:      types,
		Funcs:      funcs,
		Enums:      enums,
		EnumDocs:   enumDocs,
		EnumNames:  enumNames,
		Methods:    methods,
		Consts:     stringConsts,
		Doc:        packageDoc,
	}, nil
}

// receiverTypeName is the type a method is declared on, T for `(t T)`, `(t *T)`, `(t T[A])` and
// `(t T[A, B])`, "" for a function.
func receiverTypeName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return ""
	}
	expr := genericType(derefExpr(fn.Recv.List[0].Type))
	ident, ok := expr.(*ast.Ident)
	if !ok {
		return ""
	}

	return ident.Name
}

// genericType is the generic type an instantiation, G[A] or G[A, B], instantiates; expr itself
// for any other expression.
func genericType(expr ast.Expr) ast.Expr {
	switch t := expr.(type) {
	case *ast.IndexExpr:
		return t.X
	case *ast.IndexListExpr:
		return t.X
	}

	return expr
}

// collectStringConsts records every `Name = "value"` constant, typed or not.
func collectStringConsts(gen *ast.GenDecl, out map[string]string) {
	for _, spec := range gen.Specs {
		valueSpec, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		for i, value := range valueSpec.Values {
			if i >= len(valueSpec.Names) {
				break
			}
			text, ok := stringLiteralValue(value)
			if ok {
				out[valueSpec.Names[i].Name] = text
			}
		}
	}
}

// collectTypedStringConsts records `Name T = "value"` constants under T.
func collectTypedStringConsts(gen *ast.GenDecl, out map[string][]stringConst) {
	for _, spec := range gen.Specs {
		valueSpec, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		typeIdent, ok := valueSpec.Type.(*ast.Ident)
		if !ok {
			continue
		}
		for i, value := range valueSpec.Values {
			if i >= len(valueSpec.Names) {
				break
			}
			lit, ok := value.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			text, err := strconv.Unquote(lit.Value)
			if err != nil {
				continue
			}
			out[typeIdent.Name] = append(out[typeIdent.Name], stringConst{
				Name:  valueSpec.Names[i].Name,
				Value: text,
				Doc:   constDoc(valueSpec, valueSpec.Names[i].Name),
			})
		}
	}
}

// collectClosedSetTypes marks the element type T and direct constant members of
// every `enums.Set([]T{...})` (or any expression building a []T inside the call).
// The set is what Validate() checks, so it distinguishes closed enums from
// strings with a few constants and identifies accepted empty-string values.
func collectClosedSetTypes(gen *ast.GenDecl, out map[string]bool, members map[string]bool) {
	for _, spec := range gen.Specs {
		valueSpec, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		for _, value := range valueSpec.Values {
			ast.Inspect(value, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || selector.Sel == nil || selector.Sel.Name != "Set" {
					return true
				}
				pkgIdent, ok := selector.X.(*ast.Ident)
				if !ok || pkgIdent.Name != "enums" {
					return true
				}
				for _, arg := range call.Args {
					ast.Inspect(arg, func(inner ast.Node) bool {
						switch typed := inner.(type) {
						case *ast.ArrayType:
							elt, ok := typed.Elt.(*ast.Ident)
							if ok {
								out[elt.Name] = true
							}
						case *ast.Ident:
							members[typed.Name] = true
						}
						return true
					})
				}
				return true
			})
		}
	}
}

func (b *schemaBuilder) resolveImportPath(importPath string) (string, error) {

	if strings.HasPrefix(importPath, b.modulePath) {
		rel := strings.TrimPrefix(importPath, b.modulePath)
		rel = strings.TrimPrefix(rel, "/")
		return filepath.Join(b.repoRoot, filepath.FromSlash(rel)), nil
	}

	if b.modCache == "" {
		return "", ez.New(ez.EINTERNAL, "module cache path not set", nil)
	}

	modulePath, version := b.matchModule(importPath)
	if modulePath == "" || version == "" {
		return "", ez.New(ez.EINTERNAL, "module not found for import "+importPath, nil)
	}

	rel := strings.TrimPrefix(importPath, modulePath)
	rel = strings.TrimPrefix(rel, "/")
	escaped := escapeModulePath(modulePath)

	dir := filepath.Join(b.modCache, escaped+"@"+version, filepath.FromSlash(rel))
	return dir, nil
}

func (b *schemaBuilder) matchModule(importPath string) (string, string) {
	match := ""
	version := ""

	for modulePath, moduleVersion := range b.moduleVersions {
		if !strings.HasPrefix(importPath, modulePath) {
			continue
		}
		if len(modulePath) > len(match) {
			match = modulePath
			version = moduleVersion
		}
	}

	return match, version
}

func escapeModulePath(modulePath string) string {
	var out strings.Builder
	out.Grow(len(modulePath))

	for _, r := range modulePath {
		if r >= 'A' && r <= 'Z' {
			out.WriteByte('!')
			out.WriteRune(r + ('a' - 'A'))
			continue
		}
		out.WriteRune(r)
	}

	return out.String()
}
