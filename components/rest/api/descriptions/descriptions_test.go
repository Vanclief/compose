package descriptions_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/vanclief/ez"

	"github.com/vanclief/compose/components/rest/api/descriptions"
)

const tree = "testdata/tree"

// treePath is the import path of testdata/tree: it has no go.mod of its own,
// so its packages are in compose's module.
const treePath = "github.com/vanclief/compose/components/rest/api/descriptions/testdata/tree"

// mod is the go.mod of the trees the tests write.
const mod = "module example.com/app\n\ngo 1.26\n"

// read is Read for a tree without duplicates or several main packages.
func read(t *testing.T, dirs ...string) map[string]string {
	t.Helper()
	result, err := descriptions.Read(dirs...)
	require.NoError(t, err)
	require.Empty(t, result.Duplicates)
	require.Empty(t, result.MainPackages)
	return result.Descriptions
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

// TestRead lists every entry of the tree, so it also proves what is left
// out: the aliases Alias and Base, the embedded field Base without a
// comment, the unexported field secret of Run and its field Plain without a
// comment, Left and Right under a comment that heads both, Noted with only a
// maintainer note, Local declared in a function, Task's own comment, which it
// has none of, Fake in a _test.go file, Old and Dotted in files whose name
// starts with _ or ., Gen in a //go:build ignore file of package main, and
// the directories testdata, _skipped and .hidden.
func TestRead(t *testing.T) {
	got := read(t, tree)

	const models = treePath + "/models"
	const cmd = treePath + "/cmd/a"
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
		// Build constraints other than a //go:build ignore line are not
		// evaluated.
		models + ".Tool":  "Tool is in a file whose constraint is more than ignore.",
		models + ".Linux": "Linux is in a file for linux.",
		// The only main package that declares types is written under its
		// import path and under main, a copied field too.
		cmd + ".Config":        "Config is the configuration of a command.",
		cmd + ".Config.Port":   "Port is where it listens.",
		cmd + ".Settings":      "Settings is Config under another name.",
		cmd + ".Settings.Port": "Port is where it listens.",
		"main.Config":          "Config is the configuration of a command.",
		"main.Config.Port":     "Port is where it listens.",
		"main.Settings":        "Settings is Config under another name.",
		"main.Settings.Port":   "Port is where it listens.",
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

// TestReadDuplicates reads one struct in a cgo, a !cgo and a wasm file, with
// the same comments in all three. It is declared more than once, so neither
// it nor its field is described, and it is reported once, with the first two
// files.
func TestReadDuplicates(t *testing.T) {
	const dir = "testdata/duplicate"
	const pkg = "github.com/vanclief/compose/components/rest/api/descriptions/testdata/duplicate/engine"

	result, err := descriptions.Read(dir)
	require.NoError(t, err)
	require.Empty(t, result.Descriptions)
	require.Equal(t, []descriptions.Duplicate{{
		Key:    pkg + ".Engine",
		First:  filepath.Join(dir, "engine", "engine_cgo.go"),
		Second: filepath.Join(dir, "engine", "engine_nocgo.go"),
	}}, result.Duplicates)
	require.Empty(t, result.MainPackages)

	// One commented and one bare declaration: left out too.
	root := writeTree(t, map[string]string{
		"go.mod":       mod,
		"p/a_cgo.go":   "//go:build cgo\n\npackage p\n\n// A is a.\ntype A struct {\n\tX int // X is x.\n}\n",
		"p/a_nocgo.go": "//go:build !cgo\n\npackage p\n\ntype A struct {\n\tX int\n}\n",
		"p/b.go":       "package p\n\n// B is b.\ntype B struct{}\n",
	})
	result, err = descriptions.Read(root)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"example.com/app/p.B": "B is b."}, result.Descriptions)
	require.Equal(t, []descriptions.Duplicate{{
		Key:    "example.com/app/p.A",
		First:  filepath.Join(root, "p", "a_cgo.go"),
		Second: filepath.Join(root, "p", "a_nocgo.go"),
	}}, result.Duplicates)

	// A duplicate in the only main package has no key under main either,
	// while the other types of that package keep theirs.
	root = writeTree(t, map[string]string{
		"go.mod":            mod,
		"cmd/a/main.go":     "package main\n\n// Config is a's.\ntype Config struct{}\n\nfunc main() {}\n",
		"cmd/a/engine.go":   "//go:build cgo\n\npackage main\n\n// Engine runs a job.\ntype Engine struct{}\n",
		"cmd/a/engine_n.go": "//go:build !cgo\n\npackage main\n\n// Engine runs a job.\ntype Engine struct{}\n",
	})
	result, err = descriptions.Read(root)
	require.NoError(t, err)
	require.Equal(t, map[string]string{
		"example.com/app/cmd/a.Config": "Config is a's.",
		"main.Config":                  "Config is a's.",
	}, result.Descriptions)
	require.Len(t, result.Duplicates, 1)
	require.Equal(t, "example.com/app/cmd/a.Engine", result.Duplicates[0].Key)
	require.Empty(t, result.MainPackages)

	// One import path reached from two module roots: every type there is
	// declared twice, so none is described, in either order.
	one := writeTree(t, map[string]string{
		"go.mod": mod,
		"p/p.go": "package p\n\n// P is p.\ntype P struct {\n\tX int // X is x.\n}\n",
	})
	two := writeTree(t, map[string]string{
		"go.mod": mod,
		"p/p.go": "package p\n\n// P is p.\ntype P struct {\n\tX int // X is x.\n}\n",
	})
	// Several duplicates come sorted by key, whatever the walk order.
	root = writeTree(t, map[string]string{
		"go.mod":       mod,
		"p/a_cgo.go":   "//go:build cgo\n\npackage p\n\ntype E struct{}\n\ntype D struct{}\n\ntype C struct{}\n\ntype B struct{}\n\ntype A struct{}\n",
		"p/b_nocgo.go": "//go:build !cgo\n\npackage p\n\ntype E struct{}\n\ntype D struct{}\n\ntype C struct{}\n\ntype B struct{}\n\ntype A struct{}\n",
	})
	result, err = descriptions.Read(root)
	require.NoError(t, err)
	var keys []string
	for _, duplicate := range result.Duplicates {
		keys = append(keys, strings.TrimPrefix(duplicate.Key, "example.com/app/p."))
	}
	require.Equal(t, []string{"A", "B", "C", "D", "E"}, keys)

	for _, dirs := range [][]string{{one, two}, {two, one}} {
		result, err = descriptions.Read(dirs...)
		require.NoError(t, err)
		require.Empty(t, result.Descriptions)
		require.Equal(t, []descriptions.Duplicate{{
			Key:    "example.com/app/p.P",
			First:  filepath.Join(dirs[0], "p", "p.go"),
			Second: filepath.Join(dirs[1], "p", "p.go"),
		}}, result.Duplicates)
		data, err := descriptions.Marshal(result.Descriptions)
		require.NoError(t, err)
		require.Equal(t, "{}\n", string(data))
	}
}

