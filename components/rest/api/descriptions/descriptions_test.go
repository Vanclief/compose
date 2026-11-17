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

// read is Read for a tree without conflicts.
func read(t *testing.T, dirs ...string) map[string]string {
	t.Helper()
	got, conflicts, err := descriptions.Read(dirs...)
	require.NoError(t, err)
	require.Empty(t, conflicts)
	return got
}

// TestRead lists every entry of the tree, so it also proves what is left
// out: the aliases Alias and Base, the embedded field Base without a
// comment, the unexported field secret of Run and its field Plain without a
// comment, Left and Right under a comment that heads both, Noted with only a
// maintainer note, Local declared in a function, Task's own comment, which it
// has none of, Fake in a _test.go file, Old and Dotted in files whose name
// starts with _ or ., and the directories testdata, _skipped and .hidden.
func TestRead(t *testing.T) {
	got := read(t, tree)

	const models = treePath + "/models"
	runFields := map[string]string{
		// A doc comment, then the line comment.
		".Name": "Name is what the user calls the run.\n\nIt is unique.",
		// Without the TODO line.
		".Title": "Title is shown in lists.",
		// Without the Deprecated paragraph.
		".Old": "Old is kept for old clients.",
		// Two names, one comment.
		".First":  "First and Second share one comment.",
		".Second": "First and Second share one comment.",
		// An embedded field, under its type's name.
		".Extra": "Extra is what a run may carry.",
	}
	want := map[string]string{
		// On the declaration, without the Deprecated paragraph.
		models + ".Run": "Run is one execution of a job.",
		// On the spec, inside parentheses.
		models + ".Step":       "Step is one step of a run.",
		models + ".Step.Order": "Order is where the step runs.",
		models + ".Kind":       "Kind is a named non-struct type.",
		// Keyed without its type parameters.
		models + ".Page":       "Page is one page of a list.",
		models + ".Page.Items": "Items are the entries of the page.",
		// A type defined over a generic struct gets its fields.
		models + ".RunPage":       "RunPage is a page of runs.",
		models + ".RunPage.Items": "Items are the entries of the page.",
		models + ".Job":           "Job is a run under another name.",
		// Unexported types are recorded.
		models + ".base":    "base is unexported, but its exported fields are promoted.",
		models + ".base.ID": "ID identifies the record.",
		models + ".Extra":   "Extra is embedded through a pointer.",
		// A field whose type is an anonymous struct, whose own fields have
		// no key.
		models + ".Extra.Meta": "Meta is a nested struct without a name.",
		// A package main is keyed under main and under its import path, and
		// the same text in two of them is fine.
		"main.Config":                   "Config is the configuration of a command.",
		"main.Config.Port":              "Port is where it listens.",
		treePath + "/cmd/a.Config":      "Config is the configuration of a command.",
		treePath + "/cmd/a.Config.Port": "Port is where it listens.",
		treePath + "/cmd/b.Config":      "Config is the configuration of a command.",
		treePath + "/cmd/b.Config.Port": "Port is where it listens.",
	}
	// Job is defined over Run, and Task over Job: both get Run's fields.
	for _, typ := range []string{".Run", ".Job", ".Task"} {
		for field, text := range runFields {
			want[models+typ+field] = text
		}
	}
	require.Equal(t, want, got)

	// A directory given is read even when the walk would skip it.
	require.Equal(t, map[string]string{treePath + "/testdata.Data": "Data is in testdata."}, read(t, filepath.Join(tree, "testdata")))
}

// TestReadConflict reads two main packages that describe Config
// differently, a third that repeats the first text, and one type in a cgo
// and a !cgo file. The conflicting keys are left out and reported, and the
// fields that read the same everywhere are kept.
func TestReadConflict(t *testing.T) {
	const dir = "testdata/conflict"
	const pkg = "github.com/vanclief/compose/components/rest/api/descriptions/testdata/conflict"

	got, conflicts, err := descriptions.Read(dir)
	require.NoError(t, err)
	require.Equal(t, map[string]string{
		"main.Config.Port":          "Port is where it listens.",
		pkg + "/a.Config":           "Config is the configuration of a.",
		pkg + "/a.Config.Port":      "Port is where it listens.",
		pkg + "/b.Config":           "Config is the configuration of b.",
		pkg + "/b.Config.Port":      "Port is where it listens.",
		pkg + "/c.Config":           "Config is the configuration of a.",
		pkg + "/c.Config.Port":      "Port is where it listens.",
		pkg + "/engine.Engine.Name": "Name names the engine.",
	}, got)
	require.Equal(t, []descriptions.Conflict{
		{
			Key:    pkg + "/engine.Engine",
			First:  filepath.Join(dir, "engine", "engine_cgo.go"),
			Second: filepath.Join(dir, "engine", "engine_nocgo.go"),
		},
		{
			Key:    "main.Config",
			First:  filepath.Join(dir, "a", "main.go"),
			Second: filepath.Join(dir, "b", "main.go"),
		},
	}, conflicts)

	// The same source gives the same result.
	again, againConflicts, err := descriptions.Read(dir)
	require.NoError(t, err)
	require.Equal(t, got, again)
	require.Equal(t, conflicts, againConflicts)
}

