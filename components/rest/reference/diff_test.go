package reference

import (
	"encoding/json"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/vanclief/ez"
)

func loadDiffFixtures(t *testing.T) *Report {
	t.Helper()
	return Diff(loadEmma(t, "diff-base.yaml"), loadEmma(t, "diff-head.yaml"))
}

// change returns the change at path whose detail starts with detail.
func change(t *testing.T, changes []Change, path, detail string) Change {
	t.Helper()
	for _, c := range changes {
		if c.Path == path && strings.HasPrefix(c.Detail, detail) {
			return c
		}
	}
	var have []string
	for _, c := range changes {
		have = append(have, c.Path+": "+c.Detail)
	}
	require.Failf(t, "change not found", "%s: %s\nhave:\n%s", path, detail, strings.Join(have, "\n"))
	return Change{}
}

func TestDiffOperations(t *testing.T) {
	r := loadDiffFixtures(t)

	removed := r.Operation("DELETE", "/issues/{id}")
	require.NotNil(t, removed)
	require.Equal(t, Removed, removed.Kind)
	require.True(t, removed.Breaking, "a removed operation is breaking")
	require.Equal(t, []string{"Issues"}, removed.Tags)

	added := r.Operation("post", "/labels")
	require.NotNil(t, added)
	require.Equal(t, Added, added.Kind)
	require.False(t, added.Breaking)

	require.Nil(t, r.Operation("GET", "/health"), "an unchanged operation has no result")

	var keys []string
	for _, od := range r.Operations {
		keys = append(keys, od.Key())
	}
	require.Equal(t, []string{
		"GET /issues", "POST /issues", "GET /issues/{id}", "DELETE /issues/{id}",
		"GET /issues/{id}/stats", "GET /users/me", "POST /labels",
	}, keys, "head order, removed operations after their base predecessor")
}

func TestDiffParameters(t *testing.T) {
	r := loadDiffFixtures(t)
	list := r.Operation("GET", "/issues")
	require.NotNil(t, list)
	require.True(t, list.Breaking)

	c := change(t, list.Changes, "GET /issues › query › project", "required parameter added")
	require.Equal(t, Added, c.Kind)
	require.True(t, c.Breaking, "a new required parameter is breaking")

	c = change(t, list.Changes, "GET /issues › query › limit", "description changed")
	require.False(t, c.Breaking)

	c = change(t, list.Changes, "GET /issues › query › sort", "parameter removed")
	require.Equal(t, Removed, c.Kind)
	require.False(t, c.Breaking)
}

func TestDiffRequestRules(t *testing.T) {
	r := loadDiffFixtures(t)

	create := r.Model("CreateIssue")
	require.NotNil(t, create)
	require.True(t, create.Breaking)

	c := change(t, create.Changes, "CreateIssue › project_id", "required property added")
	require.True(t, c.Breaking, "a new required request property is breaking")
	c = change(t, create.Changes, "CreateIssue › summary", "made required")
	require.True(t, c.Breaking, "making a request property required is breaking")
	c = change(t, create.Changes, "CreateIssue › status", "enum values removed: \"archived\"")
	require.True(t, c.Breaking, "narrowing a request enum is breaking")

	op := r.Operation("POST", "/issues")
	require.NotNil(t, op)
	c = change(t, op.Changes, "POST /issues › request body", "model CreateIssue changed")
	require.True(t, c.Breaking)
	require.True(t, op.Breaking)
}