// TestReadOnce reads a directory and its subdirectory: each file is read
// once, so the declarations are counted once and the copy is made.
func TestReadOnce(t *testing.T) {
	root := writeTree(t, map[string]string{
		"go.mod":   mod,
		"p/a.go":   "package p\n\ntype A B\n",
		"p/b.go":   "package p\n\n// B is b.\ntype B struct {\n\tX int // X is x.\n}\n",
		"p/q/q.go": "package q\n\n// Q is q.\ntype Q struct{}\n",
	})
	want := map[string]string{
		"example.com/app/p.B":   "B is b.",
		"example.com/app/p.B.X": "X is x.",
		"example.com/app/p.A.X": "X is x.",
		"example.com/app/p/q.Q": "Q is q.",
	}
	require.Equal(t, want, read(t, root))
	require.Equal(t, want, read(t, root, filepath.Join(root, "p"), filepath.Join(root, "p")))
	require.Equal(t, want, read(t, filepath.Join(root, "p"), root))
}

// TestReadLinkedWorkingDirectory reads "." from a working directory reached
// through a symbolic link together with the resolved path of a subdirectory:
// each file is read once, so the output is that of "." alone.
func TestReadLinkedWorkingDirectory(t *testing.T) {
	root := writeTree(t, map[string]string{
		"real/go.mod": mod,
		"real/p/a.go": "package p\n\ntype A struct {\n\tX int // X is x.\n}\n\ntype D A\n",
	})
	link := filepath.Join(root, "link")
	err := os.Symlink(filepath.Join(root, "real"), link)
	if err != nil {
		t.Skipf("no symbolic link on this platform: %v", err)
	}
	t.Chdir(link)

	want := map[string]string{
		"example.com/app/p.A.X": "X is x.",
		"example.com/app/p.D.X": "X is x.",
	}
	require.Equal(t, want, read(t, "."))
	require.Equal(t, want, read(t, ".", filepath.Join(root, "real", "p")))
	require.Equal(t, want, read(t, filepath.Join(link, "p"), "."))
}

