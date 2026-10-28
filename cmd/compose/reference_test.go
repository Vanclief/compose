package main

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
		{name: "missing file", args: []string{"-head", "missing.yaml"}, code: 1, stderr: "compose: cannot read missing.yaml: open missing.yaml: no such file"},
		{name: "bad address", args: []string{"-head", headSpec, "-serve", "bad::addr"}, code: 1, stderr: "compose: cannot listen on bad::addr: "},
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

func TestReferenceMalformedSpec(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	err := os.WriteFile(bad, []byte("openapi: [\n"), 0o644)
	require.NoError(t, err)

	var stdout, stderr bytes.Buffer
	code := run([]string{"reference", "-head", headSpec, "-base", bad}, &stdout, &stderr)
	require.Equal(t, 1, code)
	require.Empty(t, stdout.String())
	require.Contains(t, stderr.String(), "compose: "+headSpec+": reference: base document: ")
	require.Contains(t, stderr.String(), "yaml: ")
	require.NotContains(t, stderr.String(), "main.render")

	stderr.Reset()
	code = run([]string{"reference", "-head", bad}, &stdout, &stderr)
	require.Equal(t, 1, code)
	require.Contains(t, stderr.String(), "compose: "+bad+": ")
	require.Contains(t, stderr.String(), "yaml: ")
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
	require.Contains(t, string(body), "cannot read missing.yaml")
	require.Contains(t, stderr.String(), "cannot read missing.yaml")
	require.NotContains(t, string(body), "main.render")

	stderr.Reset()
	res, err = http.Get(server.URL + "/favicon.ico")
	require.NoError(t, err)
	res.Body.Close()
	require.Equal(t, http.StatusNotFound, res.StatusCode)
	require.Empty(t, stderr.String())

	res, err = http.Post(server.URL, "text/plain", nil)
	require.NoError(t, err)
	res.Body.Close()
	require.Equal(t, http.StatusMethodNotAllowed, res.StatusCode)
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

func TestServe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	reader, writer := io.Pipe()
	done := make(chan int, 1)
	go func() {
		code := serve(ctx, "127.0.0.1:0", referenceHandler(headSpec, baseSpec, "", "", writer), headSpec, baseSpec, writer)
		writer.Close()
		done <- code
	}()

	line, err := bufio.NewReader(reader).ReadString('\n')
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(line, "serving http://127.0.0.1:"), line)
	require.True(t, strings.HasSuffix(line, " (head: "+headSpec+", base: "+baseSpec+")\n"), line)

	url := strings.Fields(line)[1]
	res, err := http.Get(url)
	require.NoError(t, err)
	body, err := io.ReadAll(res.Body)
	res.Body.Close()
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Contains(t, string(body), changesSection)

	cancel()
	rest, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.Empty(t, string(rest))
	require.Equal(t, 0, <-done)

	_, err = http.Get(url)
	require.Error(t, err)
}

func TestServeURL(t *testing.T) {
	require.Equal(t, "http://localhost:8090", serveURL(&net.TCPAddr{IP: net.IPv6unspecified, Port: 8090}))
	require.Equal(t, "http://localhost:8090", serveURL(&net.TCPAddr{IP: net.IPv4zero, Port: 8090}))
	require.Equal(t, "http://127.0.0.1:8090", serveURL(&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8090}))
	require.Equal(t, "http://[::1]:8090", serveURL(&net.TCPAddr{IP: net.IPv6loopback, Port: 8090}))
}
