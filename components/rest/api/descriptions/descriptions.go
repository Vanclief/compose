// Package descriptions reads the doc comments of Go types and of their fields
// from source, so the route registry, which builds its documents by
// reflection where comments do not exist, can describe what it publishes.
//
// A build step runs Read (through `compose descriptions`) and writes the
// result with Marshal to a file the app embeds, and api.Descriptions loads
// that file. The keys are Key(t) for a type and Key(t) + "." + the Go field
// name for a field: "github.com/x/app/runs.Run" and
// "github.com/x/app/runs.Run.Status".
package descriptions

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/vanclief/ez"
	"golang.org/x/mod/modfile"
)

// Key returns the key of a named type: its package path, a dot and its name
// without type arguments, so every instantiation of a generic type shares the
// comment of its declaration. A type declared in a package main has the path
// "main", as reflect reports it. Key returns "" for an unnamed type and for a
// predeclared one.
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

// Read walks each directory and returns the descriptions of the types
// declared under it, keyed as Key does. It skips the directories named vendor
// or testdata and those whose name starts with "." or "_", below the
// directory given, and the _test.go files. Every package-level named type
// with a doc comment has an entry, unexported ones too, as an exported field
// of an unexported embedded struct is published, and so does every named
// exported field of a package-level struct type with a comment. Aliases,
// types declared inside a function and embedded fields have none. The import
// path of a package is the module path of the nearest go.mod above it joined
// with its directory below that go.mod, or "main" for a package main. Two
// files that describe one key differently fail, naming both.
func Read(dirs ...string) (map[string]string, error) {
	rd := &reader{
		out:     map[string]string{},
		from:    map[string]string{},
		modules: map[string]string{},
	}
	for _, dir := range dirs {
		err := rd.walk(dir)
		if err != nil {
			return nil, ez.Wrap(err)
		}
	}
	return rd.out, nil
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
	out     map[string]string
	from    map[string]string // key -> file that described it
	modules map[string]string // directory holding a go.mod -> its module path
}

func (rd *reader) walk(root string) error {
	info, err := os.Stat(root)
	if err != nil {
		return ez.New(ez.EINVALID, "cannot read "+root+": "+err.Error(), err)
	}
	if !info.IsDir() {
		return ez.New(ez.EINVALID, root+" is not a directory", nil)
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
		if !entry.Type().IsRegular() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		return rd.readFile(path)
	})
}

// skipDir reports a directory the go command leaves out of ./...
func skipDir(name string) bool {
	return name == "vendor" || name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}

func (rd *reader) readFile(path string) error {
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return ez.New(ez.EINVALID, "cannot parse "+path+": "+err.Error(), err)
	}

	pkgPath := "main"
	if file.Name.Name != "main" {
		pkgPath, err = rd.importPath(filepath.Dir(path))
		if err != nil {
			return ez.Wrap(err)
		}
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
			err = rd.readType(path, pkgPath+"."+typeSpec.Name.Name, gen, typeSpec)
			if err != nil {
				return ez.Wrap(err)
			}
		}
	}
	return nil
}

// readType records the comment of a type and of its fields.
func (rd *reader) readType(path, key string, gen *ast.GenDecl, spec *ast.TypeSpec) error {
	err := rd.add(path, key, CommentText(TypeComment(gen, spec)))
	if err != nil {
		return ez.Wrap(err)
	}

	structType, ok := spec.Type.(*ast.StructType)
	if !ok {
		return nil
	}
	for _, field := range structType.Fields.List {
		text := FieldText(field)
		for _, name := range field.Names {
			if !name.IsExported() {
				continue
			}
			err = rd.add(path, key+"."+name.Name, text)
			if err != nil {
				return ez.Wrap(err)
			}
		}
	}
	return nil
}

func (rd *reader) add(path, key, text string) error {
	if text == "" {
		return nil
	}
	previous, ok := rd.out[key]
	if ok && previous != text {
		return ez.New(ez.EINVALID, fmt.Sprintf("%s is described differently in %s and in %s", key, rd.from[key], path), nil)
	}
	rd.out[key] = text
	rd.from[key] = path
	return nil
}

// importPath returns the import path of the package in dir: the module path
// of the nearest go.mod at or above dir, joined with dir's path below it.
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
				return "", ez.Wrap(err)
			}
			if rel == "." {
				return modulePath, nil
			}
			return modulePath + "/" + filepath.ToSlash(rel), nil
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