// TestReadAmbiguousKeys reads a field and a type that produce one key, as a
// directory name holds a dot. The key is left out every time, the others
// are kept.
func TestReadAmbiguousKeys(t *testing.T) {
	root := writeTree(t, map[string]string{
		"go.mod":   mod,
		"p/p.go":   "package p\n\n// T is t.\ntype T struct {\n\tX int // X is a field.\n}\n",
		"p.T/x.go": "package pt\n\n// X is a type.\ntype X struct {\n\tY int // Y is y.\n}\n",
	})
	want := map[string]string{
		"example.com/app/p.T":     "T is t.",
		"example.com/app/p.T.X.Y": "Y is y.",
	}
	for range 20 {
		require.Equal(t, want, read(t, root))
	}

	// A field without a comment claims its key too.
	root = writeTree(t, map[string]string{
		"go.mod":   mod,
		"p/p.go":   "package p\n\ntype T struct {\n\tX int\n}\n",
		"p.T/x.go": "package pt\n\n// X is a type.\ntype X struct{}\n",
	})
	require.Empty(t, read(t, root))

	// So does a field of a type declared twice.
	root = writeTree(t, map[string]string{
		"go.mod":       mod,
		"p/t_cgo.go":   "//go:build cgo\n\npackage p\n\ntype T struct {\n\tX int\n}\n",
		"p/t_nocgo.go": "//go:build !cgo\n\npackage p\n\ntype T struct {\n\tX int\n}\n",
		"p.T/x.go":     "package pt\n\n// X is a type.\ntype X struct{}\n",
	})
	result, err := descriptions.Read(root)
	require.NoError(t, err)
	require.Empty(t, result.Descriptions)
}

// TestReadNames checks where an entry goes: an embedded field under its
// type's name, a vendored package after the last vendor element, and a
// //go:build ignore line after the package clause, which is no constraint.
func TestReadNames(t *testing.T) {
	root := writeTree(t, map[string]string{
		"go.mod": mod,
		"p/p.go": "package p\n\nimport \"example.com/app/other\"\n\ntype R struct {\n" +
			"\t// T is embedded from another package.\n\tother.T\n" +
			"\t// Page is an embedded generic.\n\tPage[int]\n}\n\ntype Page[V any] struct{}\n",
		"p/late.go":                "package p\n\n//go:build ignore\n\n// Late is read.\ntype Late struct{}\n",
		"vendor/a/vendor/b/b.go":   "package b\n\n// B is vendored twice.\ntype B struct{}\n",
		"vendor/a/vendor/b/c/c.go": "package c\n\n// C is below it.\ntype C struct{}\n",
		"other/other.go":           "package other\n\ntype T struct{}\n",
	})
	require.Equal(t, map[string]string{
		"example.com/app/p.R.T":    "T is embedded from another package.",
		"example.com/app/p.R.Page": "Page is an embedded generic.",
		"example.com/app/p.Late":   "Late is read.",
	}, read(t, root))
	require.Equal(t, map[string]string{
		"b.B":   "B is vendored twice.",
		"b/c.C": "C is below it.",
	}, read(t, filepath.Join(root, "vendor", "a", "vendor", "b")))
}