func TestDiffResponseRules(t *testing.T) {
	r := loadDiffFixtures(t)

	issue := r.Model("Issue")
	require.NotNil(t, issue)
	require.Equal(t, Changed, issue.Kind)

	breaking := map[string]string{
		"Issue › title":       "property removed",
		"Issue › legacy_code": "property removed",
		"Issue › priority":    "type changed from integer to string",
		"Issue › assignee_id": "made nullable",
		"Issue › reporter":    "model User changed",
	}
	for path, detail := range breaking {
		c := change(t, issue.Changes, path, detail)
		require.True(t, c.Breaking, path)
	}

	nonBreaking := map[string]string{
		"Issue › name":        "required property added",
		"Issue › labels":      "property added",
		"Issue › state":       "enum values added: \"triaged\"",
		"Issue › description": "description changed",
	}
	for path, detail := range nonBreaking {
		c := change(t, issue.Changes, path, detail)
		require.False(t, c.Breaking, path)
	}

	get := r.Operation("GET", "/issues/{id}")
	require.NotNil(t, get)
	require.False(t, change(t, get.Changes, "GET /issues/{id}", "operation deprecated").Breaking)
	require.True(t, change(t, get.Changes, "GET /issues/{id} › response 200", "model Issue changed").Breaking)

	stats := r.Operation("GET", "/issues/{id}/stats")
	require.NotNil(t, stats)
	c := change(t, stats.Changes, "GET /issues/{id}/stats › response 200 › counts › closed", "property added")
	require.Equal(t, Added, c.Kind)
	require.False(t, stats.Breaking)

	me := r.Operation("GET", "/users/me")
	require.NotNil(t, me)
	require.True(t, change(t, me.Changes, "GET /users/me › response 200", "model User changed").Breaking)
	require.True(t, change(t, r.Model("User").Changes, "User › email", "property removed").Breaking)

	require.Nil(t, r.Model("Node"), "a recursive model that did not change")
}

func TestDiffMarkdown(t *testing.T) {
	md := loadDiffFixtures(t).Markdown()

	breaking := strings.Index(md, "## Breaking changes")
	other := strings.Index(md, "## Other changes")
	require.True(t, breaking > 0 && other > breaking, md)
	require.Contains(t, md[breaking:other], "### Issues\n")
	require.Contains(t, md[breaking:other], "- `DELETE /issues/{id}`: operation removed\n")
	require.Contains(t, md[breaking:other], "- `GET /issues` › query › project: required parameter added\n")
	require.Contains(t, md[breaking:other], "- `Issue` › title: property removed\n")
	require.Contains(t, md[breaking:other], "- `GET /users/me` › response 200: model `User` changed\n")
	require.Contains(t, md[other:], "### Labels\n\n- `POST /labels`: operation added\n")
	require.Contains(t, md[other:], "### Models\n")
	require.NotContains(t, md[other:], "operation removed")

	require.Equal(t, "# API changes\n\nNo changes.\n", Diff(loadEmma(t, "diff-head.yaml"), loadEmma(t, "diff-head.yaml")).Markdown())
}

func TestDiffEmmaWithItself(t *testing.T) {
	r := Diff(loadEmma(t, "emma.yaml"), loadEmma(t, "emma.yaml"))
	require.True(t, r.Empty(), r.Markdown())
	require.Empty(t, r.Operations)
	require.Empty(t, r.Models)

	r = Diff(loadEmma(t, "emma.yaml"), loadEmma(t, "emma.json"))
	require.True(t, r.Empty(), "the YAML and JSON forms of emma are the same API: "+r.Markdown())
}

var componentRefPattern = regexp.MustCompile(`#/components/schemas/([^"]+)"`)

// refsIn lists the component schemas v mentions, found in its JSON form so
// the test does not share code with Diff.
func refsIn(t *testing.T, v any) []string {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	var out []string
	for _, m := range componentRefPattern.FindAllStringSubmatch(string(data), -1) {
		out = append(out, m[1])
	}
	return out
}

