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
//
// A directory is keyed by where it is, after symbolic links are followed: a
// package directory that is a link to another module's package is keyed by
// that module's import path, though a binary built from the linking module
// reports the linking module's.
package descriptions

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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
// Below each directory given, it skips the directories named vendor or
// testdata and those whose name starts with "." or "_". It skips the files
// whose name starts with "." or "_" and the _test.go files. Every other file
// is read whatever its build constraints, so the result is the same on every
// platform, and so is a symbolic link to a file. Every package-level named
// type with a doc comment has an entry, unexported ones too, and so does
// every exported field of a package-level struct type with a comment, an
// embedded one under the name of its type. Aliases and types declared inside
// a function have none.
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
	structs   map[string]bool     // import-path keys of the struct types read
	defined   []definedType       // type A B, in walk order
	modules   map[string]string   // directory holding a go.mod -> its module path
}

// definedType is a type defined over a named type of its own package,
// type A B.
type definedType struct {
	pkg    string // the import path of the package
	isMain bool   // a package main, whose entries are also written under main
	name   string // A
	over   string // B
	path   string // the file that declares A
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

func (rd *reader) readFile(path string) error {
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return ez.New(ez.EINVALID, "cannot parse "+path+": "+err.Error(), err)
	}

	pkgPath, err := rd.importPath(filepath.Dir(path))
	if err != nil {
		return ez.Wrap(err)
	}
	isMain := file.Name.Name == "main"

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
			rd.readType(path, pkgPath, isMain, gen, typeSpec)
		}
	}
	return nil
}

// readType records the comment of a type and of its fields.
func (rd *reader) readType(path, pkgPath string, isMain bool, gen *ast.GenDecl, spec *ast.TypeSpec) {
	name := spec.Name.Name
	rd.addAll(path, pkgPath, isMain, name, CommentText(TypeComment(gen, spec)))

	over := localTypeName(spec.Type)
	if over != "" {
		rd.defined = append(rd.defined, definedType{pkg: pkgPath, isMain: isMain, name: name, over: over, path: path})
	}

	structType, ok := spec.Type.(*ast.StructType)
	if !ok {
		return
	}
	rd.structs[pkgPath+"."+name] = true
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
				rd.addAll(path, pkgPath, isMain, name+"."+fieldName.Name, text)
			}
		}
	}
}

// addAll adds text under the import path pkgPath and name, and for a package
// main under main and name too.
func (rd *reader) addAll(path, pkgPath string, isMain bool, name, text string) {
	rd.add(path, pkgPath+"."+name, text)
	if isMain {
		rd.add(path, "main."+name, text)
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
// directly or through a chain (type C A, type A B), the field entries that
// struct has under the package's import path, through add, so a copy that
// disagrees with another text conflicts. A field of that struct left out as a
// conflict is left out under the defined type too. A type of the chain that
// two files of the package declare over different types ends it.
func (rd *reader) copyDefinedFields() {
	over := map[string][]string{} // import-path key of A -> those of the types it is declared over
	for _, d := range rd.defined {
		key := d.pkg + "." + d.name
		target := d.pkg + "." + d.over
		if !slices.Contains(over[key], target) {
			over[key] = append(over[key], target)
		}
	}

	// The fields are copied as the walk read them, not as other copies
	// changed them.
	texts := maps.Clone(rd.out)
	conflicts := maps.Clone(rd.conflicts)
	sources := slices.Sorted(maps.Keys(texts))
	sources = append(sources, slices.Sorted(maps.Keys(conflicts))...)

	for _, d := range rd.defined {
		target := d.pkg + "." + d.over
		for range len(rd.defined) {
			next := over[target]
			if rd.structs[target] || len(next) != 1 {
				break
			}
			target = next[0]
		}
		if !rd.structs[target] {
			continue
		}
		for _, source := range sources {
			field, ok := strings.CutPrefix(source, target+".")
			if !ok || strings.Contains(field, ".") {
				continue
			}
			name := d.name + "." + field
			conflict, conflicted := conflicts[source]
			if !conflicted {
				rd.addAll(d.path, d.pkg, d.isMain, name, texts[source])
				continue
			}
			rd.leaveOut(d.pkg+"."+name, conflict)
			if d.isMain {
				rd.leaveOut("main."+name, conflict)
			}
		}
	}
}

// leaveOut puts key in conflict with the files of the conflict it follows
// from, unless it is in conflict already.
func (rd *reader) leaveOut(key string, from Conflict) {
	_, conflicted := rd.conflicts[key]
	if conflicted {
		return
	}
	delete(rd.out, key)
	rd.conflicts[key] = Conflict{Key: key, First: from.First, Second: from.Second}
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
