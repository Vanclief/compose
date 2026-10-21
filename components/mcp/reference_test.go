package mcp

import (
	"bytes"
	"encoding/json"
	"html"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vanclief/ez"
	nethtml "golang.org/x/net/html"
)

func testReferenceInfo() referenceInfo {
	return referenceInfo{
		Name:         "Front Desk",
		Version:      "1.0",
		Instructions: "Use search_functions first.\nThen describe_function <before> call_function.",
		Tools: []toolInfo{
			{
				Name:        "search_functions",
				Description: "List modules or find functions.",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"module":{"type":"string"},"query":{"type":"string"}}}`),
			},
			{
				Name:        "describe_function",
				Description: "Show how to call functions.",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"names":{"type":"array","items":{"type":"string"}}},"required":["names"]}`),
			},
		},
	}
}

func renderSmallReference(t *testing.T) (*Catalog, string) {
	t.Helper()
	c := smallCatalog(t)
	out, err := renderReference(c, testReferenceInfo())
	require.NoError(t, err)
	return c, string(out)
}

func TestRenderReferenceSmall(t *testing.T) {
	c, page := renderSmallReference(t)

	assert.Contains(t, page, "<h1>Front Desk<span class=\"version\">1.0</span></h1>")
	assert.Contains(t, page, "<p>MCP reference</p>")
	assert.Contains(t, page, html.EscapeString(testReferenceInfo().Instructions))

	for _, tool := range testReferenceInfo().Tools {
		assert.Contains(t, page, "<h3>"+tool.Name+"</h3>")
		assert.Contains(t, page, tool.Description)
	}
	assert.Contains(t, page, html.EscapeString("\"names\": {\n      \"type\": \"array\""), "schemas are indented")

	for _, m := range c.Modules {
		assert.Contains(t, page, `<a class="sub" href="#mod-`+m.Name+`">`+m.Name+` <span class="count">`)
		assert.Contains(t, page, `<section class="group" id="mod-`+m.Name+`">`)
		for _, f := range m.Functions {
			id := "fn-" + m.Name + "-" + strings.TrimPrefix(f.Name, m.Name+".")
			assert.Contains(t, page, `id="`+id+`"`)
			assert.Contains(t, page, "<h3>"+f.Name+" <span class=\"op\">")
			assert.Contains(t, page, "<pre>"+html.EscapeString(c.renderDescribe([]string{f.Name}))+"</pre>")
		}
	}
	assert.Contains(t, page, `id="fn-packages-get_package"`)
	assert.Contains(t, page, `<span class="op">composite</span><span class="badge ro">read-only</span><span class="badge composite">composite</span>`)
	assert.Contains(t, page, `<span class="op">DELETE /packages/{id}</span><span class="badge destructive">destructive</span>`)
	assert.Contains(t, page, `<span class="badge deprecated">deprecated</span>`)
	assert.Contains(t, page, `<p class="subtitle">Get one package.  -&gt; Package</p>`)

	for _, term := range c.Terms {
		assert.Contains(t, page, `id="term-`+slug(term.Name)+`"`)
		assert.Contains(t, page, "<pre>"+html.EscapeString(c.renderExplain([]string{term.Name}))+"</pre>")
	}
	assert.Contains(t, page, `<div class="block" id="term-front-desk"`)
	assert.Contains(t, page, `aliases: paquete, parcel`)

	assertSelfContained(t, page)
	assertWellFormed(t, page)
	assertUniqueIDs(t, page)

	path := filepath.Join("testdata", "reference_small.golden.html")
	if *update {
		err := os.WriteFile(path, []byte(page), 0o644)
		require.NoError(t, err)
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err, "run go test ./components/mcp/ -update to create %s", path)
	assert.Equal(t, string(want), page)
}

