package descriptions_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/vanclief/ez"

	"github.com/vanclief/compose/components/rest/api/descriptions"
)

const tree = "testdata/tree"

// treePath is the import path of testdata/tree: it has no go.mod of its own,
// so its packages are in compose's module.
const treePath = "github.com/vanclief/compose/components/rest/api/descriptions/testdata/tree"

// TestRead lists every entry of the tree, so it also proves what is left
// out: the aliases Alias and Base, the embedded fields of Run, its unexported
// field secret and its field Plain without a comment, Left and Right under a
// comment that heads both, Noted with only a maintainer note, Local declared
// in a function, Fake in a _test.go file, and the directories testdata,
// _skipped and .hidden.
func TestRead(t *testing.T) {
	got, err := descriptions.Read(tree)
	require.NoError(t, err)

	const models = treePath + "/models"
	require.Equal(t, map[string]string{
		// On the declaration, without the Deprecated paragraph.
		models + ".Run": "Run is one execution of a job.",
		// A doc comment, then the line comment.
		models + ".Run.Name": "Name is what the user calls the run.\n\nIt is unique.",
		// Without the TODO line.
		models + ".Run.Title": "Title is shown in lists.",
		// Without the Deprecated paragraph.
		models + ".Run.Old": "Old is kept for old clients.",
		// Two names, one comment.
		models + ".Run.First":  "First and Second share one comment.",
		models + ".Run.Second": "First and Second share one comment.",
		// On the spec, inside parentheses.
		models + ".Step":       "Step is one step of a run.",
		models + ".Step.Order": "Order is where the step runs.",
		models + ".Kind":       "Kind is a named non-struct type.",
		// Keyed without its type parameters.
		models + ".Page":       "Page is one page of a list.",
		models + ".Page.Items": "Items are the entries of the page.",
		// Unexported types are recorded: their exported fields may be
		// promoted into a published type.
		models + ".base":    "base is unexported, but its exported fields are promoted.",
		models + ".base.ID": "ID identifies the record.",
		models + ".Extra":   "Extra is embedded through a pointer.",
		// A field whose type is an anonymous struct, whose own fields have
		// no key.
		models + ".Extra.Meta": "Meta is a nested struct without a name.",
		// A package main is keyed under main, and the same text in two of
		// them is fine.
		"main.Config":      "Config is the configuration of a command.",
		"main.Config.Port": "Port is where it listens.",
	}, got)

	// A directory given is read even when the walk would skip it.
	data, err := descriptions.Read(filepath.Join(tree, "testdata"))
	require.NoError(t, err)
	require.Equal(t, map[string]string{treePath + "/testdata.Data": "Data is in testdata."}, data)
}

// TestReadModules builds its tree at run time, as the module zip leaves out
// a directory with a go.mod of its own and a vendor directory.
func TestReadModules(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"go.mod":                        "module example.com/app\n\ngo 1.26\n",
		"api/api.go":                    "package api\n\n// Item is sold.\ntype Item struct{}\n",
		"vendor/example.com/dep/dep.go": "package dep\n\n// Dep is vendored.\ntype Dep struct{}\n",
		"tools/go.mod":                  "module example.com/tools\n\ngo 1.26\n",
		"tools/gen/gen.go":              "package gen\n\n// Gen is in a module of its own.\ntype Gen struct{}\n",
		"orphan/go.mod":                 "go 1.26\n",
	}
	for name, content := range files {
		path := filepath.Join(root, name)
		err := os.MkdirAll(filepath.Dir(path), 0o755)
		require.NoError(t, err)
		err = os.WriteFile(path, []byte(content), 0o644)
		require.NoError(t, err)
	}

	// The vendor directory is skipped, and a directory below a go.mod of
	// its own belongs to that module.
	got, err := descriptions.Read(root)
	require.NoError(t, err)
	require.Equal(t, map[string]string{
		"example.com/app/api.Item":  "Item is sold.",
		"example.com/tools/gen.Gen": "Gen is in a module of its own.",
	}, got)

	// Two directories, one inside the other, read the same files twice.
	got, err = descriptions.Read(filepath.Join(root, "api"), root)
	require.NoError(t, err)
	require.Len(t, got, 2)

	err = os.WriteFile(filepath.Join(root, "orphan", "o.go"), []byte("package orphan\n\n// O has no module path.\ntype O struct{}\n"), 0o644)
	require.NoError(t, err)
	_, err = descriptions.Read(filepath.Join(root, "orphan"))
	require.Error(t, err)
	require.Contains(t, ez.ErrorMessage(err), filepath.Join(root, "orphan", "go.mod")+" declares no module path")
}