// TestReadMainPackages writes main keys only when exactly one main package
// declares types.
func TestReadMainPackages(t *testing.T) {
	// Two main packages: a describes Config and Run, b declares the same
	// names bare. Nothing is written under main, and both are reported.
	root := writeTree(t, map[string]string{
		"go.mod": mod,
		"cmd/a/main.go": "package main\n\n// Config is the server configuration.\ntype Config struct {\n\t// Port is where the server listens.\n\tPort int\n}\n\n" +
			"type Job Run\n\ntype Run struct {\n\tX int // X is the run x of a.\n}\n\nfunc main() {}\n",
		"cmd/b/main.go": "package main\n\ntype Config struct {\n\tPort int\n}\n\ntype Job struct {\n\tX int\n}\n\nfunc main() {}\n",
	})
	a := filepath.Join(root, "cmd", "a")
	b := filepath.Join(root, "cmd", "b")
	want := map[string]string{
		"example.com/app/cmd/a.Config":      "Config is the server configuration.",
		"example.com/app/cmd/a.Config.Port": "Port is where the server listens.",
		"example.com/app/cmd/a.Job.X":       "X is the run x of a.",
		"example.com/app/cmd/a.Run.X":       "X is the run x of a.",
	}
	var first []byte
	for _, dirs := range [][]string{{a, b}, {b, a}, {root}, {filepath.Join(root, "cmd")}} {
		result, err := descriptions.Read(dirs...)
		require.NoError(t, err)
		require.Equal(t, want, result.Descriptions)
		require.Empty(t, result.Duplicates)
		require.Equal(t, []string{"example.com/app/cmd/a", "example.com/app/cmd/b"}, result.MainPackages)
		for key := range result.Descriptions {
			require.False(t, strings.HasPrefix(key, "main."), key)
		}
		data, err := descriptions.Marshal(result.Descriptions)
		require.NoError(t, err)
		if first == nil {
			first = data
		}
		require.Equal(t, string(first), string(data))
	}

	// Two main packages without a comment, beside a described library:
	// nothing to report.
	root = writeTree(t, map[string]string{
		"go.mod":        mod,
		"cmd/a/main.go": "package main\n\ntype Config struct{ Port int }\n\nfunc main() {}\n",
		"cmd/b/main.go": "package main\n\ntype Config struct{ Port int }\n\nfunc main() {}\n",
		"lib/lib.go":    "package lib\n\n// Lib is a library type.\ntype Lib struct{}\n",
	})
	require.Equal(t, map[string]string{"example.com/app/lib.Lib": "Lib is a library type."}, read(t, root))

	// An alias is a type declaration: b is a second main package.
	root = writeTree(t, map[string]string{
		"go.mod":        mod,
		"cmd/a/main.go": "package main\n\n// Config is a's.\ntype Config struct{}\n\nfunc main() {}\n",
		"cmd/b/main.go": "package main\n\ntype Config = int\n\nfunc main() {}\n",
	})
	result, err := descriptions.Read(root)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"example.com/app/cmd/a.Config": "Config is a's."}, result.Descriptions)
	require.Equal(t, []string{"example.com/app/cmd/a", "example.com/app/cmd/b"}, result.MainPackages)

	// A library directory with a package main script that is not ignored
	// is no main package: its types stay under its import path only.
	library := map[string]string{
		"go.mod":   mod,
		"p/p.go":   "package p\n\n// Config is p's.\ntype Config struct {\n\t// Port is p's port.\n\tPort int\n}\n",
		"p/gen.go": "//go:build generate\n\npackage main\n\n// Gen writes a file.\ntype Gen struct{}\n\nfunc main() {}\n",
	}
	wantLibrary := map[string]string{
		"example.com/app/p.Config":      "Config is p's.",
		"example.com/app/p.Config.Port": "Port is p's port.",
		"example.com/app/p.Gen":         "Gen writes a file.",
	}
	require.Equal(t, wantLibrary, read(t, writeTree(t, library)))

	// Beside a real command, the command is the one main package.
	library["cmd/a/main.go"] = "package main\n\n// Config is a's.\ntype Config struct{}\n\nfunc main() {}\n"
	wantLibrary["example.com/app/cmd/a.Config"] = "Config is a's."
	wantLibrary["main.Config"] = "Config is a's."
	require.Equal(t, wantLibrary, read(t, writeTree(t, library)))

	// A main package that declares no type does not count.
	root = writeTree(t, map[string]string{
		"go.mod":        mod,
		"cmd/a/main.go": "package main\n\n// Config is a's.\ntype Config struct{}\n\nfunc main() {}\n",
		"cmd/b/main.go": "package main\n\nfunc main() {}\n",
	})
	require.Equal(t, map[string]string{
		"example.com/app/cmd/a.Config": "Config is a's.",
		"main.Config":                  "Config is a's.",
	}, read(t, root))
}