// TestReadModules builds its tree at run time, as the module zip leaves out
// a directory with a go.mod of its own and a vendor directory.
func TestReadModules(t *testing.T) {
	root := writeTree(t, map[string]string{
		"go.mod":                        "module example.com/app\n\ngo 1.26\n",
		"api/api.go":                    "package api\n\n// Item is sold.\ntype Item struct{}\n",
		"vendor/example.com/dep/dep.go": "package dep\n\n// Dep is vendored.\ntype Dep struct{}\n",
		"tools/go.mod":                  "module example.com/tools\n\ngo 1.26\n",
		"tools/gen/gen.go":              "package gen\n\n// Gen is in a module of its own.\ntype Gen struct{}\n",
		"orphan/go.mod":                 "go 1.26\n",
	})

	// The vendor directory is skipped, and a directory below a go.mod of
	// its own belongs to that module.
	require.Equal(t, map[string]string{
		"example.com/app/api.Item":  "Item is sold.",
		"example.com/tools/gen.Gen": "Gen is in a module of its own.",
	}, read(t, root))

	// Two directories, one inside the other, read the same files twice.
	require.Len(t, read(t, filepath.Join(root, "api"), root), 2)

	// A vendor directory given is keyed by the path after vendor/.
	vendored := map[string]string{"example.com/dep.Dep": "Dep is vendored."}
	require.Equal(t, vendored, read(t, filepath.Join(root, "vendor")))
	require.Equal(t, vendored, read(t, filepath.Join(root, "vendor", "example.com", "dep")))

	// A symbolic link is followed, and the import path is the target's.
	link := filepath.Join(t.TempDir(), "link")
	err := os.Symlink(filepath.Join(root, "api"), link)
	if err != nil {
		t.Logf("no symbolic link on this platform: %v", err)
	} else {
		require.Equal(t, map[string]string{"example.com/app/api.Item": "Item is sold."}, read(t, link))
		require.Equal(t, map[string]string{"example.com/app/api.Item": "Item is sold."}, read(t, link+string(filepath.Separator)))
	}

	err = os.WriteFile(filepath.Join(root, "orphan", "o.go"), []byte("package orphan\n\n// O has no module path.\ntype O struct{}\n"), 0o644)
	require.NoError(t, err)
	_, _, err = descriptions.Read(filepath.Join(root, "orphan"))
	require.Error(t, err)
	require.Contains(t, ez.ErrorMessage(err), filepath.Join(root, "orphan", "go.mod")+" declares no module path")
}

// writeTree writes files, keyed by their slash-separated path, under a new
// directory and returns it with its symbolic links resolved.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		err = os.MkdirAll(filepath.Dir(path), 0o755)
		require.NoError(t, err)
		err = os.WriteFile(path, []byte(content), 0o644)
		require.NoError(t, err)
	}
	return root
}

// TestReadSymlinkedFile reads a .go file that is a symbolic link to a
// regular file, as Go compiles it, and skips a broken link.
func TestReadSymlinkedFile(t *testing.T) {
	root := writeTree(t, map[string]string{
		"go.mod":            "module example.com/app\n\ngo 1.26\n",
		"api/api.go":        "package api\n\n// Item is sold.\ntype Item struct{}\n",
		"shared/linked.txt": "package api\n\n// Linked is in a linked file.\ntype Linked struct{}\n",
	})
	err := os.Symlink(filepath.Join(root, "shared", "linked.txt"), filepath.Join(root, "api", "linked.go"))
	if err != nil {
		t.Skipf("no symbolic link on this platform: %v", err)
	}
	err = os.Symlink(filepath.Join(root, "missing.go"), filepath.Join(root, "api", "broken.go"))
	require.NoError(t, err)

	require.Equal(t, map[string]string{
		"example.com/app/api.Item":   "Item is sold.",
		"example.com/app/api.Linked": "Linked is in a linked file.",
	}, read(t, filepath.Join(root, "api")))
}