func TestDiffEmmaRemovedPropertyMarksEveryUser(t *testing.T) {
	base := loadEmma(t, "emma.yaml")
	head := loadEmma(t, "emma.yaml")

	model := head.Components.Schemas.Map["StandardError"]
	require.NotNil(t, model)
	delete(model.Properties.Map, "request_id")
	model.Properties.Keys = []string{"code", "message"}
	model.Required = []string{"code", "message"}

	r := Diff(base, head)

	// Every model that reaches StandardError, then every operation that
	// reaches one of those.
	reaches := map[string]bool{"StandardError": true}
	for changed := true; changed; {
		changed = false
		for _, name := range head.Components.Schemas.Keys {
			if reaches[name] {
				continue
			}
			for _, ref := range refsIn(t, head.Components.Schemas.Map[name]) {
				if reaches[ref] {
					reaches[name] = true
					changed = true
					break
				}
			}
		}
	}

	var want, got []string
	for _, ref := range head.Operations() {
		for _, name := range refsIn(t, ref.Operation) {
			if reaches[name] {
				want = append(want, ref.Method+" "+ref.Path)
				break
			}
		}
	}
	for _, od := range r.Operations {
		require.Equal(t, Changed, od.Kind, od.Key())
		require.True(t, od.Breaking, "a removed response property is breaking: %s", od.Key())
		got = append(got, od.Key())
	}
	sort.Strings(want)
	sort.Strings(got)
	require.Greater(t, len(want), 100)
	require.Equal(t, want, got)

	se := r.Model("StandardError")
	require.NotNil(t, se)
	require.Equal(t, []Change{{Kind: Removed, Path: "StandardError › request_id", Detail: "property removed", Breaking: true}}, se.Changes)
	require.NotNil(t, r.Model("ErrorResponse"))
	require.Equal(t, Changed, r.Model("ErrorResponse").Kind)
}

func TestRenderDiff(t *testing.T) {
	base, err := os.ReadFile("testdata/diff-base.yaml")
	require.NoError(t, err)
	head := loadEmma(t, "diff-head.yaml")

	out, err := Render(head, Options{Base: base, AssetsURL: "/assets"})
	require.NoError(t, err)
	html := string(out)
	requireBalanced(t, html)

	require.Contains(t, html, `<body data-server="" data-mode="docs" class="diff">`)
	require.Contains(t, html, `data-diff-toggle`)
	require.NotContains(t, html, `data-diff-only`)
	require.Contains(t, html, `href="#section/changes"`)

	changes := section(t, html, `id="section/changes"`)
	require.Contains(t, changes, "What changed")
	require.Contains(t, changes, `href="#operation/issues-delete"`)
	require.Contains(t, changes, `<code class="diff-where">query › project</code>`)

	// Operations: badges, the removed one stays in the page.
	require.Contains(t, html, `<article class="op diff-removed" id="operation/issues-delete"`)
	require.Contains(t, html, `<article class="op diff-added" id="operation/labels-create"`)
	require.Contains(t, html, `<article class="op diff-changed" id="operation/issues-list"`)
	require.Contains(t, html, `<article class="op" id="operation/health"`)
	require.Contains(t, html, `<span class="diff-badge diff-removed">REMOVED</span><span class="diff-badge diff-breaking">BREAKING</span>`)
	require.Contains(t, html, `class="nav-op diff-op diff-removed" href="#operation/issues-delete"`)

	// Model card rows: removed rows after their predecessor, old and new
	// version of a changed row, added rows, enum chips, changed models.
	issue := section(t, html, `id="model/Issue"`)
	require.Regexp(t, `<div class="row diff-del"><div class="row-head"><span class="pn">title</span>`, issue)
	require.Regexp(t, `(?s)<span class="pn">id</span>.*<div class="row diff-del"><div class="row-head"><span class="pn">title</span>.*<span class="pn">name</span>`, issue)
	require.Regexp(t, `(?s)<div class="row diff-del"><div class="row-head"><span class="pn">priority</span><span class="ty">integer</span>.*?<div class="row diff-add"><div class="row-head"><span class="pn">priority</span><span class="ty">string</span>`, issue)
	require.Regexp(t, `(?s)<span class="pn">priority</span><span class="ty">string</span>.*<div class="row diff-del"><div class="row-head"><span class="pn">legacy_code</span>`, issue)
	require.Contains(t, issue, `<div class="row diff-add"><div class="row-head"><span class="pn">labels</span>`)
	require.Contains(t, issue, `<code class="chip diff-add">triaged</code>`)
	require.Contains(t, issue, `<a class="mk diff-inside" href="#model/User"`)
	require.Contains(t, issue, `<span class="diff-badge diff-changed">CHANGED</span><span class="diff-badge diff-breaking">BREAKING</span>`)

	create := section(t, html, `id="model/CreateIssue"`)
	require.Contains(t, create, `<code class="chip diff-del">archived</code>`)

	// Parameters: the removed one is written into its group, the new one is
	// marked.
	list := section(t, html, `id="operation/issues-list"`)
	require.Contains(t, list, `<div class="row diff-del"><div class="row-head"><span class="pn">sort</span>`)
	require.Contains(t, list, `<div class="row diff-add"><div class="row-head"><span class="pn">project</span>`)

	// Responses show in the Response card, so a change inside an inline
	// response marks the operation.
	require.Contains(t, html, `<article class="op diff-changed" id="operation/issues-stats"`)
}