// TestReadDefinedTypes copies the fields of a struct to a type defined over
// it only when every type of the chain is declared once in the package.
func TestReadDefinedTypes(t *testing.T) {
	tests := []struct {
		name       string
		files      map[string]string
		want       map[string]string
		duplicates []string
	}{
		{
			name: "a type of the chain declared twice",
			files: map[string]string{
				"p/b_cgo.go":   "//go:build cgo\n\npackage p\n\ntype B struct {\n\tX int // X is the cgo handle.\n}\n",
				"p/b_nocgo.go": "//go:build !cgo\n\npackage p\n\ntype B C\n\ntype C struct {\n\tX int // X is the pure Go handle.\n}\n",
				"p/a.go":       "package p\n\ntype A B\n",
			},
			want:       map[string]string{"example.com/app/p.C.X": "X is the pure Go handle."},
			duplicates: []string{"example.com/app/p.B"},
		},
		{
			name: "the defined type declared twice",
			files: map[string]string{
				"p/a_cgo.go":   "//go:build cgo\n\npackage p\n\ntype A B\n",
				"p/a_nocgo.go": "//go:build !cgo\n\npackage p\n\ntype A struct {\n\tX int // X of nocgo A.\n}\n",
				"p/b.go":       "package p\n\n// B is b.\ntype B struct {\n\tX int // X of B.\n}\n\ntype C A\n",
			},
			want: map[string]string{
				"example.com/app/p.B":   "B is b.",
				"example.com/app/p.B.X": "X of B.",
			},
			duplicates: []string{"example.com/app/p.A"},
		},
		{
			name: "the same definition in two files",
			files: map[string]string{
				"p/b_cgo.go":   "//go:build cgo\n\npackage p\n\ntype B C\n",
				"p/b_nocgo.go": "//go:build !cgo\n\npackage p\n\ntype B C\n",
				"p/a.go":       "package p\n\ntype A B\n\ntype C struct {\n\tX int // X is c.\n}\n",
			},
			want:       map[string]string{"example.com/app/p.C.X": "X is c."},
			duplicates: []string{"example.com/app/p.B"},
		},
		{
			name:  "a chain of four hops",
			files: map[string]string{"p/p.go": "package p\n\ntype A B\n\ntype B C\n\ntype C D\n\ntype D E\n\ntype E struct {\n\tX int // X is e.\n}\n"},
			want: map[string]string{
				"example.com/app/p.A.X": "X is e.",
				"example.com/app/p.B.X": "X is e.",
				"example.com/app/p.C.X": "X is e.",
				"example.com/app/p.D.X": "X is e.",
				"example.com/app/p.E.X": "X is e.",
			},
		},
		{
			name:  "two defined types over one struct",
			files: map[string]string{"p/p.go": "package p\n\ntype A S\n\ntype B S\n\ntype S struct {\n\tX int // X is s.\n}\n"},
			want: map[string]string{
				"example.com/app/p.A.X": "X is s.",
				"example.com/app/p.B.X": "X is s.",
				"example.com/app/p.S.X": "X is s.",
			},
		},
		{
			name:  "a cycle",
			files: map[string]string{"p/p.go": "package p\n\n// A is a.\ntype A B\n\ntype B A\n\ntype C C\n"},
			want:  map[string]string{"example.com/app/p.A": "A is a."},
		},
		{
			name: "a struct of the same name in another package",
			files: map[string]string{
				"p/p.go": "package p\n\ntype A B\n\ntype B struct {\n\tY int // Y is p's.\n}\n",
				"q/q.go": "package q\n\ntype B struct {\n\tX int // X is q's.\n}\n",
			},
			want: map[string]string{
				"example.com/app/p.A.Y": "Y is p's.",
				"example.com/app/p.B.Y": "Y is p's.",
				"example.com/app/q.B.X": "X is q's.",
			},
		},
		{
			name:  "a parenthesized generic struct",
			files: map[string]string{"p/p.go": "package p\n\ntype P (S[string])\n\ntype S[T any] struct {\n\tX T // X is s.\n}\n"},
			want: map[string]string{
				"example.com/app/p.P.X": "X is s.",
				"example.com/app/p.S.X": "X is s.",
			},
		},
		{
			name: "a struct and an alias of one name",
			files: map[string]string{
				"p/t_cgo.go":   "//go:build cgo\n\npackage p\n\n// T is t.\ntype T struct {\n\tX int // X is x.\n}\n",
				"p/t_nocgo.go": "//go:build !cgo\n\npackage p\n\ntype T = int\n",
			},
			want:       map[string]string{},
			duplicates: []string{"example.com/app/p.T"},
		},
		{
			name: "a struct of another package",
			files: map[string]string{
				"p/p.go":     "package p\n\ntype E struct {\n\tX int // X is e.\n}\n",
				"other/o.go": "package other\n\nimport \"example.com/app/p\"\n\ntype R p.E\n",
			},
			want: map[string]string{"example.com/app/p.E.X": "X is e."},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.files["go.mod"] = mod
			root := writeTree(t, tt.files)
			result, err := descriptions.Read(root)
			require.NoError(t, err)
			require.Equal(t, tt.want, result.Descriptions)
			var duplicates []string
			for _, duplicate := range result.Duplicates {
				duplicates = append(duplicates, duplicate.Key)
			}
			require.Equal(t, tt.duplicates, duplicates)
		})
	}
}