// TestReadDefinedTypes copies the fields of a struct to the types defined
// over it within one package, never from another main package.
func TestReadDefinedTypes(t *testing.T) {
	const mod = "module example.com/app\n\ngo 1.26\n"

	// Run 1: Wrapper in a reaches E1 through Engine. b declares another
	// Engine, over E2, which must not reach a's Wrapper.
	root := writeTree(t, map[string]string{
		"go.mod":        mod,
		"cmd/a/main.go": "package main\n\ntype Wrapper Engine\n\ntype Engine E1\n\ntype E1 struct {\n\tX int // X is e1.\n}\n\nfunc main() {}\n",
		"cmd/b/main.go": "package main\n\ntype Engine E2\n\ntype E2 struct {\n\tX int // X is e2.\n}\n\nfunc main() {}\n",
	})
	a := filepath.Join(root, "cmd", "a")
	b := filepath.Join(root, "cmd", "b")
	want := map[string]string{
		"main.Wrapper.X":                  "X is e1.",
		"main.E1.X":                       "X is e1.",
		"main.E2.X":                       "X is e2.",
		"example.com/app/cmd/a.Wrapper.X": "X is e1.",
		"example.com/app/cmd/a.Engine.X":  "X is e1.",
		"example.com/app/cmd/a.E1.X":      "X is e1.",
		"example.com/app/cmd/b.Engine.X":  "X is e2.",
		"example.com/app/cmd/b.E2.X":      "X is e2.",
	}
	for _, dirs := range [][]string{{a, b}, {b, a}} {
		got, conflicts, err := descriptions.Read(dirs...)
		require.NoError(t, err)
		require.Equal(t, want, got)
		require.Len(t, conflicts, 1)
		require.Equal(t, "main.Engine.X", conflicts[0].Key)
	}

	// Run 2: Job in a is Run of a. b declares its own Job, described
	// otherwise, so main.Job.X conflicts as main.Run.X does.
	root = writeTree(t, map[string]string{
		"go.mod":        mod,
		"cmd/a/main.go": "package main\n\ntype Job Run\n\ntype Run struct {\n\tX int // X is x of a.\n}\n\nfunc main() {}\n",
		"cmd/b/main.go": "package main\n\ntype Run struct {\n\tX int // X is x of b.\n}\n\ntype Job struct {\n\tX int // X is the job x of b.\n}\n\nfunc main() {}\n",
	})
	got, conflicts, err := descriptions.Read(filepath.Join(root, "cmd"))
	require.NoError(t, err)
	require.Equal(t, map[string]string{
		"example.com/app/cmd/a.Job.X": "X is x of a.",
		"example.com/app/cmd/a.Run.X": "X is x of a.",
		"example.com/app/cmd/b.Run.X": "X is x of b.",
		"example.com/app/cmd/b.Job.X": "X is the job x of b.",
	}, got)
	require.Equal(t, []descriptions.Conflict{
		{Key: "main.Job.X", First: filepath.Join(root, "cmd", "b", "main.go"), Second: filepath.Join(root, "cmd", "a", "main.go")},
		{Key: "main.Run.X", First: filepath.Join(root, "cmd", "a", "main.go"), Second: filepath.Join(root, "cmd", "b", "main.go")},
	}, conflicts)

	// A field of the struct left out as a conflict is left out under the
	// defined type too, and a type defined over a struct of another package
	// gets no field entries.
	root = writeTree(t, map[string]string{
		"go.mod":         mod,
		"p/e_cgo.go":     "//go:build cgo\n\npackage p\n\ntype E struct {\n\tX int // X one.\n\tY int // Y is y.\n}\n",
		"p/e_nocgo.go":   "//go:build !cgo\n\npackage p\n\ntype E struct {\n\tX int // X two.\n\tY int // Y is y.\n}\n",
		"p/a.go":         "package p\n\ntype A E\n",
		"other/other.go": "package other\n\nimport \"example.com/app/p\"\n\ntype R p.E\n",
	})
	got, conflicts, err = descriptions.Read(root)
	require.NoError(t, err)
	require.Equal(t, map[string]string{
		"example.com/app/p.E.Y": "Y is y.",
		"example.com/app/p.A.Y": "Y is y.",
	}, got)
	cgo := filepath.Join(root, "p", "e_cgo.go")
	nocgo := filepath.Join(root, "p", "e_nocgo.go")
	require.Equal(t, []descriptions.Conflict{
		{Key: "example.com/app/p.A.X", First: cgo, Second: nocgo},
		{Key: "example.com/app/p.E.X", First: cgo, Second: nocgo},
	}, conflicts)
}

func TestReadErrors(t *testing.T) {
	_, _, err := descriptions.Read("testdata/missing")
	require.Error(t, err)
	require.Contains(t, ez.ErrorMessage(err), "cannot read testdata/missing")

	_, _, err = descriptions.Read(filepath.Join(tree, "models", "models.go"))
	require.Error(t, err)
	require.Contains(t, ez.ErrorMessage(err), "is not a directory")

	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(dir, "broken.go"), []byte("package broken\n\ntype {"), 0o644)
	require.NoError(t, err)
	_, _, err = descriptions.Read(dir)
	require.Error(t, err)
	require.Contains(t, ez.ErrorMessage(err), "cannot parse "+filepath.Join(dir, "broken.go"))
}

func TestMarshalStable(t *testing.T) {
	a, err := descriptions.Marshal(read(t, tree))
	require.NoError(t, err)
	b, err := descriptions.Marshal(read(t, tree))
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
