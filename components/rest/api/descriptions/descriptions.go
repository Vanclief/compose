// Package descriptions reads the doc comments of Go types and of their fields
// from source, so the route registry, which builds its documents by
// reflection where comments do not exist, can describe what it publishes.
//
// A build step runs Read (through `compose descriptions`) and writes the
// result with Marshal to a file the app embeds, and api.Descriptions loads
// that file. The keys are Key(t) for a type, "github.com/x/app/runs.Run", and
// FieldKey(t, name) for a field, "github.com/x/app/runs.Run#Status". An import
// path can hold a dot but never a #, so no field key is also a type key.
//
// A type declared more than once in its package, such as in a cgo and a !cgo
// file, is not described, nor are its fields: with one declaration there is
// one truth. A defined type
// over a struct of the same package, type A B, gets the field entries of B
// under A, as reflection reports B's fields with A as their struct, also
// through a chain such as type C A, when A, every type along the chain and
// the struct are each declared once in the package. A defined type over a
// struct of another package gets none.
//
// A directory given is keyed by where it is, after symbolic links are
// followed: given as a link to another module's package, it is keyed by that
// module's import path, though a binary built from the linking module reports
// the linking module's. A link to a directory below a directory given is not
// followed. A type declared inside a function shares its key with a
// package-level type of the same name, so it gets that type's description.
// The descriptions are the same for the same files, whatever the order of
// the directories given and however they are reached.
package descriptions

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/vanclief/ez"
	"golang.org/x/mod/modfile"
)

// Key returns the key of a named type: its package path, a dot and its name
// without type arguments, so every instantiation of a generic type shares the
// comment of its declaration. Key returns "" for an unnamed type and for a
// predeclared one. A type of a package main has the package path "main" in a
// binary built with go build or go run, and its import path in a test binary.
// Read writes its entries under its import path, and also under main when
// it is the only main package read that declares types.
func Key(t reflect.Type) string {
	name, _, _ := strings.Cut(t.Name(), "[")
	if name == "" || t.PkgPath() == "" {
		return ""
	}
	return t.PkgPath() + "." + name
}

// fieldSeparator separates a type key from a field name. It cannot appear in
// an import path or an identifier.
const fieldSeparator = "#"

// FieldKey returns the key of the field named field of the struct t: Key(t),
// a # and the Go field name. It returns "" when Key(t) does.
func FieldKey(t reflect.Type, field string) string {
	key := Key(t)
	if key == "" {
		return ""
	}
	return key + fieldSeparator + field
}

// CommentText is a doc comment as a description: its text without the
// "Deprecated:" paragraph (the schema says deprecated) and without TODO,
// FIXME and nolint lines, which are notes for maintainers.
func CommentText(group *ast.CommentGroup) string {
	if group == nil {
		return ""
	}

	var kept []string
	deprecated := false
	for _, line := range strings.Split(group.Text(), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			deprecated = false
		}
		if strings.HasPrefix(trimmed, "Deprecated: ") {
			deprecated = true
		}
		if deprecated || isMaintainerNote(trimmed) {
			continue
		}
		kept = append(kept, line)
	}

	return strings.TrimSpace(strings.Join(kept, "\n"))
}

func isMaintainerNote(line string) bool {
	for _, prefix := range []string{"TODO", "FIXME", "nolint"} {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}

	return false
}

// TypeComment returns the doc comment of the type spec declares: the one on
// spec, or the one on gen when gen declares that type alone.
func TypeComment(gen *ast.GenDecl, spec *ast.TypeSpec) *ast.CommentGroup {
	if spec.Doc == nil && len(gen.Specs) == 1 {
		return gen.Doc
	}
	return spec.Doc
}

// FieldText describes a struct field: its doc comment, then its line comment.
func FieldText(field *ast.Field) string {
	var parts []string
	for _, group := range []*ast.CommentGroup{field.Doc, field.Comment} {
		text := CommentText(group)
		if text != "" {
			parts = append(parts, text)
		}
	}

	return strings.Join(parts, "\n\n")
}

// Duplicate is a type declared more than once in its package. Read describes
// neither it nor its fields.
type Duplicate struct {
	Key    string // the key of the type
	First  string // the first file that declares it, in walk order
	Second string // the second file that declares it
}

// Result is what Read found.
type Result struct {
	// Descriptions maps each key to its text. It is what Marshal writes.
	Descriptions map[string]string
	// Duplicates are the types declared more than once in their package,
	// left out of Descriptions, sorted by key.
	Duplicates []Duplicate
	// MainPackages are the import paths of the main packages that declare
	// types, sorted, when there are two or more of them and one has an
	// entry. No key is then written under main, so those entries are not
	// found in a binary built with go build or go run. It is empty otherwise.
	MainPackages []string
}

