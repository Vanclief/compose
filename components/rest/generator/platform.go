package generator

import (
	"bytes"
	"go/ast"
	"go/build"
	"go/types"
	"io"
	"strconv"
	"strings"

	"github.com/vanclief/compose/components/rest/api/descriptions"
)

// ports are the GOOS/GOARCH pairs Go builds for, as `go tool dist list` prints them on Go 1.26
// and Go 1.27.
var ports = strings.Fields(`aix/ppc64 android/386 android/amd64 android/arm android/arm64
darwin/amd64 darwin/arm64 dragonfly/amd64 freebsd/386 freebsd/amd64 freebsd/arm freebsd/arm64
illumos/amd64 ios/amd64 ios/arm64 js/wasm linux/386 linux/amd64 linux/arm linux/arm64
linux/loong64 linux/mips linux/mips64 linux/mips64le linux/mipsle linux/ppc64 linux/ppc64le
linux/riscv64 linux/s390x netbsd/386 netbsd/amd64 netbsd/arm netbsd/arm64 openbsd/386
openbsd/amd64 openbsd/arm openbsd/arm64 openbsd/ppc64 openbsd/riscv64 plan9/386 plan9/amd64
plan9/arm solaris/amd64 wasip1/wasm windows/386 windows/amd64 windows/arm64`)

// platformSet is a set of platforms, a bit per port with cgo off and one per port with cgo on.
type platformSet [2]uint64

// platformCount is the number of platforms a platformSet holds.
var platformCount = 2 * len(ports)

func (s *platformSet) add(i int) {
	s[i/64] |= 1 << (i % 64)
}

func (s platformSet) union(other platformSet) platformSet {
	return platformSet{s[0] | other[0], s[1] | other[1]}
}

// within reports whether every platform of s is in other.
func (s platformSet) within(other platformSet) bool {
	return s[0]&^other[0] == 0 && s[1]&^other[1] == 0
}

// toolTags are the GOEXPERIMENT tags files are selected with: those of the environment the
// generator runs in, which go/build reads at run time, except goexperiment.jsonv2, which follows
// the rules compiled into the generator (encodingJSONV2), so that a dependency's files are read as
// the toolchain that builds the generator builds them. The tags go/build sets for the GOARCH the
// generator runs on, such as amd64.v1, are left out, so a file that needs one is not read.
var toolTags = func() []string {
	var tags []string
	for _, tag := range build.Default.ToolTags {
		if strings.HasPrefix(tag, "goexperiment.") && tag != "goexperiment.jsonv2" {
			tags = append(tags, tag)
		}
	}
	if encodingJSONV2 {
		tags = append(tags, "goexperiment.jsonv2")
	}
	return tags
}()

// filePlatforms is the set of platforms whose build selects the file name of a module package
// in dir, holding src: its GOOS and GOARCH file-name suffixes and its //go:build line, read by
// go/build for each port with cgo off and on, with toolTags, the release tags of the toolchain
// the generator runs on and no custom tags. go/build reads only the file's header and does not
// require a valid package clause, so a file selected on no platform, such as a template marked
// //go:build ignore, is left out before it is parsed. Its error says why the constraints cannot
// be evaluated.
func filePlatforms(dir string, name string, src []byte) (platformSet, error) {
	var selected platformSet
	for i := range platformCount {
		goos, goarch, _ := strings.Cut(ports[i%len(ports)], "/")
		ctxt := build.Default
		ctxt.GOOS = goos
		ctxt.GOARCH = goarch
		ctxt.CgoEnabled = i >= len(ports)
		ctxt.BuildTags = nil
		ctxt.ToolTags = toolTags
		ctxt.OpenFile = func(string) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(src)), nil
		}
		match, err := ctxt.MatchFile(dir, name)
		if err != nil {
			return platformSet{}, err
		}
		if match {
			selected.add(i)
		}
	}

	return selected, nil
}

// cgoPlatforms holds the platforms with cgo on, the only ones that build a file importing "C".
var cgoPlatforms = func() platformSet {
	var cgo platformSet
	for i := len(ports); i < platformCount; i++ {
		cgo.add(i)
	}
	return cgo
}()

func (s platformSet) intersect(other platformSet) platformSet {
	return platformSet{s[0] & other[0], s[1] & other[1]}
}

// sameDeclarations reports declarations of one type name, read from several files, that declare
// the same type (declarationShape), each file importing the same package with a dot import, if
// any.
func sameDeclarations(decls []typeSpecInfo, docComments bool) bool {
	first := declarationShape(decls[0], docComments)
	for _, decl := range decls[1:] {
		if declarationShape(decl, docComments) != first || decl.Imports["."] != decls[0].Imports["."] {
			return false
		}
	}

	return true
}

// declarationShape writes a type declaration so that two declarations of the same type, an
// alias or not, read the same whatever their layout: its type parameters and type with each
// package qualifier written as its import path, parentheses dropped and names declared together
// written one by one, a struct field with its tag as reflect.StructTag reads it and the
// deprecation the generator reads from its comments (hasDeprecatedParagraph), which the document
// shows, and, with docComments (Config.DocComments), the field's description
// (descriptions.FieldText) and the type's doc comment, which the document then shows too. A
// struct field that is neither exported nor embedded is left out, as encoding/json does not
// write it.
func declarationShape(decl typeSpecInfo, docComments bool) string {
	var out strings.Builder
	if docComments {
		out.WriteString(strconv.Quote(descriptions.CommentText(decl.Doc)))
	}
	if decl.Spec.TypeParams != nil {
		out.WriteString("[" + fieldListShape(decl.Spec.TypeParams, decl.Imports, false, docComments) + "]")
	}
	if decl.Spec.Assign.IsValid() {
		out.WriteString("=")
	}
	out.WriteString(typeShape(decl.Spec.Type, decl.Imports, docComments))

	return out.String()
}