func TestRenderReferenceEscapes(t *testing.T) {
	c := smallCatalog(t)
	info := testReferenceInfo()
	info.Name = `<script>alert(1)</script>`
	info.Tools[0].Description = `<img src=x onerror=alert(1)>`
	out, err := renderReference(c, info)
	require.NoError(t, err)
	page := string(out)
	assert.NotContains(t, page, "<script>alert")
	assert.NotContains(t, page, "<img")
	assert.Contains(t, page, "&lt;script&gt;alert(1)&lt;/script&gt;")
}

func TestRenderReferenceInvalidSchema(t *testing.T) {
	c := smallCatalog(t)
	info := testReferenceInfo()
	info.Tools[1].InputSchema = json.RawMessage(`{"type":`)
	_, err := renderReference(c, info)
	require.Error(t, err)
	assert.Equal(t, ez.EINTERNAL, ez.ErrorCode(err))
	assert.Contains(t, ez.ErrorMessage(err), "describe_function")
}

func TestRenderReferenceEmma(t *testing.T) {
	c := emmaCatalog(t)
	info := referenceInfo{Name: "Large API", Version: "2.0", Instructions: "Read the catalog.", Tools: testReferenceInfo().Tools}

	start := time.Now()
	out, err := renderReference(c, info)
	elapsed := time.Since(start)
	require.NoError(t, err)
	page := string(out)

	assert.Less(t, elapsed, 2*time.Second)
	assert.Less(t, len(out), 6<<20)
	assert.Equal(t, 16, strings.Count(page, `<section class="group" id="mod-`))
	assert.Equal(t, 633, strings.Count(page, `<div class="block" id="term-`))
	assert.Equal(t, 296, strings.Count(page, `<div class="block" id="fn-`))
	assertSelfContained(t, page)
	assertUniqueIDs(t, page)
}

func TestSlug(t *testing.T) {
	assert.Equal(t, "front-desk", slug("Front desk"))
	assert.Equal(t, "units.status", slug("units.Status"))
	assert.Equal(t, "a-b_c", slug(" A / b_c! "))
	assert.Equal(t, "recepci-n", slug("recepción"))
}

// assertSelfContained fails when the page loads anything from elsewhere.
func assertSelfContained(t *testing.T, page string) {
	t.Helper()
	assert.NotContains(t, page, "<script src")
	assert.NotContains(t, page, "<link")
	assert.NotContains(t, page, "@import")
	assert.NotContains(t, page, "url(")
	assert.NotContains(t, page, "http://")
	assert.NotContains(t, page, "https://")
}

// voidElements have no end tag.
var voidElements = map[string]bool{
	"area": true, "base": true, "br": true, "col": true, "embed": true, "hr": true, "img": true,
	"input": true, "link": true, "meta": true, "source": true, "track": true, "wbr": true,
}

// assertWellFormed tokenizes the page and fails when an end tag does not
// close the innermost open element or an element is left open.
func assertWellFormed(t *testing.T, page string) {
	t.Helper()
	z := nethtml.NewTokenizer(strings.NewReader(page))
	var open []string
	for {
		kind := z.Next()
		if kind == nethtml.ErrorToken {
			require.ErrorIs(t, z.Err(), io.EOF)
			break
		}
		name, _ := z.TagName()
		tag := string(name)
		switch kind {
		case nethtml.StartTagToken:
			if !voidElements[tag] {
				open = append(open, tag)
			}
		case nethtml.EndTagToken:
			require.NotEmpty(t, open, "unexpected </%s>", tag)
			require.Equal(t, open[len(open)-1], tag, "mismatched end tag")
			open = open[:len(open)-1]
		}
	}
	assert.Empty(t, open, "unclosed elements")

	_, err := nethtml.Parse(bytes.NewReader([]byte(page)))
	require.NoError(t, err)
}

var idPattern = regexp.MustCompile(` id="([^"]*)"`)

// assertUniqueIDs fails when two elements share an id.
func assertUniqueIDs(t *testing.T, page string) {
	t.Helper()
	seen := map[string]bool{}
	for _, match := range idPattern.FindAllStringSubmatch(page, -1) {
		assert.False(t, seen[match[1]], "duplicate id %q", match[1])
		seen[match[1]] = true
	}
}