func TestReadConflict(t *testing.T) {
	_, err := descriptions.Read("testdata/conflict")
	require.Error(t, err)
	require.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	require.Equal(t, "main.Config is described differently in "+
		filepath.Join("testdata", "conflict", "a", "main.go")+" and in "+
		filepath.Join("testdata", "conflict", "b", "main.go"), ez.ErrorMessage(err))
}

func TestReadErrors(t *testing.T) {
	_, err := descriptions.Read("testdata/missing")
	require.Error(t, err)
	require.Contains(t, ez.ErrorMessage(err), "cannot read testdata/missing")

	_, err = descriptions.Read(filepath.Join(tree, "models", "models.go"))
	require.Error(t, err)
	require.Contains(t, ez.ErrorMessage(err), "is not a directory")

	dir := t.TempDir()
	err = os.WriteFile(filepath.Join(dir, "broken.go"), []byte("package broken\n\ntype {"), 0o644)
	require.NoError(t, err)
	_, err = descriptions.Read(dir)
	require.Error(t, err)
	require.Contains(t, ez.ErrorMessage(err), "cannot parse "+filepath.Join(dir, "broken.go"))
}

func TestMarshalStable(t *testing.T) {
	first, err := descriptions.Read(tree)
	require.NoError(t, err)
	a, err := descriptions.Marshal(first)
	require.NoError(t, err)

	second, err := descriptions.Read(tree)
	require.NoError(t, err)
	b, err := descriptions.Marshal(second)
	require.NoError(t, err)
	require.Equal(t, string(a), string(b))

	out, err := descriptions.Marshal(map[string]string{"b.T": "<b> & c", "a.T": "line\nline"})
	require.NoError(t, err)
	require.Equal(t, "{\n  \"a.T\": \"line\\nline\",\n  \"b.T\": \"<b> & c\"\n}\n", string(out))

	empty, err := descriptions.Marshal(nil)
	require.NoError(t, err)
	require.Equal(t, "{}\n", string(empty))
}

type page[T any] struct{ Items []T }

type item struct{}

func TestKey(t *testing.T) {
	const pkg = "github.com/vanclief/compose/components/rest/api/descriptions_test"
	require.Equal(t, pkg+".item", descriptions.Key(reflect.TypeFor[item]()))
	require.Equal(t, pkg+".page", descriptions.Key(reflect.TypeFor[page[item]]()))
	require.Equal(t, pkg+".page", descriptions.Key(reflect.TypeFor[page[map[string]page[int]]]()))
	require.Equal(t, "", descriptions.Key(reflect.TypeFor[struct{ A int }]()))
	require.Equal(t, "", descriptions.Key(reflect.TypeFor[int]()))
	require.Equal(t, "", descriptions.Key(reflect.TypeFor[*item]()))
}

func TestTypeComment(t *testing.T) {
	src := "package p\n\n// One is alone.\ntype One struct{}\n\n// Group heads two.\ntype (\n\tA struct{}\n\t// B has its own.\n\tB struct{}\n)\n"
	file, err := parser.ParseFile(token.NewFileSet(), "p.go", src, parser.ParseComments)
	require.NoError(t, err)

	got := map[string]string{}
	for _, decl := range file.Decls {
		gen := decl.(*ast.GenDecl)
		for _, spec := range gen.Specs {
			typeSpec := spec.(*ast.TypeSpec)
			got[typeSpec.Name.Name] = descriptions.CommentText(descriptions.TypeComment(gen, typeSpec))
		}
	}
	require.Equal(t, map[string]string{"One": "One is alone.", "A": "", "B": "B has its own."}, got)
}
