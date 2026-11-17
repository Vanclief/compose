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
// struct. A defined type over a struct of another package gets none.
package descriptions

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/vanclief/ez"
	"golang.org/x/mod/modfile"
)

// Key returns the key of a named type: its package path, a dot and its name
// without type arguments, so every instantiation of a generic type shares the
// comment of its declaration. Key returns "" for an unnamed type and for a
// predeclared one. A type of a package main has the package path "main" in
// a binary built with go build or go run, and its import path in a test
// binary, so Read writes its entries under both.
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
	First  string // the file that described it first, in walk order
	Second string // the first file that described it otherwise
}

// Read walks each directory and returns the descriptions of the types
// declared under it, keyed as Key does, and the keys it left out because two
// files describe them differently, sorted by key.
//
// It skips the directories named vendor or testdata and those whose name
// starts with "." or "_", below the directory given, the files whose name
// starts with "." or "_", and the _test.go files. Every other file is read
// whatever its build constraints, so the result is the same on every
// platform. Every package-level named type with a doc comment has an entry,
// unexported ones too, and so does every exported field of a package-level
// struct type with a comment, an embedded one under the name of its type.
// Aliases and types declared inside a function have none.
//
// The import path of a package is the module path of the nearest go.mod above
// it joined with its directory below that go.mod, or what follows the last
// vendor element of that directory. The entries of a package main are written
// under its import path and under "main". A key that two files describe
// differently is left out, even when a third file repeats one of the texts,
// and reported as a Conflict: no description is better than a wrong one.
func Read(dirs ...string) (map[string]string, []Conflict, error) {
	rd := &reader{
		out:       map[string]string{},
		from:      map[string]string{},
		conflicts: map[string]Conflict{},
		structs:   map[string]bool{},
		modules:   map[string]string{},
	}
	for _, dir := range dirs {
		err := rd.walk(dir)
		if err != nil {
			return nil, nil, ez.Wrap(err)
		}
	}
	rd.copyDefinedFields()

	conflicts := make([]Conflict, 0, len(rd.conflicts))
	for _, conflict := range rd.conflicts {
		conflicts = append(conflicts, conflict)
	}
	sort.Slice(conflicts, func(i, j int) bool {
		return conflicts[i].Key < conflicts[j].Key
	})
	return rd.out, conflicts, nil
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
	out       map[string]string
	from      map[string]string   // key -> file that described it first
	conflicts map[string]Conflict // keys left out
	structs   map[string]bool     // keys of the struct types read
	defined   []definedType       // type A B, in walk order
	modules   map[string]string   // directory holding a go.mod -> its module path
}

// definedType is a type defined over a named type of its own package,
// type A B, both as keys.
type definedType struct {
	key  string
	over string
	path string
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

	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
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
		if !entry.Type().IsRegular() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || ignored(name) {
			return nil
		}
		return rd.readFile(path)
	})
}

// ignored reports a name the go command ignores, a file or a directory.
func ignored(name string) bool {
	return strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}

// skipDir reports a directory the go command leaves out of ./...
func skipDir(name string) bool {
	return name == "vendor" || name == "testdata" || ignored(name)
}

func (rd *reader) readFile(path string) error {
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return ez.New(ez.EINVALID, "cannot parse "+path+": "+err.Error(), err)
	}

	pkgPath, err := rd.importPath(filepath.Dir(path))
	if err != nil {
		return ez.Wrap(err)
	}
	prefixes := []string{pkgPath}
	if file.Name.Name == "main" {
		prefixes = append(prefixes, "main")
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
			for _, prefix := range prefixes {
				rd.readType(path, prefix, gen, typeSpec)
			}
		}
	}
	return nil
}

// readType records the comment of a type and of its fields under the
// package path prefix.
func (rd *reader) readType(path, prefix string, gen *ast.GenDecl, spec *ast.TypeSpec) {
	key := prefix + "." + spec.Name.Name
	rd.add(path, key, CommentText(TypeComment(gen, spec)))

	over := localTypeName(spec.Type)
	if over != "" {
		rd.defined = append(rd.defined, definedType{key: key, over: prefix + "." + over, path: path})
	}

	structType, ok := spec.Type.(*ast.StructType)
	if !ok {
		return
	}
	rd.structs[key] = true
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
		for _, name := range names {
			if name.IsExported() {
				rd.add(path, key+"."+name.Name, text)
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

// copyDefinedFields gives each type defined over a struct of its own package,
// directly or through a chain (type C A, type A B), the field entries of
// that struct.
func (rd *reader) copyDefinedFields() {
	over := map[string]string{}
	for _, d := range rd.defined {
		over[d.key] = d.over
	}

	var keys []string
	for key := range rd.out {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, d := range rd.defined {
		target := d.over
		for range len(rd.defined) {
			next, ok := over[target]
			if rd.structs[target] || !ok {
				break
			}
			target = next
		}
		if !rd.structs[target] || target == d.key {
			continue
		}
		for _, key := range keys {
			field, ok := strings.CutPrefix(key, target+".")
			if ok && !strings.Contains(field, ".") {
				rd.add(d.path, d.key+"."+field, rd.out[key])
			}
		}
	}
}

// add records text under key, unless text is empty. A key described
// differently twice is left out for good.
func (rd *reader) add(path, key, text string) {
	if text == "" {
		return
	}
	_, conflicted := rd.conflicts[key]
	if conflicted {
		return
	}
	previous, ok := rd.out[key]
	if ok && previous != text {
		rd.conflicts[key] = Conflict{Key: key, First: rd.from[key], Second: path}
		delete(rd.out, key)
		return
	}
	if !ok {
		rd.from[key] = path
	}
	rd.out[key] = text
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
