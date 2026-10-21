package generator

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
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

func goEnv(name string) (string, error) {
	out, err := exec.Command("go", "env", name).Output()
	if err != nil {
		return "", ez.Wrap(err)
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

	// Standard-library types are not modeled; they stay opaque objects.
	if b.isStandardLibraryImport(ref.ImportPath) {
		return extendedSchema(map[string]any{"type": "object"}, override, b)
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

	// A struct that marshals itself has whatever shape its MarshalJSON writes; its fields say
	// nothing reliable about it, so the generator refuses to guess.
	if !overridden {
		reason := b.customMarshalReason(pkg, info, ref)
		if reason != "" {
			b.recordUnresolved(ref, reason)
			return map[string]any{
				"type": "object",
			}
		}
	}

	schema := b.schemaForExpr(info.Spec.Type, info.Imports, pkg.ImportPath)

	values, isEnum := pkg.Enums[ref.Name]
	if isEnum && schema["type"] == "string" {
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

// customMarshalReason explains why a struct type's fields do not describe its JSON: it defines
// MarshalJSON, or embeds a type that does and so marshals as that type alone. "" when its
// fields do.
func (b *schemaBuilder) customMarshalReason(pkg *packageInfo, info typeSpecInfo, ref typeRef) string {
	structure, ok := info.Spec.Type.(*ast.StructType)
	if !ok {
		return ""
	}

	hint := "; describe it with Config.TypeSchemas[" + strconv.Quote(typeKey(ref)) + "]"
	if pkg.Methods[ref.Name]["MarshalJSON"] {
		return "type " + ref.Name + " defines MarshalJSON" + hint
	}

	for _, field := range structure.Fields.List {
		if len(field.Names) > 0 {
			continue
		}
		embedded, ok := typeRefFromExpr(derefExpr(field.Type), info.Imports, pkg.ImportPath)
		if !ok {
			continue
		}
		_, described := b.typeSchemas[typeKey(embedded)]
		if described {
			continue
		}
		if selfMarshalingTypes[typeKey(embedded)] {
			return "type " + ref.Name + " embeds " + embedded.Name + ", which marshals itself, so it marshals as " + embedded.Name + " alone" + hint
		}
		if b.isStandardLibraryImport(embedded.ImportPath) {
			continue
		}
		embeddedPkg, err := b.loadPackage(embedded.ImportPath)
		if err != nil {
			continue
		}
		if embeddedPkg.Methods[embedded.Name]["MarshalJSON"] {
			return "type " + ref.Name + " embeds " + embedded.Name + ", which defines MarshalJSON, so it marshals as " + embedded.Name + " alone" + hint
		}
	}

	return ""
}

// selfMarshalingTypes are the well-known types (wellKnownSchema) whose JSON or text marshaler an
// embedding struct inherits, so that struct marshals as the embedded value alone. Their source
// is not loaded (the standard library) or not required, so their methods are not looked up.
var selfMarshalingTypes = map[string]bool{
	"time.Time":                   true,
	"encoding/json.RawMessage":    true,
	"net.IP":                      true,
	"math/big.Int":                true,
	"github.com/google/uuid.UUID": true,
}

func anyNonEmpty(values []string) bool {
	for _, value := range values {
		if value != "" {
			return true
		}
	}

	return false
}

func (b *schemaBuilder) recordUnresolved(ref typeRef, reason string) {
	b.unresolved = append(b.unresolved, ref.ImportPath+"."+ref.Name+": "+reason)
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
		if t.Len == nil && isByteSlice(t) {
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
		return b.schemaForStruct(t, imports, pkgPath)
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
	} else {
		ref = typeRef{ImportPath: pkgPath, Name: types.ExprString(generic)}
	}

	b.recordUnresolved(ref, "generic instantiation "+types.ExprString(expr)+" is not supported; describe it with Config.TypeSchemas["+strconv.Quote(typeKey(ref))+"]")
	return map[string]any{
		"type": "object",
	}
}

func (b *schemaBuilder) schemaForIdent(name string, pkgPath string) map[string]any {
	schema, ok := builtinSchema(name)
	if ok {
		return schema
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
	case "uint", "uint64":
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
					"hash": map[string]any{
						"type": "string",
					},
				},
				// HasNextPage has no omitempty; NextCursor and Hash do.
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

	return nil, false
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
	if ref.ImportPath == "database/sql" {
		switch ref.Name {
		case "NullString":
			return map[string]any{"type": "string", "nullable": true}, true
		case "NullInt64":
			return map[string]any{"type": "integer", "format": "int64", "nullable": true}, true
		case "NullInt32", "NullInt16", "NullByte":
			return map[string]any{"type": "integer", "format": "int32", "nullable": true}, true
		case "NullBool":
			return map[string]any{"type": "boolean", "nullable": true}, true
		case "NullFloat64":
			return map[string]any{"type": "number", "format": "double", "nullable": true}, true
		case "NullTime":
			return map[string]any{"type": "string", "format": "date-time", "nullable": true}, true
		}
	}

	return nil, false
}

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

func (b *schemaBuilder) schemaForStruct(st *ast.StructType, imports map[string]string, pkgPath string) map[string]any {
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

	return schema
}

// isNilableFieldType reports whether a struct field's type can marshal as JSON
// null: pointers, slices, maps, and interfaces, either directly or through a
// named type (resolved via the package loader, depth-limited). Unresolvable
// types count as nil-able so the field degrades to optional rather than being
// falsely required.
func (b *schemaBuilder) isNilableFieldType(expr ast.Expr, imports map[string]string, pkgPath string, depth int) bool {
	if expr == nil || depth > 4 {
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
	override, ok := b.typeSchemas[typeKey(ref)]
	if ok && !isExtension(override) {
		return override["nullable"] == true
	}

	// Well-known mapped types (time.Time, uuid.UUID, enum overrides) are value
	// types that always serialize.
	schema, ok := b.wellKnownSchema(ref)
	if ok {
		return schema["nullable"] == true
	}

	pkg, err := b.loadPackage(ref.ImportPath)
	if err != nil {
		return true
	}

	info, ok := pkg.Types[ref.Name]
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

// quotableKinds are the predeclared types the `,string` option applies to.
var quotableKinds = map[string]bool{
	"bool": true, "string": true,
	"int": true, "int8": true, "int16": true, "int32": true, "int64": true,
	"uint": true, "uint8": true, "uint16": true, "uint32": true, "uint64": true, "uintptr": true,
	"byte": true, "rune": true, "float32": true, "float64": true,
}

// isQuotableType reports a type whose kind the `,string` option applies to: a boolean, number
// or string, directly or through a named type without a JSON or text marshaler (one with a
// marshaler writes its own value). A type it cannot resolve is not.
func (b *schemaBuilder) isQuotableType(expr ast.Expr, imports map[string]string, pkgPath string, depth int) bool {
	if depth > 4 {
		return false
	}

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

	if ref == (typeRef{ImportPath: "time", Name: "Duration"}) {
		return true
	}
	_, described := b.typeSchemas[typeKey(ref)]
	if described || b.isStandardLibraryImport(ref.ImportPath) {
		return false
	}
	pkg, err := b.loadPackage(ref.ImportPath)
	if err != nil {
		return false
	}
	info, ok := pkg.Types[ref.Name]
	if !ok {
		return false
	}
	methods := pkg.Methods[ref.Name]
	if methods["MarshalJSON"] || methods["MarshalText"] {
		return false
	}

	return b.isQuotableType(info.Spec.Type, info.Imports, pkg.ImportPath, depth+1)
}

// isStructType reports a struct type, directly or through a named type, whose fields encoding/json
// promotes when it is embedded. A type it cannot resolve counts as one, as embedded types always
// did; so does a standard-library type other than the few non-structs wellKnownSchema maps.
func (b *schemaBuilder) isStructType(expr ast.Expr, imports map[string]string, pkgPath string, depth int) bool {
	if depth > 4 {
		return true
	}

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

	override, described := b.typeSchemas[typeKey(ref)]
	if described && !isExtension(override) {
		kind, typed := override["type"]
		return !typed || kind == "object"
	}
	if b.isStandardLibraryImport(ref.ImportPath) {
		return !standardLibraryNonStructs[typeKey(ref)]
	}
	pkg, err := b.loadPackage(ref.ImportPath)
	if err != nil {
		return true
	}
	info, ok := pkg.Types[ref.Name]
	if !ok {
		return true
	}

	return b.isStructType(info.Spec.Type, info.Imports, pkg.ImportPath, depth+1)
}

// standardLibraryNonStructs are the well-known standard-library types (wellKnownSchema) that are
// not structs.
var standardLibraryNonStructs = map[string]bool{
	"time.Duration":            true,
	"net.IP":                   true,
	"encoding/json.RawMessage": true,
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

	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, ez.Wrap(err)
	}
	if len(files) == 0 {
		return nil, ez.New(ez.ENOTFOUND, "no Go files in "+dir, nil)
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
		file, err := parser.ParseFile(fset, filePath, nil, parser.ParseComments)
		if err != nil {
			return nil, ez.Wrap(err)
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

	pkg = &packageInfo{
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
	}

	b.packages[importPath] = pkg

	return pkg, nil
}

// receiverTypeName is the type a method is declared on, T for both `(t T)` and `(t *T)`, ""
// for a function.
func receiverTypeName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return ""
	}
	expr := derefExpr(fn.Recv.List[0].Type)
	index, ok := expr.(*ast.IndexExpr)
	if ok {
		expr = index.X
	}
	ident, ok := expr.(*ast.Ident)
	if !ok {
		return ""
	}

	return ident.Name
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