func TestRenderDiffBadBase(t *testing.T) {
	head := loadEmma(t, "diff-head.yaml")
	_, err := Render(head, Options{Base: []byte("swagger: '2.0'\n")})
	require.Error(t, err)
	require.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	require.Contains(t, ez.ErrorMessage(err), "base document")

	_, err = New([]byte("openapi: 3.0.3\ninfo: {title: x}\npaths: {}\n"), Options{Base: []byte("{")})
	require.Error(t, err)
	require.Equal(t, ez.EINVALID, ez.ErrorCode(err))
}

// TestRenderWithoutBase checks that a page without Base carries no diff
// markup at all, so the plain page is byte for byte what it was.
func TestRenderWithoutBase(t *testing.T) {
	doc := loadEmma(t, "emma.yaml")
	out, err := Render(doc, Options{AssetsURL: "/assets"})
	require.NoError(t, err)
	html := string(out)
	for _, marker := range []string{"diff-", "data-diff", "section/changes", `class="diff"`, "changed inside"} {
		require.NotContains(t, html, marker)
	}

	again, err := Render(doc, Options{AssetsURL: "/assets", Base: nil})
	require.NoError(t, err)
	require.Equal(t, out, again)
}

// TestRenderModelDiffs checks the data of the side-by-side model diffs: a
// changed model's base sample and changes by path below the model, a model
// that changed inside another, an added model without a base sample, and a
// removed model, which gets a card built from the base.
func TestRenderModelDiffs(t *testing.T) {
	base := `{
  "openapi": "3.1.0",
  "info": {"title": "t", "version": "1.0"},
  "paths": {},
  "components": {"schemas": {
    "Pet": {"type": "object", "required": ["id"], "properties": {
      "id": {"type": "string"},
      "legacy": {"type": "string"},
      "owner": {"$ref": "#/components/schemas/Owner"}
    }},
    "Owner": {"type": "object", "properties": {"name": {"type": "string"}}},
    "Legacy": {"type": "object", "description": "Gone.", "properties": {"code": {"type": "string"}}}
  }}
}`
	head := `{
  "openapi": "3.1.0",
  "info": {"title": "t", "version": "1.1"},
  "paths": {},
  "components": {"schemas": {
    "Pet": {"type": "object", "required": ["id"], "properties": {
      "id": {"type": "integer"},
      "owner": {"$ref": "#/components/schemas/Owner"},
      "tags": {"type": "array", "items": {"type": "string"}}
    }},
    "Owner": {"type": "object", "properties": {"name": {"type": "string"}, "email": {"type": "string"}}},
    "Tag": {"type": "object", "properties": {"name": {"type": "string"}}}
  }}
}`
	doc, err := Parse([]byte(head))
	require.NoError(t, err)
	out, err := Render(doc, Options{Base: []byte(base)})
	require.NoError(t, err)
	html := string(out)
	requireBalanced(t, html)

	start := strings.Index(html, `<script type="application/json" id="model-diffs">`)
	require.Positive(t, start)
	raw := html[start+len(`<script type="application/json" id="model-diffs">`):]
	raw = raw[:strings.Index(raw, "</script>")]
	var diffs modelDiffs
	require.NoError(t, json.Unmarshal([]byte(raw), &diffs))
	require.Equal(t, "1.0", diffs.Base)
	require.Equal(t, "1.1", diffs.Head)

	pet := diffs.Models["model/Pet"]
	require.NotNil(t, pet)
	require.Equal(t, "changed", pet.Kind)
	require.NotNil(t, pet.Base)
	require.Contains(t, string(pet.Base.Value), `"legacy"`)
	kinds := map[string]string{}
	for _, c := range pet.Changes {
		kinds[c.Path] = c.Kind
	}
	require.Equal(t, "removed", kinds["legacy"])
	require.Equal(t, "added", kinds["tags"])
	require.Equal(t, "changed", kinds["id"])
	require.Equal(t, "inside", kinds["owner"])
	for _, c := range pet.Changes {
		if c.Kind == "inside" {
			require.Equal(t, "model/Owner", c.Model)
			require.Equal(t, "Owner", c.Detail)
		}
	}

	tag := diffs.Models["model/Tag"]
	require.NotNil(t, tag)
	require.Equal(t, "added", tag.Kind)
	require.Nil(t, tag.Base)

	legacy := diffs.Models["model/Legacy"]
	require.NotNil(t, legacy)
	require.Equal(t, "removed", legacy.Kind)
	require.NotNil(t, legacy.Base)

	// The removed model has a card and a sidebar entry, from the base.
	card := section(t, html, `id="model/Legacy"`)
	require.Contains(t, html, `<article class="model diff-removed" id="model/Legacy"`)
	require.Contains(t, card, "Gone.")
	require.Contains(t, card, `data-model-oi`)
	require.Contains(t, html, `class="nav-model diff-op diff-removed" href="#model/Legacy"`)

	// Without a base the page has no model diffs.
	plain, err := Render(doc, Options{})
	require.NoError(t, err)
	require.NotContains(t, string(plain), `id="model-diffs"`)
}

