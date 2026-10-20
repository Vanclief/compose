package generator

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"

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
	schemas        map[string]map[string]any
	building       map[typeRef]bool
	// Types emitted as opaque objects because their source could not be read. Silently
	// dropping their fields would publish a broken contract, so Generate refuses to return a
	// document while this is non-empty.
	unresolved []string

	requiredPackagePrefix string
	prefixCollapse        map[string]string
	nameAliases           map[string]string
	docComments           bool
	// origins maps each component schema to the import path of the Go type it was built from.
	origins map[string]string
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
		schemas:               map[string]map[string]any{},
		building:              map[typeRef]bool{},
		requiredPackagePrefix: cfg.RequiredPackagePrefix,
		prefixCollapse:        cfg.SchemaPrefixCollapse,
		nameAliases:           cfg.SchemaNameAliases,
		docComments:           cfg.DocComments,
		origins:               map[string]string{},
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

func importMap(file *ast.File) map[string]string {
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
			name = path.Base(pathValue)
		}
		if name == "_" || name == "." {
			continue
		}

		imports[name] = pathValue
	}

	return imports
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

		respSchema, err := responseSchemaForRequest(builder, reqType)
		if err != nil {
			return nil, ez.Wrap(err)
		}

		out[name] = handlerSchema{
			Request:    reqSchema,
			Response:   respSchema,
			PathFields: builder.jsonFieldNames(reqType, facts.pathFields[name]),
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
		if len(field.Names) == 0 || !wanted[field.Names[0].Name] {
			continue
		}
		name := jsonFieldName(field)
		if name != "" {
			names = append(names, name)
		}
	}

	return names
}

func responseSchemaForRequest(builder *schemaBuilder, reqType typeRef) (map[string]any, error) {

	pkg, err := builder.loadPackage(reqType.ImportPath)
	if err != nil {
		return nil, ez.Wrap(err)
	}

	for _, fn := range pkg.Funcs {
		if fn.Decl == nil {
			continue
		}
		if !funcHasRequestParam(fn.Decl, reqType) {
			continue
		}

		resultType := firstResultType(fn.Decl)
		if resultType == nil {
			return nil, nil
		}

		schema := builder.schemaForExpr(resultType, fn.Imports, pkg.ImportPath)
		return schema, nil
	}

	// A handler that decodes the raw body documents the wire payload, but the module function
	// is keyed on the request type the decoder normalizes it into, so follow the decoder.
	moduleType, ok := decodedRequestType(pkg, reqType)
	if ok {
		return responseSchemaForRequest(builder, moduleType)
	}

	return nil, nil
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
	componentSchema, ok := b.wellKnownComponentSchema(ref)
	if ok {
		name := b.schemaName(ref)
		_, exists := b.schemas[name]
		if !exists {
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
		return map[string]any{
			"type": "object",
		}
	}

	b.ensureSchema(ref)

	return map[string]any{
		"$ref": schemaRefPrefix + b.schemaName(ref),
	}
}

// schemaName is the component name of a type: its import path relative to the module, with
// SchemaNameAliases and SchemaPrefixCollapse applied, `/ . -` turned into `_`, then the name.
func (b *schemaBuilder) schemaName(ref typeRef) string {
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
		b.origins = map[string]string{}
	}
	b.origins[name] = ref.ImportPath
}