// Read walks each directory and returns the descriptions of the types
// declared under it, keyed as Key and FieldKey do. A type of a main package
// written under main has the keys main.Run and main.Run#Status.
//
// Each directory given is made absolute and its symbolic links are resolved,
// so a file is read once however the arguments reach it. Below it, Read skips
// the directories named vendor or testdata, those whose name starts with "."
// or "_", and symbolic links to directories, as ./... does. It skips the files
// whose name starts with "." or "_", the _test.go files and the files with a
// //go:build ignore line. It reads every other .go file, a symbolic link to
// a file too, whatever its build constraints, so the result is the same on
// every platform. A file that does not parse fails Read, a skipped name
// aside. Every package-level named type with a doc comment has an entry,
// unexported ones too, and so does every exported field of a package-level
// struct type with a comment, an embedded one under the name of its type.
// Aliases have none.
//
// The import path of a package is the module path of the nearest go.mod above
// it joined with its directory below that go.mod, or what follows the last
// vendor element of that directory. A main package is an import path whose
// files read all say package main. When exactly one main package read
// declares a type, an alias counting, its entries are also written under
// "main". When two or more do, none is, and Result.MainPackages names them.
// A package whose import path is exactly main, in a module named main, has
// entries only when it is the one main package, as its keys are then its
// main keys. Otherwise its keys would be those of another command's types,
// and a library there is in no binary, as main cannot be imported. A package
// below it, such as main/sub, is keyed as usual.
//
// A type declared more than once in its package, of any kind and whatever its
// comments, has no entry and its fields have none, as which declaration a
// binary holds depends on build constraints Read does not evaluate. It is
// reported as a Duplicate. No description is better than a wrong one.
func Read(dirs ...string) (Result, error) {
	rd := &reader{
		read:    map[string]bool{},
		out:     map[entry]string{},
		files:   map[entry][]string{},
		structs: map[entry]bool{},
		over:    map[entry]string{},
		pkgs:    map[string]*pkgInfo{},
		modules: map[string]string{},
	}
	for _, dir := range dirs {
		err := rd.walk(dir)
		if err != nil {
			return Result{}, ez.Wrap(err)
		}
	}
	duplicates := rd.dropDuplicates()
	rd.copyDefinedFields()

	var mains []string
	for pkgPath, info := range rd.pkgs {
		if info.allMain && info.types {
			mains = append(mains, pkgPath)
		}
	}
	slices.Sort(mains)

	result := Result{Descriptions: map[string]string{}, Duplicates: duplicates}
	mainEntries := false
	for e, text := range rd.out {
		isMain := slices.Contains(mains, e.pkg)
		mainEntries = mainEntries || isMain
		onlyMain := isMain && len(mains) == 1
		if e.pkg == "main" && !onlyMain {
			continue
		}
		result.Descriptions[e.pkg+"."+e.name] = text
		if onlyMain {
			result.Descriptions["main."+e.name] = text
		}
	}
	if len(mains) > 1 && mainEntries {
		result.MainPackages = mains
	}
	return result, nil
}