// typeShape writes a type expression for declarationShape.
func typeShape(expr ast.Expr, imports map[string]string, docComments bool) string {
	switch t := expr.(type) {
	case nil:
		return ""
	case *ast.Ident:
		return t.Name
	case *ast.ParenExpr:
		return typeShape(t.X, imports, docComments)
	case *ast.SelectorExpr:
		qualifier, ok := t.X.(*ast.Ident)
		if ok && imports[qualifier.Name] != "" {
			return strconv.Quote(imports[qualifier.Name]) + "." + t.Sel.Name
		}
		return types.ExprString(t)
	case *ast.StarExpr:
		return "*" + typeShape(t.X, imports, docComments)
	case *ast.ArrayType:
		length := ""
		if t.Len != nil {
			length = types.ExprString(t.Len)
		}
		return "[" + length + "]" + typeShape(t.Elt, imports, docComments)
	case *ast.Ellipsis:
		return "..." + typeShape(t.Elt, imports, docComments)
	case *ast.MapType:
		return "map[" + typeShape(t.Key, imports, docComments) + "]" + typeShape(t.Value, imports, docComments)
	case *ast.ChanType:
		direction := "chan "
		if t.Dir == ast.SEND {
			direction = "chan<- "
		} else if t.Dir == ast.RECV {
			direction = "<-chan "
		}
		return direction + typeShape(t.Value, imports, docComments)
	case *ast.FuncType:
		return "func(" + fieldListShape(t.Params, imports, false, docComments) + ")(" + fieldListShape(t.Results, imports, false, docComments) + ")"
	case *ast.StructType:
		return "struct{" + fieldListShape(t.Fields, imports, true, docComments) + "}"
	case *ast.InterfaceType:
		return "interface{" + fieldListShape(t.Methods, imports, false, docComments) + "}"
	case *ast.IndexExpr:
		return typeShape(t.X, imports, docComments) + "[" + typeShape(t.Index, imports, docComments) + "]"
	case *ast.IndexListExpr:
		arguments := make([]string, 0, len(t.Indices))
		for _, index := range t.Indices {
			arguments = append(arguments, typeShape(index, imports, docComments))
		}
		return typeShape(t.X, imports, docComments) + "[" + strings.Join(arguments, ",") + "]"
	case *ast.UnaryExpr:
		return t.Op.String() + typeShape(t.X, imports, docComments)
	case *ast.BinaryExpr:
		return typeShape(t.X, imports, docComments) + t.Op.String() + typeShape(t.Y, imports, docComments)
	}

	return types.ExprString(expr)
}

// fieldListShape writes a field, parameter or method list for declarationShape, one entry per
// name; fields adds a struct field's tag and comments and leaves out a name that is not exported.
func fieldListShape(list *ast.FieldList, imports map[string]string, fields bool, docComments bool) string {
	if list == nil {
		return ""
	}
	var entries []string
	for _, field := range list.List {
		suffix := " " + typeShape(field.Type, imports, docComments)
		if fields {
			suffix += " " + tagShape(field) + " " + strconv.FormatBool(hasDeprecatedParagraph(field))
		}
		if fields && docComments {
			suffix += " " + strconv.Quote(descriptions.FieldText(field))
		}
		if len(field.Names) == 0 {
			entries = append(entries, suffix)
			continue
		}
		for _, name := range field.Names {
			if fields && !name.IsExported() {
				continue
			}
			entries = append(entries, name.Name+suffix)
		}
	}

	return strings.Join(entries, ";")
}

// tagShape writes a struct field's tag as reflect.StructTag reads it: each key with the value
// its first occurrence gives, in key order, so the spacing between keys and a raw or an
// interpreted string literal do not count. A key whose first value does not unquote reads as
// absent, as reflect.StructTag.Lookup then finds nothing.
func tagShape(field *ast.Field) string {
	if field.Tag == nil {
		return ""
	}
	tag, err := strconv.Unquote(field.Tag.Value)
	if err != nil {
		// The parser only gives a tag that is a valid string literal.
		return field.Tag.Value
	}

	// The loop of reflect.StructTag.Lookup, over every key.
	values := map[string]string{}
	seen := map[string]bool{}
	for tag != "" {
		i := 0
		for i < len(tag) && tag[i] == ' ' {
			i++
		}
		tag = tag[i:]
		if tag == "" {
			break
		}
		i = 0
		for i < len(tag) && tag[i] > ' ' && tag[i] != ':' && tag[i] != '"' && tag[i] != 0x7f {
			i++
		}
		if i == 0 || i+1 >= len(tag) || tag[i] != ':' || tag[i+1] != '"' {
			break
		}
		name := tag[:i]
		tag = tag[i+1:]
		i = 1
		for i < len(tag) && tag[i] != '"' {
			if tag[i] == '\\' {
				i++
			}
			i++
		}
		if i >= len(tag) {
			break
		}
		quoted := tag[:i+1]
		tag = tag[i+1:]
		if seen[name] {
			continue
		}
		seen[name] = true
		value, err := strconv.Unquote(quoted)
		if err == nil {
			values[name] = value
		}
	}

	var pairs []string
	for _, name := range sortedKeys(values) {
		pairs = append(pairs, name+":"+strconv.Quote(values[name]))
	}

	return strings.Join(pairs, " ")
}
