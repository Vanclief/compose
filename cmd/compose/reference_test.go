package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	headSpec = "../../components/rest/reference/testdata/diff-head.yaml"
	baseSpec = "../../components/rest/reference/testdata/diff-base.yaml"
	// changesSection is the id of the "What changed" section, rendered only
	// in diff mode.
	changesSection = `id="section/changes"`
)

func TestReferenceOut(t *testing.T) {
	dir := t.TempDir()

	plain := filepath.Join(dir, "plain.html")
	var stdout, stderr bytes.Buffer
	code := run([]string{"reference", "-head", headSpec, "-out", plain}, &stdout, &stderr)
	require.Equal(t, 0, code, stderr.String())
	require.Empty(t, stdout.String())

	html, err := os.ReadFile(plain)
	require.NoError(t, err)
	require.Contains(t, string(html), "<html")
	require.NotContains(t, string(html), changesSection)

	diff := filepath.Join(dir, "diff.html")
	code = run([]string{"reference", "-head", headSpec, "-base", baseSpec, "-title", "Diff test", "-out", diff}, &stdout, &stderr)
	require.Equal(t, 0, code, stderr.String())

	html, err = os.ReadFile(diff)
	require.NoError(t, err)
	require.Contains(t, string(html), "<html")
	require.Contains(t, string(html), changesSection)
	require.Contains(t, string(html), "What changed")
	require.Contains(t, string(html), "<title>Diff test</title>")
}

func TestReferenceStdout(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"reference", "-head", headSpec}, &stdout, &stderr)
	require.Equal(t, 0, code, stderr.String())
	require.Contains(t, stdout.String(), "<html")
}

func TestReferenceErrors(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		code   int
		stderr string
	}{
		{name: "missing head", args: nil, code: 2, stderr: "compose: -head is required"},
		{name: "out and serve", args: []string{"-head", headSpec, "-out", "x.html", "-serve", "127.0.0.1:0"}, code: 2, stderr: "compose: -out and -serve cannot be used together"},
		{name: "unknown flag", args: []string{"-nope"}, code: 2, stderr: "flag provided but not defined"},
		{name: "extra argument", args: []string{"-head", headSpec, "extra"}, code: 2, stderr: `compose: unexpected argument "extra"`},
		{name: "missing file", args: []string{"-head", "missing.yaml"}, code: 1, stderr: "missing.yaml: no such file"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(append([]string{"reference"}, tt.args...), &stdout, &stderr)
			require.Equal(t, tt.code, code)
			require.Contains(t, stderr.String(), tt.stderr)
			require.Empty(t, stdout.String())
			if tt.code == 2 {
				require.Contains(t, stderr.String(), "Usage:")
			}
		})
	}
}

func TestReferenceHandler(t *testing.T) {
	var stderr bytes.Buffer
	server := httptest.NewServer(referenceHandler(headSpec, baseSpec, "", "", &stderr))
	defer server.Close()

	res, err := http.Get(server.URL)
	require.NoError(t, err)
	body, err := io.ReadAll(res.Body)
	res.Body.Close()
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Equal(t, "text/html; charset=utf-8", res.Header.Get("Content-Type"))
	require.Equal(t, "no-store", res.Header.Get("Cache-Control"))
	require.Contains(t, string(body), "<html")
	require.Contains(t, string(body), changesSection)
	require.Empty(t, stderr.String())

	broken := httptest.NewServer(referenceHandler("missing.yaml", "", "", "", &stderr))
	defer broken.Close()

	res, err = http.Get(broken.URL)
	require.NoError(t, err)
	body, err = io.ReadAll(res.Body)
	res.Body.Close()
	require.NoError(t, err)
	require.Equal(t, http.StatusInternalServerError, res.StatusCode)
	require.Equal(t, "text/plain; charset=utf-8", res.Header.Get("Content-Type"))
	require.Contains(t, string(body), "missing.yaml")
	require.Contains(t, stderr.String(), "missing.yaml")
}

func TestReferenceHandlerRereads(t *testing.T) {
	head := filepath.Join(t.TempDir(), "head.yaml")
	var stderr bytes.Buffer
	server := httptest.NewServer(referenceHandler(head, "", "", "", &stderr))
	defer server.Close()

	res, err := http.Get(server.URL)
	require.NoError(t, err)
	res.Body.Close()
	require.Equal(t, http.StatusInternalServerError, res.StatusCode)

	spec, err := os.ReadFile(headSpec)
	require.NoError(t, err)
	err = os.WriteFile(head, spec, 0o644)
	require.NoError(t, err)

	res, err = http.Get(server.URL)
	require.NoError(t, err)
	res.Body.Close()
	require.Equal(t, http.StatusOK, res.StatusCode)
}