// Marshal returns descriptions as the file api.Descriptions loads: one JSON
// object with sorted keys, indented, ending in a newline, the same bytes for
// the same map.
func Marshal(descriptions map[string]string) ([]byte, error) {
	if descriptions == nil {
		descriptions = map[string]string{}
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	err := enc.Encode(descriptions)
	if err != nil {
		return nil, ez.New(ez.EINTERNAL, "cannot encode the descriptions", err)
	}
	return buf.Bytes(), nil
}

type reader struct {
	read    map[string]bool     // resolved absolute paths of the files read
	out     map[entry]string    // the texts
	files   map[entry][]string  // the files that declare each type, in walk order
	structs map[entry]bool      // the struct types
	over    map[entry]string    // B for each type A B, B a type of A's package
	pkgs    map[string]*pkgInfo // import path -> what its files declare
	modules map[string]string   // directory holding a go.mod -> its module path
}

// pkgInfo is what the files read of one import path declare.
type pkgInfo struct {
	allMain bool // every file says package main
	types   bool // a file declares a package-level type, an alias too
}

// entry is a key split into the import path of its package and the rest,
// Name or Name#Field.
type entry struct {
	pkg  string
	name string
}

// walk reads the directory dir. It walks dir made absolute with its links
// resolved, so a file has one path however the argument reached it, and names
// a file in errors and duplicates by dir joined with its path below it.
func (rd *reader) walk(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return ez.New(ez.EINVALID, "cannot resolve "+dir+": "+err.Error(), err)
	}
	root, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return ez.New(ez.EINVALID, "cannot read "+dir+": "+err.Error(), err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return ez.New(ez.EINVALID, "cannot read "+dir+": "+err.Error(), err)
	}
	if !info.IsDir() {
		return ez.New(ez.EINVALID, dir+" is not a directory", nil)
	}

	err = filepath.WalkDir(root, func(path string, dirEntry fs.DirEntry, err error) error {
		shown := shownPath(dir, root, path)
		if err != nil {
			return ez.New(ez.EINVALID, "cannot read "+shown+": "+err.Error(), err)
		}
		name := dirEntry.Name()
		if dirEntry.IsDir() {
			if path != root && skipDir(name) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || ignored(name) || !regularFile(path, dirEntry) {
			return nil
		}
		err = rd.readFile(path, shown)
		if err != nil {
			return ez.Wrap(err)
		}
		return nil
	})
	if err != nil {
		return ez.Wrap(err)
	}
	return nil
}

// shownPath returns path, a path below root, as below dir, the argument root
// was resolved from.
func shownPath(dir, root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.Join(dir, rel)
}

// regularFile reports a regular file, or a symbolic link to one, which Go
// compiles too. A broken link is none.
func regularFile(path string, entry fs.DirEntry) bool {
	if entry.Type()&fs.ModeSymlink == 0 {
		return entry.Type().IsRegular()
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// ignored reports a name the go command ignores, a file or a directory.
func ignored(name string) bool {
	return strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}

// skipDir reports a directory the go command leaves out of ./...
func skipDir(name string) bool {
	return name == "vendor" || name == "testdata" || ignored(name)
}

// readFile reads the file at path, once, and names it shown. path is
// absolute with the links of its directories resolved, so a file reached
// twice has the same path.
func (rd *reader) readFile(path, shown string) error {
	if rd.read[path] {
		return nil
	}
	rd.read[path] = true

	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return ez.New(ez.EINVALID, "cannot parse "+shown+": "+err.Error(), err)
	}
	if buildIgnored(file) {
		return nil
	}

	pkgPath, err := rd.importPath(filepath.Dir(path))
	if err != nil {
		return ez.Wrap(err)
	}
	info := rd.pkgs[pkgPath]
	if info == nil {
		info = &pkgInfo{allMain: true}
		rd.pkgs[pkgPath] = info
	}
	info.allMain = info.allMain && file.Name.Name == "main"

	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			typeSpec := spec.(*ast.TypeSpec)
			typ := entry{pkg: pkgPath, name: typeSpec.Name.Name}
			rd.files[typ] = append(rd.files[typ], shown)
			info.types = true
			if !typeSpec.Assign.IsValid() {
				rd.readType(pkgPath, gen, typeSpec)
			}
		}
	}
	return nil
}

// buildIgnored reports a file with a //go:build ignore line, a
// script that is never part of its package.
func buildIgnored(file *ast.File) bool {
	for _, group := range file.Comments {
		if group.Pos() > file.Package {
			return false
		}
		for _, comment := range group.List {
			if !constraint.IsGoBuild(comment.Text) {
				continue
			}
			expr, err := constraint.Parse(comment.Text)
			if err != nil {
				return false
			}
			tag, ok := expr.(*constraint.TagExpr)
			return ok && tag.Tag == "ignore"
		}
	}
	return false
}

// readType records the comment of a type and of its fields.
func (rd *reader) readType(pkgPath string, gen *ast.GenDecl, spec *ast.TypeSpec) {
	typ := entry{pkg: pkgPath, name: spec.Name.Name}
	rd.add(typ, CommentText(TypeComment(gen, spec)))

	over := localTypeName(spec.Type)
	if over != "" {
		rd.over[typ] = over
	}

	structType, ok := spec.Type.(*ast.StructType)
	if !ok {
		return
	}
	rd.structs[typ] = true
	for _, field := range structType.Fields.List {
		text := FieldText(field)
		names := field.Names
		if len(names) == 0 {
			embedded := embeddedName(field.Type)
			if embedded == nil {
				continue
			}
			names = []*ast.Ident{embedded}
		}
		for _, fieldName := range names {
			if fieldName.IsExported() {
				rd.add(entry{pkg: pkgPath, name: typ.name + fieldSeparator + fieldName.Name}, text)
			}
		}
	}
}

// add records text under e, unless text is empty.
func (rd *reader) add(e entry, text string) {
	if text != "" {
		rd.out[e] = text
	}
}

// localTypeName returns the name of the type of its own package expr names,
// B in type A B or type A B[int], or "".
func localTypeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr:
		return localTypeName(t.X)
	case *ast.IndexListExpr:
		return localTypeName(t.X)
	case *ast.ParenExpr:
		return localTypeName(t.X)
	}
	return ""
}

