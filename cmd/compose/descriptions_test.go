package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	descriptionsTree     = "../../components/rest/api/descriptions/testdata/tree"
	descriptionsConflict = "../../components/rest/api/descriptions/testdata/conflict"
	// runKey is a key of descriptionsTree.
	runKey = `"github.com/vanclief/compose/components/rest/api/descriptions/testdata/tree/models.Run": "Run is one execution of a job."`
)

func TestDescriptionsStdout(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"descriptions", descriptionsTree}, &stdout, &stderr)
	require.Equal(t, 0, code, stderr.String())
	require.Empty(t, stderr.String())
	require.Contains(t, stdout.String(), runKey)
	require.Equal(t, byte('{'), stdout.Bytes()[0])
	require.Equal(t, "}\n", stdout.String()[stdout.Len()-2:])

	// The same source gives the same bytes.
	var again bytes.Buffer
	code = run([]string{"descriptions", descriptionsTree}, &again, &stderr)
	require.Equal(t, 0, code, stderr.String())
	require.Equal(t, stdout.String(), again.String())
}

func TestDescriptionsOut(t *testing.T) {
	out := filepath.Join(t.TempDir(), "descriptions.json")
	var stdout, stderr bytes.Buffer
	code := run([]string{"descriptions", "-out", out, descriptionsTree, filepath.Join(descriptionsTree, "testdata")}, &stdout, &stderr)
	require.Equal(t, 0, code, stderr.String())
	require.Empty(t, stdout.String())

	data, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Contains(t, string(data), runKey)
	require.Contains(t, string(data), "Data is in testdata.")
}

// TestDescriptionsDefaultDir runs the command without a directory, which
// reads the working directory.
func TestDescriptionsDefaultDir(t *testing.T) {
	var want, stderr bytes.Buffer
	code := run([]string{"descriptions", descriptionsTree}, &want, &stderr)
	require.Equal(t, 0, code, stderr.String())

	t.Chdir(descriptionsTree)
	var stdout bytes.Buffer
	code = run([]string{"descriptions"}, &stdout, &stderr)
	require.Equal(t, 0, code, stderr.String())
	require.Equal(t, want.String(), stdout.String())
}

// TestDescriptionsConflict checks that a key two files describe differently
// is left out with one warning per key, and the command still succeeds.
func TestDescriptionsConflict(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"descriptions", descriptionsConflict}, &stdout, &stderr)
	require.Equal(t, 0, code, stderr.String())
	require.Equal(t, ""+
		"compose descriptions: github.com/vanclief/compose/components/rest/api/descriptions/testdata/conflict/engine.Engine is described differently in "+
		filepath.Join(descriptionsConflict, "engine", "engine_cgo.go")+" and "+filepath.Join(descriptionsConflict, "engine", "engine_nocgo.go")+
		": left out, make the comments match to describe it\n", stderr.String())
	require.NotContains(t, stdout.String(), `engine.Engine"`)
	require.Contains(t, stdout.String(), `engine.Engine.Name": "Name names the engine."`)
}

// TestDescriptionsMainPackages checks that two main packages that declare
// types give one warning, no main key, and a command that still succeeds.
func TestDescriptionsMainPackages(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"go.mod":        "module example.com/app\n\ngo 1.26\n",
		"cmd/a/main.go": "package main\n\n// Config is a's.\ntype Config struct{}\n\nfunc main() {}\n",
		"cmd/b/main.go": "package main\n\ntype Config struct{}\n\nfunc main() {}\n",
	}
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		err := os.MkdirAll(filepath.Dir(path), 0o755)
		require.NoError(t, err)
		err = os.WriteFile(path, []byte(content), 0o644)
		require.NoError(t, err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"descriptions", root}, &stdout, &stderr)
	require.Equal(t, 0, code, stderr.String())
	require.Equal(t, "compose descriptions: 2 main packages declare types (example.com/app/cmd/a, example.com/app/cmd/b), so no type is described under main: "+
		"run the command on the directories of one command at a time to describe the types of its package main\n", stderr.String())
	require.Equal(t, "{\n  \"example.com/app/cmd/a.Config\": \"Config is a's.\"\n}\n", stdout.String())

	// One command at a time describes its types under main too.
	stdout.Reset()
	stderr.Reset()
	code = run([]string{"descriptions", filepath.Join(root, "cmd", "a")}, &stdout, &stderr)
	require.Equal(t, 0, code, stderr.String())
	require.Empty(t, stderr.String())
	require.Contains(t, stdout.String(), `"main.Config": "Config is a's."`)
}

func TestDescriptionsErrors(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		code   int
		stderr string
	}{
		{name: "help", args: []string{"-h"}, code: 0, stderr: "Usage:\n  compose descriptions"},
		{name: "unknown flag", args: []string{"-nope"}, code: 2, stderr: "flag provided but not defined"},
		{name: "missing directory", args: []string{"missing"}, code: 1, stderr: "compose: cannot read missing: "},
		{name: "a file", args: []string{"main.go"}, code: 1, stderr: "compose: main.go is not a directory"},
		{name: "flag after a directory", args: []string{".", "-out", "x.json"}, code: 2, stderr: `compose: unexpected argument "-out": flags go before the directories`},
		{name: "unwritable out", args: []string{"-out", filepath.Join("missing", "x.json"), descriptionsTree}, code: 1, stderr: "compose: cannot write " + filepath.Join("missing", "x.json")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(append([]string{"descriptions"}, tt.args...), &stdout, &stderr)
			require.Equal(t, tt.code, code)
			require.Contains(t, stderr.String(), tt.stderr)
			require.Empty(t, stdout.String())
		})
	}
}