func (b *schemaBuilder) buildSchema(ref typeRef) map[string]any {
	// Standard-library types are not modeled; they stay opaque objects
	// (mime/multipart.FileHeader is the only one in the spec today).
	if isStandardLibraryImport(ref.ImportPath) {
		return map[string]any{
			"type": "object",
		}
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

	schema := b.schemaForExpr(info.Spec.Type, info.Imports, pkg.ImportPath)

	values, isEnum := pkg.Enums[ref.Name]
	if isEnum && schema["type"] == "string" {
		enum := make([]any, 0, len(values))
		for _, value := range values {
			enum = append(enum, value)
		}
		schema["enum"] = enum

		docs := pkg.EnumDocs[ref.Name]
		if b.docComments && anyNonEmpty(docs) {
			descriptions := make([]any, 0, len(docs))
			for _, doc := range docs {
				descriptions = append(descriptions, doc)
			}
			schema["x-enum-descriptions"] = descriptions
		}
	}

	if b.docComments {
		doc := commentText(info.Doc)
		_, isRef := schema["$ref"]
		if doc != "" && !isRef {
			schema["description"] = doc
		}
	}

	return schema
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
// carries a dot, the standard library's never does.
func isStandardLibraryImport(importPath string) bool {
	first, _, _ := strings.Cut(importPath, "/")
	return importPath != "" && !strings.Contains(first, ".")
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
		schema := b.schemaForExpr(t.X, imports, pkgPath)
		schema["nullable"] = true
		return schema
	case *ast.SelectorExpr:
		pkgIdent, ok := t.X.(*ast.Ident)
		if !ok {
			return map[string]any{"type": "object"}
		}
		importPath := imports[pkgIdent.Name]
		ref := typeRef{ImportPath: importPath, Name: t.Sel.Name}
		return b.schemaRef(ref)
	case *ast.ArrayType:
		if isByteSlice(t) {
			return map[string]any{
				"type":   "string",
				"format": "byte",
			}
		}
		return map[string]any{
			"type":  "array",
			"items": b.schemaForExpr(t.Elt, imports, pkgPath),
		}
	case *ast.MapType:
		return map[string]any{
			"type":                 "object",
			"additionalProperties": b.schemaForExpr(t.Value, imports, pkgPath),
		}
	case *ast.StructType:
		return b.schemaForStruct(t, imports, pkgPath)
	case *ast.InterfaceType:
		return map[string]any{
			"type": "object",
		}
	default:
		return map[string]any{
			"type": "object",
		}
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
	case "int", "int8", "int16", "int32":
		return map[string]any{"type": "integer", "format": "int32"}, true
	case "uint", "uint8", "uint16", "uint32":
		return map[string]any{"type": "integer", "format": "int32"}, true
	case "int64":
		return map[string]any{"type": "integer", "format": "int64"}, true
	case "uint64":
		return map[string]any{"type": "integer", "format": "int64"}, true
	case "float32":
		return map[string]any{"type": "number", "format": "float"}, true
	case "float64":
		return map[string]any{"type": "number", "format": "double"}, true
	case "byte":
		return map[string]any{"type": "string", "format": "byte"}, true
	case "rune":
		return map[string]any{"type": "integer", "format": "int32"}, true
	case "any":
		// `any` carries arbitrary JSON, so it must not be constrained.
		return map[string]any{}, true
	}
	return nil, false
}

func (b *schemaBuilder) wellKnownComponentSchema(ref typeRef) (map[string]any, bool) {
	if ref.ImportPath == "github.com/vanclief/compose/drivers/databases/relational/postgres/pagination" {
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

func (b *schemaBuilder) wellKnownSchema(ref typeRef) (map[string]any, bool) {
	if ref.ImportPath == "time" && ref.Name == "Time" {
		return map[string]any{"type": "string", "format": "date-time"}, true
	}
	if ref.ImportPath == "github.com/google/uuid" && ref.Name == "UUID" {
		return map[string]any{"type": "string", "format": "uuid"}, true
	}
	if ref.ImportPath == "encoding/json" && ref.Name == "RawMessage" {
		return map[string]any{"type": "object"}, true
	}
	if ref.ImportPath == "database/sql" {
		switch ref.Name {
		case "NullString":
			return map[string]any{"type": "string", "nullable": true}, true
		case "NullInt64":
			return map[string]any{"type": "integer", "format": "int64", "nullable": true}, true
		case "NullBool":
			return map[string]any{"type": "boolean", "nullable": true}, true
		case "NullFloat64":
			return map[string]any{"type": "number", "format": "double", "nullable": true}, true
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

// stripRequestSideRequired clears the required list of every models-package
// component a request body can reach. schemaForStruct marks models required
// because the server always serializes their value-typed fields, but a type
// that also rides on a request (junction rows, invoice adjustments, expense
// items, settings updates) cannot require anything on the way in — Go reads a
// missing key as the zero value. Only server-stamped fields marked readOnly
// stay required, which OpenAPI 3.0 scopes to responses. Hand-curated request
// schemas (modules packages) are left alone. modelsPrefix is the component name
// prefix of the models packages; empty strips nothing.
func stripRequestSideRequired(handlerSchemas map[string]handlerSchema, schemas map[string]map[string]any, modelsPrefix string) {
	if modelsPrefix == "" {
		return
	}

	visited := map[string]bool{}

	var visit func(node any)
	visit = func(node any) {
		switch typed := node.(type) {
		case map[string]any:
			ref, isRef := typed["$ref"].(string)
			if isRef {
				name := strings.TrimPrefix(ref, schemaRefPrefix)
				if !visited[name] {
					visited[name] = true
					component, found := schemas[name]
					if found {
						if strings.HasPrefix(name, modelsPrefix) {
							keepReadOnlyRequired(component)
						}
						visit(component)
					}
				}
			}
			for _, value := range typed {
				visit(value)
			}
		case []any:
			for _, value := range typed {
				visit(value)
			}
		}
	}

	for _, handler := range handlerSchemas {
		visit(handler.Request)
	}
}

// keepReadOnlyRequired drops every required name except those whose property
// is readOnly. The builder emits required as []string, tweaks as []any.
func keepReadOnlyRequired(schema map[string]any) {
	for _, member := range schemaMembers(schema) {
		properties, _ := member["properties"].(map[string]any)

		var kept []string
		for _, name := range requiredNames(member["required"]) {
			property, ok := properties[name].(map[string]any)
			if ok && property["readOnly"] == true {
				kept = append(kept, name)
			}
		}

		if len(kept) == 0 {
			delete(member, "required")
			continue
		}
		member["required"] = kept
	}
}

func requiredNames(required any) []string {
	switch typed := required.(type) {
	case []string:
		return typed
	case []any:
		var names []string
		for _, name := range typed {
			text, ok := name.(string)
			if ok {
				names = append(names, text)
			}
		}
		return names
	default:
		return nil
	}
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

	// Models are server-serialized: a value-typed field without omitempty always
	// has its key in the payload, so it is required. Request and response wrapper
	// structs (modules, interfaces) keep every field optional — Go unmarshals a
	// missing request key as the zero value, so requiring them would be wrong.
	// Models that also ride on a request body get their list cleared again by
	// stripRequestSideRequired once the request graph is known.
	markRequired := b.requiredPackagePrefix != "" && strings.Contains(pkgPath, b.requiredPackagePrefix)
	table := ""

	for _, field := range st.Fields.List {
		if field == nil {
			continue
		}
		flaggedDeprecated := hasDeprecatedParagraph(field)
		if !flaggedDeprecated && isDeprecatedField(field) {
			continue
		}

		if len(field.Names) == 0 {
			// bun.BaseModel marshals nothing; its tag names the model's table.
			if isBunBaseModel(field.Type, imports) {
				table = bunTable(field)
				continue
			}
			embedded := b.schemaForExpr(field.Type, imports, pkgPath)
			allOf = append(allOf, embedded)
			continue
		}

		name := jsonFieldName(field)
		if name == "" {
			continue
		}

		schema := b.schemaForExpr(field.Type, imports, pkgPath)
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
		if markRequired && !jsonFieldOmitsEmpty(field) && !b.isNilableFieldType(field.Type, imports, pkgPath, 0) {
			required = append(required, name)
		}
	}

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
		return true
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
	// Well-known mapped types (time.Time, uuid.UUID, enum overrides) are value
	// types that always serialize.
	_, ok := b.wellKnownSchema(ref)
	if ok {
		return false
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

func jsonFieldOmitsEmpty(field *ast.Field) bool {
	if field == nil || field.Tag == nil {
		return false
	}

	tag := strings.Trim(field.Tag.Value, "`")
	if tag == "" {
		return false
	}

	jsonTag := reflect.StructTag(tag).Get("json")
	parts := strings.Split(jsonTag, ",")
	for _, option := range parts[1:] {
		if option == "omitempty" || option == "omitzero" {
			return true
		}
	}

	return false
}

func jsonFieldName(field *ast.Field) string {
	if len(field.Names) == 0 {
		return ""
	}

	name := field.Names[0].Name
	if !ast.IsExported(name) {
		return ""
	}

	if field.Tag == nil {
		return name
	}

	tag := strings.Trim(field.Tag.Value, "`")
	if tag == "" {
		return name
	}

	jsonTag := reflect.StructTag(tag).Get("json")
	if jsonTag == "-" {
		return ""
	}
	if jsonTag == "" {
		return name
	}

	parts := strings.Split(jsonTag, ",")
	if len(parts) == 0 {
		return name
	}
	if parts[0] == "" {
		return name
	}
	return parts[0]
}

// hasDeprecatedParagraph reports a field documented the Go way, with a paragraph that starts
// "Deprecated:". Such a field is still served and stays in the contract, flagged deprecated, so
// clients keep compiling while they move off it. Any other mention of deprecation drops the field
// from the contract (isDeprecatedField).
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

// deprecatedSchema flags a property schema deprecated. OpenAPI 3.0 ignores keywords beside a
// $ref, so a reference is wrapped in allOf first.
func deprecatedSchema(schema map[string]any) map[string]any {
	_, isRef := schema["$ref"]
	if isRef {
		return map[string]any{
			"allOf":      []any{schema},
			"deprecated": true,
		}
	}

	flagged := make(map[string]any, len(schema)+1)
	for key, value := range schema {
		flagged[key] = value
	}
	flagged["deprecated"] = true

	return flagged
}

func isDeprecatedField(field *ast.Field) bool {
	if field == nil {
		return false
	}

	comments := ""
	if field.Doc != nil {
		comments = comments + field.Doc.Text()
	}
	if field.Comment != nil {
		comments = comments + field.Comment.Text()
	}

	comments = strings.ToLower(comments)
	if strings.Contains(comments, "deprecat") {
		return true
	}

	return false
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
	var funcs []funcInfo

	for _, filePath := range files {
		if strings.HasSuffix(filePath, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filePath, nil, parser.ParseComments)
		if err != nil {
			return nil, ez.Wrap(err)
		}

		imports := importMap(file)

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if ok {
				funcs = append(funcs, funcInfo{
					Decl:    fn,
					Imports: imports,
				})
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
			case token.VAR:
				collectClosedSetTypes(gen, closedSets, closedSetMembers)
			}
		}
	}

	enums := map[string][]string{}
	enumDocs := map[string][]string{}
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
		}
	}

	pkg = &packageInfo{
		ImportPath: importPath,
		Dir:        dir,
		Types:      types,
		Funcs:      funcs,
		Enums:      enums,
		EnumDocs:   enumDocs,
	}

	b.packages[importPath] = pkg

	return pkg, nil
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
