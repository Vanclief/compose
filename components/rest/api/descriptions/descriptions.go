// Package descriptions reads the doc comments of Go types and of their fields
// from source, so the route registry, which builds its documents by
// reflection where comments do not exist, can describe what it publishes.
//
// A build step runs Read (through `compose descriptions`) and writes the
// result with Marshal to a file the app embeds, and api.Descriptions loads
// that file. The keys are Key(t) for a type and Key(t) + "." + the Go field
// name for a field: "github.com/x/app/runs.Run" and
// "github.com/x/app/runs.Run.Status".
//
// A defined type over a struct of the same package, type A B, gets the field
// entries of B under A, as reflection reports B's fields with A as their
// struct. It gets them only when A, every type along the chain and the
// struct at its end are each declared once in the package. A defined type
// over a struct of another package gets none.
//
// A directory is keyed by where it is, after symbolic links are followed: a
// package directory that is a link to another module's package is keyed by
// that module's import path, though a binary built from the linking module
// reports the linking module's. A type declared inside a function shares its
// key with a package-level type of the same name, so it gets that type's
// description.
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

// Conflict is a key two files describe differently. Read leaves it out.
type Conflict struct {
	Key    string
	First  string // the first file that described it, in walk order
	Second string // the first file that described it otherwise
}

// Result is what Read found.
type Result struct {
	// Descriptions maps each key to its text. It is what Marshal writes.
	Descriptions map[string]string
	// Conflicts are the keys two files describe differently, left out of
	// Descriptions, sorted by key.
	Conflicts []Conflict
	// MainPackages are the import paths of the main packages that declare
	// types, sorted, when there are two or more of them and one has an
	// entry. No key is then written under main, so those entries are not
	// found in a binary built with go build or go run. It is empty otherwise.
	MainPackages []string
}

// Read walks each directory and returns the descriptions of the types
// declared under it, keyed as Key does.
//
// Below each directory given, it skips the directories named vendor or
// testdata and those whose name starts with "." or "_". It skips the files
// whose name starts with "." or "_", the _test.go files and the files whose
// build constraint is exactly ignore. Every other file is read once, whatever
// its build constraints, so the result is the same on every platform, and so
// is a symbolic link to a file. Every package-level named type with a doc
// comment has an entry, unexported ones too, and so does every exported field
// of a package-level struct type with a comment, an embedded one under the
// name of its type. Aliases have none.
//
// The import path of a package is the module path of the nearest go.mod above
// it joined with its directory below that go.mod, or what follows the last
// vendor element of that directory. When exactly one main package read
// declares types, its entries are also written under "main". When two or more
// do, none is, and Result.MainPackages names them. A key that two files
// describe differently is left out, even when a third file repeats one of the
// texts, and reported as a Conflict: no description is better than a wrong
// one.
func Read(dirs ...string) (Result, error) {
	rd := &reader{
		read:      map[string]bool{},
		out:       map[entry]string{},
		from:      map[entry]string{},
		conflicts: map[entry]Conflict{},
		decls:     map[entry]int{},
		structs:   map[entry]bool{},
		over:      map[entry]string{},
		mains:     map[string]bool{},
		modules:   map[string]string{},
	}
	for _, dir := range dirs {
		err := rd.walk(dir)
		if err != nil {
			return Result{}, ez.Wrap(err)
		}
	}
	rd.copyDefinedFields()

	mains := slices.Sorted(maps.Keys(rd.mains))
	result := Result{Descriptions: map[string]string{}}
	for e, text := range rd.out {
		result.Descriptions[e.pkg+"."+e.name] = text
		if rd.mains[e.pkg] && len(mains) == 1 {
			result.Descriptions["main."+e.name] = text
		}
		if rd.mains[e.pkg] && len(mains) > 1 {
			result.MainPackages = mains
		}
	}
	result.Conflicts = slices.SortedFunc(maps.Values(rd.conflicts), func(a, b Conflict) int {
		return strings.Compare(a.Key, b.Key)
	})
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
	read      map[string]bool    // absolute paths of the files read
	out       map[entry]string   // the texts
	from      map[entry]string   // the first file that described each entry
	conflicts map[entry]Conflict // the entries left out
	decls     map[entry]int      // how many times its package declares each type
	structs   map[entry]bool     // the struct types
	over      map[entry]string   // B for each type A B, B a type of A's package
	mains     map[string]bool    // import paths of the main packages that declare types
	modules   map[string]string  // directory holding a go.mod -> its module path
}