// embeddedName returns the name reflection gives an embedded field: the name
// of its type, without package, pointer or type arguments.
func embeddedName(expr ast.Expr) *ast.Ident {
	switch t := expr.(type) {
	case *ast.Ident:
		return t
	case *ast.SelectorExpr:
		return t.Sel
	case *ast.StarExpr:
		return embeddedName(t.X)
	case *ast.IndexExpr:
		return embeddedName(t.X)
	case *ast.IndexListExpr:
		return embeddedName(t.X)
	case *ast.ParenExpr:
		return embeddedName(t.X)
	}
	return nil
}

// copyDefinedFields gives each type A defined over a struct of its own
// package, directly or through a chain (type C A, type A B), the field
// entries of that struct, when A, every type along the chain and the struct
// are each declared once in the package.
func (rd *reader) copyDefinedFields() {
	copies := map[entry]string{}
	for typ, over := range rd.over {
		target, ok := rd.structOf(entry{pkg: typ.pkg, name: over})
		if len(rd.files[typ]) != 1 || !ok {
			continue
		}
		for e, text := range rd.out {
			field, ok := strings.CutPrefix(e.name, target.name+fieldSeparator)
			if ok && e.pkg == typ.pkg {
				copies[entry{pkg: typ.pkg, name: typ.name + fieldSeparator + field}] = text
			}
		}
	}
	maps.Copy(rd.out, copies)
}

// structOf returns the struct the chain from typ ends at, when every type
// along it is declared once in its package. It stops on a cycle.
func (rd *reader) structOf(typ entry) (entry, bool) {
	for range len(rd.over) + 1 {
		if len(rd.files[typ]) != 1 {
			return entry{}, false
		}
		if rd.structs[typ] {
			return typ, true
		}
		over, ok := rd.over[typ]
		if !ok {
			return entry{}, false
		}
		typ.name = over
	}
	return entry{}, false
}

// dropDuplicates removes the entries of every type declared more than once in
// its package, and of its fields, and returns those types.
func (rd *reader) dropDuplicates() []Duplicate {
	for e := range rd.out {
		typeName, _, _ := strings.Cut(e.name, fieldSeparator)
		if len(rd.files[entry{pkg: e.pkg, name: typeName}]) > 1 {
			delete(rd.out, e)
		}
	}

	var duplicates []Duplicate
	for typ, files := range rd.files {
		if len(files) > 1 {
			duplicates = append(duplicates, Duplicate{Key: typ.pkg + "." + typ.name, First: files[0], Second: files[1]})
		}
	}
	slices.SortFunc(duplicates, func(a, b Duplicate) int {
		return strings.Compare(a.Key, b.Key)
	})
	return duplicates
}

// importPath returns the import path of the package in dir: the module path
// of the nearest go.mod at or above dir, joined with dir's path below it, or
// what follows the last vendor element of that path.
func (rd *reader) importPath(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", ez.New(ez.EINVALID, "cannot resolve "+dir+": "+err.Error(), err)
	}

	moduleDir := abs
	for {
		modulePath, err := rd.modulePath(moduleDir)
		if err != nil {
			return "", ez.Wrap(err)
		}
		if modulePath != "" {
			rel, err := filepath.Rel(moduleDir, abs)
			if err != nil {
				return "", ez.New(ez.EINTERNAL, "cannot place "+abs+" below "+moduleDir+": "+err.Error(), err)
			}
			if rel == "." {
				return modulePath, nil
			}
			parts := strings.Split(filepath.ToSlash(rel), "/")
			for i := len(parts) - 2; i >= 0; i-- {
				if parts[i] == "vendor" {
					return strings.Join(parts[i+1:], "/"), nil
				}
			}
			return modulePath + "/" + strings.Join(parts, "/"), nil
		}

		parent := filepath.Dir(moduleDir)
		if parent == moduleDir {
			return "", ez.New(ez.EINVALID, dir+" is in no module: no go.mod at or above it", nil)
		}
		moduleDir = parent
	}
}

// modulePath returns the module path the go.mod in dir declares, or "" when
// dir holds no go.mod.
func (rd *reader) modulePath(dir string) (string, error) {
	modulePath, ok := rd.modules[dir]
	if ok {
		return modulePath, nil
	}

	goMod := filepath.Join(dir, "go.mod")
	data, err := os.ReadFile(goMod)
	if os.IsNotExist(err) {
		rd.modules[dir] = ""
		return "", nil
	}
	if err != nil {
		return "", ez.New(ez.EINVALID, "cannot read "+goMod+": "+err.Error(), err)
	}

	modulePath = modfile.ModulePath(data)
	if modulePath == "" {
		return "", ez.New(ez.EINVALID, goMod+" declares no module path", nil)
	}
	rd.modules[dir] = modulePath
	return modulePath, nil
}