// TestRenderSampleDiffs covers the response sample diffs: a changed status
// with its own and its models' changes, an added and a removed status, the
// status tab letters and the flow ports' marks.
func TestRenderSampleDiffs(t *testing.T) {
	base := `{
  "openapi": "3.1.0",
  "info": {"title": "t", "version": "1.0"},
  "paths": {"/pets": {"get": {
    "operationId": "pets.list",
    "parameters": [
      {"name": "limit", "in": "query", "schema": {"type": "integer"}},
      {"name": "X-Old", "in": "header", "required": true, "schema": {"type": "string"}}
    ],
    "responses": {
      "200": {"description": "ok", "content": {"application/json": {"schema": {"type": "object", "properties": {
        "pets": {"type": "array", "items": {"$ref": "#/components/schemas/Pet"}},
        "total": {"type": "string"}
      }}}}},
      "404": {"description": "missing", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Problem"}}}}
    }
  }}},
  "components": {"schemas": {
    "Pet": {"type": "object", "properties": {"id": {"type": "string"}, "legacy": {"type": "string"}}},
    "Problem": {"type": "object", "properties": {"message": {"type": "string"}}}
  }}
}`
	head := `{
  "openapi": "3.1.0",
  "info": {"title": "t", "version": "1.1"},
  "paths": {"/pets": {"get": {
    "operationId": "pets.list",
    "parameters": [
      {"name": "limit", "in": "query", "schema": {"type": "integer"}},
      {"name": "X-New", "in": "header", "required": true, "schema": {"type": "string"}}
    ],
    "responses": {
      "200": {"description": "ok", "content": {"application/json": {"schema": {"type": "object", "properties": {
        "pets": {"type": "array", "items": {"$ref": "#/components/schemas/Pet"}},
        "total": {"type": "integer"},
        "next": {"type": "string"}
      }}}}},
      "202": {"description": "later", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Problem"}}}}
    }
  }}},
  "components": {"schemas": {
    "Pet": {"type": "object", "properties": {"id": {"type": "string"}, "alias": {"type": "string"}}},
    "Problem": {"type": "object", "properties": {"message": {"type": "string"}}}
  }}
}`
	doc, err := Parse([]byte(head))
	require.NoError(t, err)
	out, err := Render(doc, Options{Base: []byte(base)})
	require.NoError(t, err)
	html := string(out)
	requireBalanced(t, html)

	start := strings.Index(html, `<script type="application/json" id="sample-diffs">`)
	require.Positive(t, start)
	raw := html[start+len(`<script type="application/json" id="sample-diffs">`):]
	raw = raw[:strings.Index(raw, "</script>")]
	var diffs []struct {
		Kind    string            `json:"k"`
		Base    *modelSample      `json:"b"`
		Model   string            `json:"m"`
		Changes []modelChangeView `json:"c"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &diffs))
	require.Len(t, diffs, 3)
	byKind := map[string]int{}
	for i, d := range diffs {
		byKind[d.Kind] = i
	}

	changed := diffs[byKind["changed"]]
	require.NotNil(t, changed.Base, "a changed response carries its base sample")
	require.Contains(t, string(changed.Base.Value), `"total"`)
	require.Empty(t, changed.Model, "an inline response names no model")
	paths := map[string]modelChangeView{}
	for _, c := range changed.Changes {
		paths[c.Path] = c
	}
	require.Equal(t, "added", paths["next"].Kind)
	require.Equal(t, "changed", paths["total"].Kind)
	require.True(t, paths["total"].Breaking)
	require.Equal(t, "inside", paths["pets[]"].Kind, "the list's model changed inside")
	require.Equal(t, "model/Pet", paths["pets[]"].Model)

	require.Nil(t, diffs[byKind["added"]].Base)
	require.Equal(t, "model/Problem", diffs[byKind["added"]].Model)
	require.Equal(t, "model/Problem", diffs[byKind["removed"]].Model)

	op := section(t, html, `id="operation/pets-list"`)
	// Tabs in status order, each with its letters; the removed one struck.
	require.Contains(t, op, `<button type="button" role="tab" class="tab s2xx" aria-selected="true">200<span class="diff-dot diff-changed" title="CHANGED, BREAKING">C</span><span class="diff-dot diff-breaking" title="BREAKING">!</span></button>`)
	require.Contains(t, op, `<button type="button" role="tab" class="tab s2xx" aria-selected="false">202<span class="diff-dot diff-added" title="ADDED">A</span></button>`)
	require.Contains(t, op, `<button type="button" role="tab" class="tab s4xx tab-removed" aria-selected="false">404<span class="diff-dot diff-removed" title="REMOVED">R</span></button>`)
	require.Contains(t, op, `<div class="tabpanel oi-panel tabpanel-removed" role="tabpanel" hidden>`)
	require.Contains(t, op, ` data-sdiff="`)
	// The removed panel shows the base sample.
	require.Contains(t, op, "tabpanel-removed\" role=\"tabpanel\" hidden>")
	require.Regexp(t, `tabpanel-removed" role="tabpanel" hidden>.*?data-sdiff="\d+"></div><pre class="raw"><code class="json">\{\s+"message": "string"`, op)

	// Ports: the new header is added, the old one comes back removed, the
	// changed response is marked, the removed one is a removed port.
	require.Contains(t, op, `pdiff diff-added" data-port="in:header:X-New"`)
	require.Contains(t, op, `pdiff diff-removed" data-port="in:removed:header:X-Old"`)
	require.Contains(t, op, `pdiff diff-changed" data-port="out:200"`)
	require.Contains(t, op, `pdiff diff-added" data-port="out:202"`)
	require.Contains(t, op, `pdiff diff-removed" data-port="out:removed:404"`)
	require.NotContains(t, op, `data-port="in:optional"`+` class`, "the unchanged optional port has no mark")

	plain, err := Render(doc, Options{})
	require.NoError(t, err)
	require.NotContains(t, string(plain), `id="sample-diffs"`)
	require.NotContains(t, string(plain), `<span class="pdiff-mark">`)
}