// TestReadModules builds its tree at run time, as the module zip leaves out
// a directory with a go.mod of its own and a vendor directory.
func TestReadModules(t *testing.T) {
	root := writeTree(t, map[string]string{
		"go.mod":                        mod,
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
	_, err = descriptions.Read(filepath.Join(root, "orphan"))
	require.Error(t, err)
	require.Contains(t, ez.ErrorMessage(err), filepath.Join(root, "orphan", "go.mod")+" declares no module path")
}

// TestReadSymlinkedFile reads a .go file that is a symbolic link to a
// regular file, as Go compiles it, and skips a broken link.
func TestReadSymlinkedFile(t *testing.T) {
	root := writeTree(t, map[string]string{
		"go.mod":            mod,
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

func TestReadErrors(t *testing.T) {
	_, err := descriptions.Read("testdata/missing")
	require.Error(t, err)
	require.Contains(t, ez.ErrorMessage(err), "cannot read testdata/missing")

	_, err = descriptions.Read(filepath.Join(tree, "models", "models.go"))
	require.Error(t, err)
	require.Contains(t, ez.ErrorMessage(err), "is not a directory")

	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(dir, "broken.go"), []byte("package broken\n\ntype {"), 0o644)
	require.NoError(t, err)
	_, err = descriptions.Read(dir)
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