// entry is a key split into the import path of its package and the rest,
// Name or Name.Field.
type entry struct {
	pkg  string
	name string
}

func (rd *reader) walk(dir string) error {
	root, err := filepath.EvalSymlinks(dir)
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

	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return ez.New(ez.EINVALID, "cannot read "+path+": "+err.Error(), err)
		}
		name := entry.Name()
		if entry.IsDir() {
			if path != root && skipDir(name) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || ignored(name) || !regularFile(path, entry) {
			return nil
		}
		err = rd.readFile(path)
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

// readFile reads the file at path, once. The directories walked have their
// links resolved, so a file reached twice has the same absolute path.
func (rd *reader) readFile(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return ez.New(ez.EINVALID, "cannot resolve "+path+": "+err.Error(), err)
	}
	if rd.read[abs] {
		return nil
	}
	rd.read[abs] = true

	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return ez.New(ez.EINVALID, "cannot parse "+path+": "+err.Error(), err)
	}
	if buildIgnored(file) {
		return nil
	}

	pkgPath, err := rd.importPath(filepath.Dir(path))
	if err != nil {
		return ez.Wrap(err)
	}

	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			typeSpec := spec.(*ast.TypeSpec)
			if typeSpec.Assign.IsValid() {
				continue
			}
			if file.Name.Name == "main" {
				rd.mains[pkgPath] = true
			}
			rd.readType(path, pkgPath, gen, typeSpec)
		}
	}
	return nil
}

// buildIgnored reports a file whose //go:build line is exactly ignore, a
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
func (rd *reader) readType(path, pkgPath string, gen *ast.GenDecl, spec *ast.TypeSpec) {
	typ := entry{pkg: pkgPath, name: spec.Name.Name}
	rd.decls[typ]++
	rd.add(path, typ, CommentText(TypeComment(gen, spec)))

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
				rd.add(path, entry{pkg: pkgPath, name: typ.name + "." + fieldName.Name}, text)
			}
		}
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
// are each declared once in the package. A copy cannot conflict: the
// struct's fields come from its one declaration, and A has no fields of its
// own.
func (rd *reader) copyDefinedFields() {
	copies := map[entry]string{}
	for typ, over := range rd.over {
		target, ok := rd.structOf(entry{pkg: typ.pkg, name: over})
		if rd.decls[typ] != 1 || !ok {
			continue
		}
		for e, text := range rd.out {
			field, ok := strings.CutPrefix(e.name, target.name+".")
			if ok && e.pkg == typ.pkg {
				copies[entry{pkg: typ.pkg, name: typ.name + "." + field}] = text
			}
		}
	}
	maps.Copy(rd.out, copies)
}

// structOf returns the struct the chain from typ ends at, when every type
// along it is declared once in its package. It stops on a cycle.
func (rd *reader) structOf(typ entry) (entry, bool) {
	for range len(rd.over) + 1 {
		if rd.decls[typ] != 1 {
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

// add records text under e, unless text is empty. An entry described
// differently twice is left out for good.
func (rd *reader) add(path string, e entry, text string) {
	_, conflicted := rd.conflicts[e]
	if text == "" || conflicted {
		return
	}
	previous, ok := rd.out[e]
	if !ok {
		rd.out[e] = text
		rd.from[e] = path
		return
	}
	if previous != text {
		rd.conflicts[e] = Conflict{Key: e.pkg + "." + e.name, First: rd.from[e], Second: path}
		delete(rd.out, e)
	}
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
