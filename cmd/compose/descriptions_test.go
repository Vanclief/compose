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
		{name: "conflict", args: []string{descriptionsConflict}, code: 1, stderr: "compose: main.Config is described differently in "},
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
