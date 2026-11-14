package reference

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

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

// keywordSpec is a document whose POST /things takes the schema s as the
// query parameter q and as the request body property name, and returns it
// as the response property name. Code and Code2 are models a schema can
// refer to.
func keywordSpec(t *testing.T, version, s string) *Document {
	t.Helper()
	return keywordSpecWith(t, version, s, `"Code": {"type": "string"}, "Code2": {"type": "string"}`)
}

// keywordSpecWith is keywordSpec with the component schemas given.
func keywordSpecWith(t *testing.T, version, s, schemas string) *Document {
	t.Helper()
	if version == "" {
		version = "3.1.0"
	}
	spec := `{
  "openapi": "` + version + `",
  "info": {"title": "t", "version": "1"},
  "paths": {"/things": {"post": {
    "parameters": [{"name": "q", "in": "query", "schema": ` + s + `}],
    "requestBody": {"content": {"application/json": {"schema": {"type": "object", "properties": {"name": ` + s + `}}}}},
    "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {"type": "object", "properties": {"name": ` + s + `}}}}}}
  }}},
  "components": {"schemas": {` + schemas + `}}
}`
	doc, err := Parse([]byte(spec))
	require.NoError(t, err)
	return doc
}

// changesAt returns the changes of od at path.
func changesAt(od *OperationDiff, path string) []Change {
	var out []Change
	for _, c := range od.Changes {
		if c.Path == path {
			out = append(out, c)
		}
	}
	return out
}

// TestDiffValidationKeywords checks every validation keyword in a request
// body property, a query parameter and a response property: a change that
// narrows the accepted values breaks the request and the parameter, one
// that widens them breaks the response, and one with no direction breaks
// all three or none.
func TestDiffValidationKeywords(t *testing.T) {
	tests := []struct {
		name    string
		version string // 3.1.0 when empty
		base    string
		head    string
		detail  string
		narrows bool // breaking in a request
		widens  bool // breaking in a response
	}{
		{"minimum raised", "", `{"type": "integer", "minimum": 0}`, `{"type": "integer", "minimum": 5}`, "minimum raised from 0 to 5", true, false},
		{"minimum lowered", "", `{"type": "integer", "minimum": 5}`, `{"type": "integer", "minimum": 0}`, "minimum lowered from 5 to 0", false, true},
		{"minimum added", "", `{"type": "integer"}`, `{"type": "integer", "minimum": 5}`, "minimum added: 5", true, false},
		{"minimum removed", "", `{"type": "integer", "minimum": 5}`, `{"type": "integer"}`, "minimum removed (was 5)", false, true},
		{"maximum lowered", "", `{"type": "number", "maximum": 100}`, `{"type": "number", "maximum": 10.5}`, "maximum lowered from 100 to 10.5", true, false},
		{"maximum raised", "", `{"type": "number", "maximum": 10}`, `{"type": "number", "maximum": 100}`, "maximum raised from 10 to 100", false, true},
		{"maximum added", "", `{"type": "number"}`, `{"type": "number", "maximum": 10}`, "maximum added: 10", true, false},
		{"maximum removed", "", `{"type": "number", "maximum": 10}`, `{"type": "number"}`, "maximum removed (was 10)", false, true},

		{"3.0 minimum made exclusive", "3.0.3", `{"type": "integer", "minimum": 0}`, `{"type": "integer", "minimum": 0, "exclusiveMinimum": true}`, "minimum raised from 0 to 0 exclusive", true, false},
		{"3.0 minimum made inclusive", "3.0.3", `{"type": "integer", "minimum": 0, "exclusiveMinimum": true}`, `{"type": "integer", "minimum": 0}`, "minimum lowered from 0 exclusive to 0", false, true},
		{"3.0 exclusive maximum raised", "3.0.3", `{"type": "number", "maximum": 10, "exclusiveMaximum": true}`, `{"type": "number", "maximum": 20, "exclusiveMaximum": true}`, "maximum raised from 10 exclusive to 20 exclusive", false, true},
		{"3.0 maximum made exclusive", "3.0.3", `{"type": "number", "maximum": 10}`, `{"type": "number", "maximum": 10, "exclusiveMaximum": true}`, "maximum lowered from 10 to 10 exclusive", true, false},
		{"3.1 minimum made exclusive", "", `{"type": "integer", "minimum": 0}`, `{"type": "integer", "exclusiveMinimum": 0}`, "minimum raised from 0 to 0 exclusive", true, false},
		{"3.1 maximum made inclusive", "", `{"type": "number", "exclusiveMaximum": 10}`, `{"type": "number", "maximum": 10}`, "maximum raised from 10 exclusive to 10", false, true},
		{"3.1 exclusive minimum added", "", `{"type": "number"}`, `{"type": "number", "exclusiveMinimum": 1}`, "minimum added: 1 exclusive", true, false},
		{"3.1 tighter of both bounds", "", `{"type": "number", "minimum": 5, "exclusiveMinimum": 1}`, `{"type": "number", "minimum": 5, "exclusiveMinimum": 7}`, "minimum raised from 5 to 7 exclusive", true, false},

		{"multipleOf added", "", `{"type": "integer"}`, `{"type": "integer", "multipleOf": 5}`, "multipleOf added: 5", true, false},
		{"multipleOf removed", "", `{"type": "integer", "multipleOf": 5}`, `{"type": "integer"}`, "multipleOf removed (was 5)", false, true},
		{"multipleOf changed", "", `{"type": "integer", "multipleOf": 5}`, `{"type": "integer", "multipleOf": 10}`, "multipleOf changed from 5 to 10", true, true},

		{"minLength raised", "", `{"type": "string", "minLength": 1}`, `{"type": "string", "minLength": 3}`, "minLength raised from 1 to 3", true, false},
		{"minLength lowered", "", `{"type": "string", "minLength": 3}`, `{"type": "string", "minLength": 1}`, "minLength lowered from 3 to 1", false, true},
		{"minLength added", "", `{"type": "string"}`, `{"type": "string", "minLength": 3}`, "minLength added: 3", true, false},
		{"minLength added as 0", "", `{"type": "string"}`, `{"type": "string", "minLength": 0}`, "minLength added: 0", false, false},
		{"minLength removed", "", `{"type": "string", "minLength": 3}`, `{"type": "string"}`, "minLength removed (was 3)", false, true},
		{"maxLength lowered", "", `{"type": "string", "maxLength": 50}`, `{"type": "string", "maxLength": 10}`, "maxLength lowered from 50 to 10", true, false},
		{"maxLength raised", "", `{"type": "string", "maxLength": 10}`, `{"type": "string", "maxLength": 50}`, "maxLength raised from 10 to 50", false, true},
		{"maxLength added", "", `{"type": "string"}`, `{"type": "string", "maxLength": 10}`, "maxLength added: 10", true, false},
		{"maxLength removed", "", `{"type": "string", "maxLength": 50}`, `{"type": "string"}`, "maxLength removed (was 50)", false, true},
		{"maxLength in an allOf member", "", `{"allOf": [{"type": "string", "maxLength": 50}]}`, `{"allOf": [{"type": "string", "maxLength": 10}]}`, "maxLength lowered from 50 to 10", true, false},

		{"pattern added", "", `{"type": "string"}`, `{"type": "string", "pattern": "^[0-9]+$"}`, `pattern added: "^[0-9]+$"`, true, false},
		{"pattern removed", "", `{"type": "string", "pattern": "^[0-9]+$"}`, `{"type": "string"}`, `pattern removed (was "^[0-9]+$")`, false, true},
		{"pattern changed", "", `{"type": "string", "pattern": "^a$"}`, `{"type": "string", "pattern": "^b$"}`, `pattern changed from "^a$" to "^b$"`, true, true},
		{"pattern next to a model", "", `{"allOf": [{"$ref": "#/components/schemas/Code"}]}`, `{"allOf": [{"$ref": "#/components/schemas/Code"}], "pattern": "^c$"}`, `pattern added: "^c$"`, true, false},

		{"minItems raised", "", `{"type": "array", "items": {"type": "string"}, "minItems": 1}`, `{"type": "array", "items": {"type": "string"}, "minItems": 2}`, "minItems raised from 1 to 2", true, false},
		{"minItems removed", "", `{"type": "array", "items": {"type": "string"}, "minItems": 1}`, `{"type": "array", "items": {"type": "string"}}`, "minItems removed (was 1)", false, true},
		{"maxItems added", "", `{"type": "array", "items": {"type": "string"}}`, `{"type": "array", "items": {"type": "string"}, "maxItems": 5}`, "maxItems added: 5", true, false},
		{"maxItems raised", "", `{"type": "array", "items": {"type": "string"}, "maxItems": 5}`, `{"type": "array", "items": {"type": "string"}, "maxItems": 9}`, "maxItems raised from 5 to 9", false, true},
		{"uniqueItems added", "", `{"type": "array", "items": {"type": "string"}}`, `{"type": "array", "items": {"type": "string"}, "uniqueItems": true}`, "uniqueItems added", true, false},
		{"uniqueItems removed", "", `{"type": "array", "items": {"type": "string"}, "uniqueItems": true}`, `{"type": "array", "items": {"type": "string"}}`, "uniqueItems removed", false, true},

		{"minProperties added", "", `{"type": "object"}`, `{"type": "object", "minProperties": 1}`, "minProperties added: 1", true, false},
		{"minProperties lowered", "", `{"type": "object", "minProperties": 2}`, `{"type": "object", "minProperties": 1}`, "minProperties lowered from 2 to 1", false, true},
		{"maxProperties lowered", "", `{"type": "object", "maxProperties": 5}`, `{"type": "object", "maxProperties": 3}`, "maxProperties lowered from 5 to 3", true, false},
		{"maxProperties removed", "", `{"type": "object", "maxProperties": 5}`, `{"type": "object"}`, "maxProperties removed (was 5)", false, true},

		{"const added", "", `{"type": "string"}`, `{"type": "string", "const": "a"}`, `const added: "a"`, true, false},
		{"const removed", "", `{"type": "string", "const": "a"}`, `{"type": "string"}`, `const removed (was "a")`, false, true},
		{"const changed", "", `{"type": "string", "const": "a"}`, `{"type": "string", "const": "b"}`, `const changed from "a" to "b"`, true, true},

		{"default changed", "", `{"type": "integer", "default": 1}`, `{"type": "integer", "default": 2}`, "default changed from 1 to 2", false, false},
		{"default added", "", `{"type": "integer"}`, `{"type": "integer", "default": 1}`, "default added: 1", false, false},
		{"default removed", "", `{"type": "integer", "default": 1}`, `{"type": "integer"}`, "default removed (was 1)", false, false},

		{"no longer nullable", "3.0.3", `{"type": "string", "nullable": true}`, `{"type": "string"}`, "no longer nullable", true, false},
		{"made nullable", "", `{"type": "string"}`, `{"type": ["string", "null"]}`, "made nullable", false, true},

		{"additional properties closed", "", `{"type": "object"}`, `{"type": "object", "additionalProperties": false}`, "additional properties no longer allowed", true, false},
		{"additional properties closed from true", "", `{"type": "object", "additionalProperties": true}`, `{"type": "object", "additionalProperties": false}`, "additional properties no longer allowed", true, false},
		{"additional properties closed from a schema", "", `{"type": "object", "additionalProperties": {"type": "string"}}`, `{"type": "object", "additionalProperties": false}`, "additional properties no longer allowed", true, false},
		{"additional properties opened", "", `{"type": "object", "additionalProperties": false}`, `{"type": "object"}`, "additional properties allowed", false, false},

		{"maxLength lowered in a later allOf member", "", `{"allOf": [{"type": "string", "maxLength": 100}, {"maxLength": 50}]}`, `{"allOf": [{"type": "string", "maxLength": 100}, {"maxLength": 10}]}`, "maxLength lowered from 50 to 10", true, false},
		{"minimum raised in an earlier allOf member", "", `{"allOf": [{"type": "integer", "minimum": 1}, {"minimum": 5}]}`, `{"allOf": [{"type": "integer", "minimum": 7}, {"minimum": 5}]}`, "minimum raised from 5 to 7", true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := Diff(keywordSpec(t, tt.version, tt.base), keywordSpec(t, tt.version, tt.head))
			od := r.Operation("POST", "/things")
			require.NotNil(t, od, "the change is reported")
			want := map[string]bool{
				"POST /things › query › q":           tt.narrows,
				"POST /things › request body › name": tt.narrows,
				"POST /things › response 200 › name": tt.widens,
			}
			for path, breaking := range want {
				require.Equal(t, []Change{{Kind: Changed, Path: path, Detail: tt.detail, Breaking: breaking}}, changesAt(od, path), path)
			}
			require.Len(t, od.Changes, len(want), "no other changes")
			require.Equal(t, tt.narrows || tt.widens, od.Breaking)
		})
	}
}

// TestDiffBoundForms checks that the 3.0 and 3.1 spellings of the same
// exclusive bound are the same bound, also in a document built without
// Parse, which still holds the 3.0 boolean.
func TestDiffBoundForms(t *testing.T) {
	r := Diff(
		keywordSpec(t, "3.0.3", `{"type": "number", "minimum": 0, "exclusiveMinimum": true, "maximum": 9, "exclusiveMaximum": true}`),
		keywordSpec(t, "3.1.0", `{"type": "number", "exclusiveMinimum": 0, "exclusiveMaximum": 9}`),
	)
	require.True(t, r.Empty(), r.Markdown())

	zero := 0.0
	raw := &Schema{Minimum: &zero, ExclusiveMinimum: &Bound{Flag: true}}
	require.Equal(t, &numBound{value: 0, exclusive: true}, lowerBound(raw))
	require.Nil(t, upperBound(raw))
}

// TestDiffResponseHeaders checks response headers: removed, added, made
// optional or required, their schemas compared in response context, $ref
// headers resolved and names matched regardless of case.
func TestDiffResponseHeaders(t *testing.T) {
	spec := func(headers, trace string) *Document {
		doc, err := Parse([]byte(`{
  "openapi": "3.1.0",
  "info": {"title": "t", "version": "1"},
  "paths": {"/things": {"get": {"responses": {"200": {"description": "ok", "headers": ` + headers + `}}}}},
  "components": {"headers": {"Trace": {"schema": ` + trace + `}}}
}`))
		require.NoError(t, err)
		return doc
	}
	base := spec(`{
  "X-Old": {"schema": {"type": "string"}},
  "X-Rate-Limit": {"required": true, "schema": {"type": "integer", "maximum": 100}},
  "X-Count": {"schema": {"type": "string", "maxLength": 50}},
  "X-Req": {"schema": {"type": "string"}},
  "X-Trace": {"$ref": "#/components/headers/Trace"}
}`, `{"type": "string", "maxLength": 10}`)
	head := spec(`{
  "X-Rate-Limit": {"schema": {"type": "integer", "maximum": 1000}},
  "X-Count": {"schema": {"type": "string", "maxLength": 10}},
  "X-Req": {"required": true, "schema": {"type": "string"}},
  "x-trace": {"$ref": "#/components/headers/Trace"},
  "X-New": {"schema": {"type": "string"}}
}`, `{"type": "string", "maxLength": 20}`)

	od := Diff(base, head).Operation("GET", "/things")
	require.NotNil(t, od)
	const at = "GET /things › response 200 › header "
	require.ElementsMatch(t, []Change{
		{Kind: Removed, Path: at + "X-Old", Detail: "header removed", Breaking: true},
		{Kind: Changed, Path: at + "X-Rate-Limit", Detail: "header made optional", Breaking: true},
		{Kind: Changed, Path: at + "X-Rate-Limit", Detail: "maximum raised from 100 to 1000", Breaking: true},
		{Kind: Changed, Path: at + "X-Count", Detail: "maxLength lowered from 50 to 10", Breaking: false},
		{Kind: Changed, Path: at + "X-Req", Detail: "header made required", Breaking: false},
		{Kind: Changed, Path: at + "x-trace", Detail: "maxLength raised from 10 to 20", Breaking: true},
		{Kind: Added, Path: at + "X-New", Detail: "header added", Breaking: false},
	}, od.Changes)
	require.True(t, od.Breaking)
}

// keywordsSpec carries every validation keyword, response headers and both
// spellings of exclusive bounds.
const keywordsSpec = `{
  "openapi": "%s",
  "info": {"title": "t", "version": "1"},
  "paths": {"/things": {"post": {
    "parameters": [{"name": "q", "in": "query", "schema": {"type": "string", "minLength": 1, "maxLength": 50, "pattern": "^[a-z]+$", "default": "a"}}],
    "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Thing"}}}},
    "responses": {"200": {
      "description": "ok",
      "headers": {
        "X-Rate-Limit": {"required": true, "schema": {"type": "integer", "minimum": 0, "maximum": 1000}},
        "X-Trace": {"$ref": "#/components/headers/Trace"}
      },
      "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Thing"}}}
    }}
  }}},
  "components": {
    "headers": {"Trace": {"schema": {"type": "string", "maxLength": 64}}},
    "schemas": {"Thing": {"type": "object", "additionalProperties": false, "minProperties": 1, "maxProperties": 9, "properties": {
      "count": {"type": "integer", "minimum": 0, "maximum": 100, %s, "multipleOf": 5, "default": 10},
      "name": {"type": "string", "minLength": 1, "maxLength": 50, "pattern": "^[0-9]+$", "nullable": true},
      "tags": {"type": "array", "items": {"type": "string"}, "minItems": 1, "maxItems": 5, "uniqueItems": true},
      "kind": {"type": "string", "const": "thing"},
      "extra": {"type": "object", "additionalProperties": {"type": "string"}},
      "code": {"allOf": [{"type": "string", "maxLength": 8}], "default": "x"}
    }}}
  }
}`

func TestDiffKeywordsWithItself(t *testing.T) {
	for _, form := range []struct{ version, exclusive string }{
		{"3.0.3", `"exclusiveMaximum": true`},
		{"3.1.0", `"exclusiveMaximum": 100`},
	} {
		parse := func() *Document {
			doc, err := Parse([]byte(fmt.Sprintf(keywordsSpec, form.version, form.exclusive)))
			require.NoError(t, err)
			return doc
		}
		r := Diff(parse(), parse())
		require.True(t, r.Empty(), r.Markdown())
		require.Equal(t, "# API changes\n\nNo changes.\n", r.Markdown())
	}
}

// TestDiffKeywordsEndToEnd follows a narrowed request property, a removed
// response header and a narrowed model through the markdown output and the
// rendered page.
func TestDiffKeywordsEndToEnd(t *testing.T) {
	base := fmt.Sprintf(keywordsSpec, "3.1.0", `"exclusiveMaximum": 100`)
	head := strings.Replace(base, `"name": {"type": "string", "minLength": 1, "maxLength": 50`, `"name": {"type": "string", "minLength": 1, "maxLength": 10`, 1)
	head = strings.Replace(head, `"X-Rate-Limit": {"required": true, "schema": {"type": "integer", "minimum": 0, "maximum": 1000}},`, ``, 1)
	require.NotEqual(t, base, head)
	doc, err := Parse([]byte(head))
	require.NoError(t, err)
	baseDoc, err := Parse([]byte(base))
	require.NoError(t, err)

	r := Diff(baseDoc, doc)
	require.True(t, r.Breaking())
	thing := r.Model("Thing")
	require.NotNil(t, thing)
	require.True(t, thing.Breaking, "Thing is a request body")

	md := r.Markdown()
	breaking := strings.Index(md, "## Breaking changes")
	other := strings.Index(md, "## Other changes")
	require.True(t, breaking > 0 && other > breaking, md)
	require.Contains(t, md, "3 breaking and 1 non-breaking changes in 1 operation and 1 model.\n")
	require.Contains(t, md[breaking:other], "- `POST /things` › response 200 › header X-Rate-Limit: header removed\n")
	require.Contains(t, md[breaking:other], "- `POST /things` › request body: model `Thing` changed\n")
	require.Contains(t, md[breaking:other], "### Models\n\n- `Thing` › name: maxLength lowered from 50 to 10\n")
	// A narrower response breaks no reader.
	require.Contains(t, md[other:], "- `POST /things` › response 200: model `Thing` changed\n")

	out, err := Render(doc, Options{Base: []byte(base)})
	require.NoError(t, err)
	html := string(out)
	requireBalanced(t, html)
	changes := section(t, html, `id="section/changes"`)
	require.Contains(t, changes, "maxLength lowered from 50 to 10")
	require.Contains(t, changes, "header removed")
	require.Contains(t, html, `<article class="op diff-changed" id="operation/post-things"`)
	card := section(t, html, `id="model/Thing"`)
	require.Contains(t, card, `<div class="row diff-del"><div class="row-head"><span class="pn">name</span>`, "the old row is written before the changed one")
}

// TestDiffAdditionalPropertiesAny checks that additionalProperties: {},
// true and none at all are spellings of one thing, an object that allows any
// extra property, so no change between them is reported, in 3.0 and 3.1,
// from JSON and from YAML.
func TestDiffAdditionalPropertiesAny(t *testing.T) {
	spellings := []string{
		`{"type": "object"}`,
		`{"type": "object", "additionalProperties": true}`,
		`{"type": "object", "additionalProperties": {}}`,
		`{"type": "object", "additionalProperties": {"x-note": "any"}}`,
	}
	for _, version := range []string{"3.0.3", "3.1.0"} {
		for _, base := range spellings {
			for _, head := range spellings {
				r := Diff(keywordSpec(t, version, base), keywordSpec(t, version, head))
				require.True(t, r.Empty(), "%s: %s to %s: %s", version, base, head, r.Markdown())
			}
		}
	}

	// An emma-style map: a free-form metadata object on a model.
	yamlSpec := func(additional string) *Document {
		return parseYAML(t, `openapi: 3.0.3
info: {title: t, version: "1"}
paths:
  /units:
    post:
      requestBody: {content: {application/json: {schema: {$ref: '#/components/schemas/Unit'}}}}
      responses: {"200": {description: ok, content: {application/json: {schema: {$ref: '#/components/schemas/Unit'}}}}}
components:
  schemas:
    Unit:
      type: object
      properties:
        metadata:
          type: object`+additional+`
`)
	}
	yamlSpellings := []string{"", "\n          additionalProperties: true", "\n          additionalProperties: {}"}
	for _, base := range yamlSpellings {
		for _, head := range yamlSpellings {
			r := Diff(yamlSpec(base), yamlSpec(head))
			require.True(t, r.Empty(), "%q to %q: %s", base, head, r.Markdown())
		}
	}
}

// TestDiffAdditionalPropertiesSchemas checks that the schemas of the
// additional properties of two open objects are compared like any others,
// under <key>, with the empty schema standing for an absent keyword, true
// and {}.
func TestDiffAdditionalPropertiesSchemas(t *testing.T) {
	tests := []struct {
		name    string
		version string // 3.1.0 when empty
		base    string
		head    string
		detail  string
		narrows bool // breaking in a request
		widens  bool // breaking in a response
	}{
		{"schema added", "", `{"type": "object"}`, `{"type": "object", "additionalProperties": {"type": "string"}}`, "type changed from any to string", true, false},
		{"schema removed", "", `{"type": "object", "additionalProperties": {"type": "string"}}`, `{"type": "object"}`, "type changed from string to any", false, true},
		{"schema replaced by {}", "3.0.3", `{"type": "object", "additionalProperties": {"type": "string"}}`, `{"type": "object", "additionalProperties": {}}`, "type changed from string to any", false, true},
		{"schema replaced by true", "", `{"type": "object", "additionalProperties": {"type": "string"}}`, `{"type": "object", "additionalProperties": true}`, "type changed from string to any", false, true},
		{"bound added to none", "", `{"type": "object"}`, `{"type": "object", "additionalProperties": {"maxLength": 5}}`, "maxLength added: 5", true, false},
		{"bound added to true", "", `{"type": "object", "additionalProperties": true}`, `{"type": "object", "additionalProperties": {"maxLength": 5}}`, "maxLength added: 5", true, false},
		{"bound added to {}", "", `{"type": "object", "additionalProperties": {}}`, `{"type": "object", "additionalProperties": {"maxLength": 5}}`, "maxLength added: 5", true, false},
		{"bound removed", "", `{"type": "object", "additionalProperties": {"maxLength": 5}}`, `{"type": "object"}`, "maxLength removed (was 5)", false, true},
		{"description added", "", `{"type": "object", "additionalProperties": {}}`, `{"type": "object", "additionalProperties": {"description": "d"}}`, "description changed", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := Diff(keywordSpec(t, tt.version, tt.base), keywordSpec(t, tt.version, tt.head))
			od := r.Operation("POST", "/things")
			require.NotNil(t, od, "the change is reported")
			want := map[string]bool{
				"POST /things › query › q › <key>":           tt.narrows,
				"POST /things › request body › name › <key>": tt.narrows,
				"POST /things › response 200 › name › <key>": tt.widens,
			}
			for path, breaking := range want {
				require.Equal(t, []Change{{Kind: Changed, Path: path, Detail: tt.detail, Breaking: breaking}}, changesAt(od, path), path)
			}
			require.Len(t, od.Changes, len(want), "no other changes")
		})
	}

	// emma's Profile.metadata went from a map of objects to a map of any
	// value, which a client reading it as objects can no longer rely on.
	yamlSpec := func(additional string) *Document {
		return parseYAML(t, `openapi: 3.0.3
info: {title: t, version: "1"}
paths:
  /profiles:
    get:
      responses: {"200": {description: ok, content: {application/json: {schema: {$ref: '#/components/schemas/Profile'}}}}}
components:
  schemas:
    Profile:
      type: object
      properties:
        metadata:
          type: object
          additionalProperties: `+additional+`
`)
	}
	r := Diff(yamlSpec("{type: object}"), yamlSpec("{}"))
	profile := r.Model("Profile")
	require.NotNil(t, profile)
	require.Equal(t, []Change{{Kind: Changed, Path: "Profile › metadata › <key>", Detail: "type changed from object to any", Breaking: true}}, profile.Changes)
	require.True(t, r.Breaking())
}

// TestDiffAllOfBoundsReordered checks that allOf members that swap their
// bounds leave the tightest, the one that holds, where it was.
func TestDiffAllOfBoundsReordered(t *testing.T) {
	r := Diff(
		keywordSpec(t, "", `{"allOf": [{"minimum": 1}, {"minimum": 5}]}`),
		keywordSpec(t, "", `{"allOf": [{"minimum": 5}, {"minimum": 1}]}`),
	)
	require.True(t, r.Empty(), r.Markdown())
}

// TestDiffKeywordsBesideRef checks that keywords written beside a $ref apply
// with the model it names, written as an allOf of the model and the
// keywords, as a single-member allOf with the keywords on the wrapper or as
// a $ref with siblings, and when the two sides name different models.
func TestDiffKeywordsBesideRef(t *testing.T) {
	members := `{"allOf": [{"$ref": "#/components/schemas/Code"}, {"maxLength": 5}]}`
	beside := `{"$ref": "#/components/schemas/Code", "maxLength": 5}`
	wrapper := `{"allOf": [{"$ref": "#/components/schemas/Code"}], "maxLength": 5}`
	spellings := []string{members, beside, wrapper}
	for _, version := range []string{"3.0.3", "3.1.0"} {
		for _, base := range spellings {
			for _, head := range spellings {
				r := Diff(keywordSpec(t, version, base), keywordSpec(t, version, head))
				require.True(t, r.Empty(), "%s: %s to %s: %s", version, base, head, r.Markdown())
			}
		}
	}

	paths := map[string]bool{
		"POST /things › query › q":           true,
		"POST /things › request body › name": true,
		"POST /things › response 200 › name": false,
	}

	r := Diff(
		keywordSpec(t, "", `{"$ref": "#/components/schemas/Code", "maxLength": 5}`),
		keywordSpec(t, "", `{"$ref": "#/components/schemas/Code2", "maxLength": 3}`),
	)
	od := r.Operation("POST", "/things")
	require.NotNil(t, od)
	for path, narrows := range paths {
		require.Equal(t, []Change{
			{Kind: Changed, Path: path, Detail: "schema changed from Code to Code2", parts: []string{"schema changed from ", "Code", " to ", "Code2"}},
			{Kind: Changed, Path: path, Detail: "maxLength lowered from 5 to 3", Breaking: narrows},
		}, changesAt(od, path), path)
	}
	require.Len(t, od.Changes, 6)

	r = Diff(
		keywordSpec(t, "", `{"allOf": [{"$ref": "#/components/schemas/Code"}], "maxLength": 5}`),
		keywordSpec(t, "", `{"$ref": "#/components/schemas/Code2", "maxLength": 5}`),
	)
	od = r.Operation("POST", "/things")
	require.NotNil(t, od)
	for path := range paths {
		require.Equal(t, []Change{{Kind: Changed, Path: path, Detail: "schema changed from Code to Code2", parts: []string{"schema changed from ", "Code", " to ", "Code2"}}}, changesAt(od, path), path)
	}
	require.Len(t, od.Changes, 3)
}

// TestDiffKeywordsBesideRefWithModel checks that the keywords beside a $ref
// are compared together with the model they apply to: a bound looser than
// the model's changes nothing, and keywords inside an allOf member beside
// the $ref, or an allOf beside it, apply as they do written beside it.
func TestDiffKeywordsBesideRefWithModel(t *testing.T) {
	const code = `{"$ref": "#/components/schemas/Code"}`
	tests := []struct {
		name    string
		version string // 3.1.0 when empty
		schemas string
		base    string
		head    string
		detail  string // "" when nothing is reported
	}{
		{"looser bound added", "", `"Code": {"type": "string", "maxLength": 5}`, code, `{"$ref": "#/components/schemas/Code", "maxLength": 10}`, ""},
		{"looser bound raised", "", `"Code": {"type": "string", "maxLength": 5}`, `{"$ref": "#/components/schemas/Code", "maxLength": 8}`, `{"$ref": "#/components/schemas/Code", "maxLength": 10}`, ""},
		{"bound lowered below the model's", "", `"Code": {"type": "string", "maxLength": 5}`, `{"$ref": "#/components/schemas/Code", "maxLength": 8}`, `{"$ref": "#/components/schemas/Code", "maxLength": 3}`, "maxLength lowered from 5 to 3"},
		{"nullable model", "3.0.3", `"Code": {"type": "string", "nullable": true}`, `{"allOf": [` + code + `], "nullable": true}`, code, ""},
		{"read-only model", "", `"Code": {"type": "string", "readOnly": true}`, `{"allOf": [` + code + `], "readOnly": true}`, code, ""},
		{"bound in the only member", "", `"Code": {"type": "string"}`, `{"allOf": [{"$ref": "#/components/schemas/Code", "maxLength": 3}]}`, `{"$ref": "#/components/schemas/Code", "maxLength": 3}`, ""},
		{"allOf beside the $ref", "", `"Code": {"type": "string"}`, `{"$ref": "#/components/schemas/Code", "allOf": [{"maxLength": 3}]}`, `{"$ref": "#/components/schemas/Code", "maxLength": 3}`, ""},
		{"bound in one of two members", "", `"Code": {"type": "string"}`, `{"allOf": [{"$ref": "#/components/schemas/Code", "maxLength": 3}, {"description": "d"}]}`, `{"$ref": "#/components/schemas/Code", "maxLength": 3, "description": "d"}`, ""},
		{"bound in a member lowered", "", `"Code": {"type": "string"}`, `{"allOf": [{"$ref": "#/components/schemas/Code", "maxLength": 3}, {"description": "d"}]}`, `{"$ref": "#/components/schemas/Code", "maxLength": 2, "description": "d"}`, "maxLength lowered from 3 to 2"},
		{"bound through an alias", "", `"Code": {"type": "string"}, "Short": {"$ref": "#/components/schemas/Code", "maxLength": 3}`, `{"allOf": [{"$ref": "#/components/schemas/Short"}, {"description": "d"}]}`, `{"$ref": "#/components/schemas/Code", "maxLength": 3, "description": "d"}`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, pair := range [][2]string{{tt.base, tt.head}, {tt.head, tt.base}} {
				r := Diff(keywordSpecWith(t, tt.version, pair[0], tt.schemas), keywordSpecWith(t, tt.version, pair[1], tt.schemas))
				if tt.detail == "" {
					require.True(t, r.Empty(), "%s to %s: %s", pair[0], pair[1], r.Markdown())
					continue
				}
				od := r.Operation("POST", "/things")
				require.NotNil(t, od)
				if pair[0] == tt.base {
					require.Equal(t, []Change{{Kind: Changed, Path: "POST /things › request body › name", Detail: tt.detail, Breaking: true}}, changesAt(od, "POST /things › request body › name"))
					require.Equal(t, []Change{{Kind: Changed, Path: "POST /things › response 200 › name", Detail: tt.detail}}, changesAt(od, "POST /things › response 200 › name"))
				}
				require.Len(t, od.Changes, 3, r.Markdown())
			}
		})
	}

	// A model that is a reference with keywords beside it is a model of its
	// own: a reference to it inherits its changes.
	r := Diff(
		keywordSpecWith(t, "", `{"$ref": "#/components/schemas/Short"}`, `"Code": {"type": "string"}, "Short": {"$ref": "#/components/schemas/Code", "maxLength": 3}`),
		keywordSpecWith(t, "", `{"$ref": "#/components/schemas/Short"}`, `"Code": {"type": "string"}, "Short": {"$ref": "#/components/schemas/Code", "maxLength": 2}`),
	)
	require.Equal(t, []Change{{Kind: Changed, Path: "Short", Detail: "maxLength lowered from 3 to 2", Breaking: true}}, r.Model("Short").Changes)
	od := r.Operation("POST", "/things")
	require.NotNil(t, od)
	require.Len(t, od.Changes, 3)
	require.Equal(t, "model Short changed", od.Changes[0].Detail)
	require.True(t, od.Breaking)

	// A change to the model itself is reported once, through the model.
	r = Diff(
		keywordSpecWith(t, "", `{"$ref": "#/components/schemas/Code", "maxLength": 8}`, `"Code": {"type": "string", "maxLength": 5}`),
		keywordSpecWith(t, "", `{"$ref": "#/components/schemas/Code", "maxLength": 8}`, `"Code": {"type": "string", "maxLength": 3}`),
	)
	od = r.Operation("POST", "/things")
	require.NotNil(t, od)
	for _, c := range od.Changes {
		require.Equal(t, "model Code changed", c.Detail, c.Path)
	}
	require.Equal(t, []Change{{Kind: Changed, Path: "Code", Detail: "maxLength lowered from 5 to 3", Breaking: true}}, r.Model("Code").Changes)
}

// TestDiffMarkdownEscapesSpecText checks that spec text in the markdown
// summary, path segments and details, shows as written once rendered and
// cannot add headings, list items or markup of its own.
func TestDiffMarkdownEscapesSpecText(t *testing.T) {
	const injection = `<img src=x onerror=alert(1)>\n## injected\n- item`
	spec := func(head bool) *Document {
		headers := `{}`
		properties := `"code": {"type": "string"}, "note": {"type": "string"}`
		paths := ``
		if head {
			headers = `{"` + injection + `": {"schema": {"type": "string"}}}`
			properties = `"code": {"type": "string", "pattern": "^[A-Z]*-[0-9]*$"}, "note": {"type": "string", "pattern": "` + injection + `"}`
			paths = `, "/x` + "`" + `y\n# z": {"get": {"responses": {"204": {"description": "none"}}}}`
		}
		doc, err := Parse([]byte(`{
  "openapi": "3.1.0",
  "info": {"title": "t", "version": "1"},
  "paths": {"/things": {"get": {"tags": ["Things"], "responses": {"200": {
    "description": "ok",
    "headers": ` + headers + `,
    "content": {"application/json": {"schema": {"type": "object", "properties": {` + properties + `}}}}
  }}}}` + paths + `}
}`))
		require.NoError(t, err)
		return doc
	}

	r := Diff(spec(false), spec(true))
	require.Len(t, r.Changes, 4)
	md := r.Markdown()
	require.Contains(t, md, "- `GET /things` › response 200 › code: pattern added: \"^\\[A-Z\\]\\*-\\[0-9\\]\\*$\"\n")
	require.Contains(t, md, "- ``GET /x`y # z``: operation added\n")
	require.Equal(t, len(r.Changes), strings.Count(md, "\n- "), md)

	html := string(markdown(md))
	require.Contains(t, html, `pattern added: &quot;^[A-Z]*-[0-9]*$&quot;`)
	require.Contains(t, html, "<code>GET /x`y # z</code>")
	require.NotContains(t, html, "<img")
	require.NotContains(t, html, "<em>")
	require.NotRegexp(t, `<h[1-6][^>]*>[^<]*injected`, html)
	require.Equal(t, 1, strings.Count(html, "<h1"), html)
	require.Equal(t, len(r.Changes), strings.Count(html, "<li>"), html)
	require.Contains(t, html, `header &lt;img src=x onerror=alert(1)&gt; ## injected - item: header added`)
	require.Contains(t, html, `pattern added: &quot;&lt;img src=x onerror=alert(1)&gt;\n## injected\n- item&quot;`)
}

// TestDiffMarkdownNoAutolinks checks that spec text that looks like a URL,
// an email address, a GitHub mention or an issue reference renders as plain
// text in the markdown summary, whose renderer links bare URLs and
// addresses, and reads as written, with a zero-width space after the @, #
// and GH- that GitHub would otherwise link from the rendered text.
func TestDiffMarkdownNoAutolinks(t *testing.T) {
	const name = "see http://evil.example/x and www.evil.example (ftp://e.co) x@example.com @github owner/repo#12 #1 GH-3 gh-4 #a GH-b a-1"
	r := Diff(keywordSpec(t, "", `{"type": "string"}`), keywordSpec(t, "", `{"type": "object", "properties": {"`+name+`": {"type": "string"}}}`))
	md := r.Markdown()
	require.Contains(t, md, "see http\\://evil.example/x and www\\.evil.example (ftp\\://e.co) x\\@\u200bexample.com \\@\u200bgithub owner/repo\\#\u200b12 \\#\u200b1 GH-\u200b3 gh-\u200b4 \\#a GH-b a-1")
	html := string(markdown(md))
	require.NotContains(t, html, "<a")
	require.Contains(t, strings.ReplaceAll(html, "\u200b", ""), name)
	// GitHub reads the rendered text: no @ or # is followed by what it
	// would link.
	require.NotRegexp(t, `@[\w.-]|#\d|(?i)gh-\d`, html)
}

// TestRenderSampleDiffsHeaderChanges checks that a response header change,
// which has no row in the sample, is a note on the response's sample diff
// that names the header, so the status tab's badge has a visible reason.
func TestRenderSampleDiffsHeaderChanges(t *testing.T) {
	spec := `{
  "openapi": "3.1.0",
  "info": {"title": "t", "version": "%s"},
  "paths": {"/things": {"get": {
    "operationId": "things.list",
    "responses": {"200": {
      "description": "ok",
      "headers": %s,
      "content": {"application/json": {"schema": {"type": "object", "properties": {"id": {"type": "string"}}}}}
    }}
  }}},
  "components": {"schemas": {"Code": %s}}
}`
	base := fmt.Sprintf(spec, "1.0", `{
  "X-Rate-Limit": {"required": true, "schema": {"type": "integer"}},
  "X-Count": {"schema": {"type": "string", "maxLength": 50}},
  "X-Code": {"schema": {"$ref": "#/components/schemas/Code"}}
}`, `{"type": "string", "maxLength": 5}`)
	head := fmt.Sprintf(spec, "1.1", `{
  "X-Count": {"schema": {"type": "string", "maxLength": 10}},
  "X-Code": {"schema": {"$ref": "#/components/schemas/Code"}}
}`, `{"type": "string", "maxLength": 9}`)
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
		Changes []modelChangeView `json:"c"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &diffs))
	require.Len(t, diffs, 1)
	require.Equal(t, "changed", diffs[0].Kind)
	require.Equal(t, []modelChangeView{
		{Path: "", Kind: "removed", Detail: "header X-Rate-Limit removed", Breaking: true},
		{Path: "", Kind: "changed", Detail: "header X-Count: maxLength lowered from 50 to 10"},
		{Path: "", Kind: "changed", Detail: "header X-Code: model Code changed", Breaking: true},
	}, diffs[0].Changes)

	op := section(t, html, `id="operation/things-list"`)
	require.Contains(t, op, `200<span class="diff-dot diff-changed" title="CHANGED, BREAKING">C</span><span class="diff-dot diff-breaking" title="BREAKING">!</span></button>`)
}

// TestDiffResponseHeadersSkipContentType checks that a response header named
// Content-Type, in any case, is never compared: OpenAPI says to ignore it.
func TestDiffResponseHeadersSkipContentType(t *testing.T) {
	spec := func(headers string) *Document {
		doc, err := Parse([]byte(`{
  "openapi": "3.1.0",
  "info": {"title": "t", "version": "1"},
  "paths": {"/things": {"get": {"responses": {"200": {"description": "ok", "headers": ` + headers + `}}}}}
}`))
		require.NoError(t, err)
		return doc
	}
	none := spec(`{}`)
	upper := spec(`{"Content-Type": {"required": true, "schema": {"type": "string"}}}`)
	lower := spec(`{"content-type": {"schema": {"type": "integer"}}}`)
	for _, pair := range [][2]*Document{{upper, none}, {none, lower}, {upper, lower}, {lower, upper}} {
		r := Diff(pair[0], pair[1])
		require.True(t, r.Empty(), r.Markdown())
	}
}

// parentSpec is a 3.0 document whose GET /parent returns Parent, an object
// whose property p is the schema p, with M, an object whose property x has
// the description desc, and the other component schemas given.
func parentSpec(t *testing.T, p, desc, schemas string) *Document {
	t.Helper()
	if schemas != "" {
		schemas = ", " + schemas
	}
	spec := `{
  "openapi": "3.0.3",
  "info": {"title": "t", "version": "1"},
  "paths": {"/parent": {"get": {"responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Parent"}}}}}}}},
  "components": {"schemas": {
    "Parent": {"type": "object", "properties": {"p": ` + p + `}},
    "M": {"type": "object", "properties": {"x": {"type": "string", "description": "` + desc + `"}}}` + schemas + `
  }}
}`
	doc, err := Parse([]byte(spec))
	require.NoError(t, err)
	return doc
}

// changeLines is every change of r as its path and detail.
func changeLines(r *Report) []string {
	var out []string
	for _, c := range r.Changes {
		out = append(out, c.Path+" | "+c.Detail)
	}
	return out
}

// TestDiffSameModelShapes checks that a change inside a model is reported
// once, in the model, where both sides refer to it with a $ref or a
// single-member allOf around one, with a type, nullable or other keywords
// beside it that give it no structure, or through an alias of it, and that
// what changes beside the reference is still reported where it is used.
func TestDiffSameModelShapes(t *testing.T) {
	const m = `{"$ref": "#/components/schemas/M"}`
	const a = `{"$ref": "#/components/schemas/A"}`
	const nullable = `{"allOf": [` + m + `], "nullable": true, "type": "object"}`
	const typed = `{"allOf": [` + m + `], "type": "object"}`
	const recursive = `"R": {"type": "object", "properties": {"y": ` + m + `, "next": {"allOf": [{"$ref": "#/components/schemas/R"}], "nullable": true, "type": "object"}}}`
	inherited := []string{
		"GET /parent › response 200 | model Parent changed (through M)",
		"Parent › p | model M changed",
		"M › x | description changed",
	}
	tests := []struct {
		name       string
		base, head string
		schemas    string
		want       []string
	}{
		{"3.0 nullable reference", nullable, nullable, "", inherited},
		{"reference with a type", typed, typed, "", inherited},
		{"made nullable", m, nullable, "", []string{
			"GET /parent › response 200 | model Parent changed",
			"Parent › p | made nullable",
			"Parent › p | model M changed",
			"M › x | description changed",
		}},
		{"alias with a type is a model", a, a, `"A": ` + typed, []string{
			"GET /parent › response 200 | model Parent changed (through M)",
			"Parent › p | model A changed (through M)",
			"M › x | description changed",
			"A | model M changed",
		}},
		{"model to its alias", m, a, `"A": ` + m, append(slices.Clone(inherited), "A | model M changed")},
		{"alias to its model", a, m, `"A": ` + m, append(slices.Clone(inherited), "A | model M changed")},
		{"recursive model", `{"$ref": "#/components/schemas/R"}`, `{"$ref": "#/components/schemas/R"}`, recursive, []string{
			"GET /parent › response 200 | model Parent changed (through M)",
			"Parent › p | model R changed (through M)",
			"M › x | description changed",
			"R › y | model M changed",
			"R › next | model R changed (through M)",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := Diff(parentSpec(t, tt.base, "old", tt.schemas), parentSpec(t, tt.head, "new", tt.schemas))
			require.Equal(t, tt.want, changeLines(r))
		})
	}

	// An alias the head no longer has cannot be flattened there, so the
	// property is compared in full.
	r := Diff(parentSpec(t, a, "old", `"A": `+m), parentSpec(t, m, "new", ""))
	require.Equal(t, []string{
		"GET /parent › response 200 | model Parent changed",
		"Parent › p › x | description changed",
		"M › x | description changed",
		"A | model removed",
	}, changeLines(r))

	// A type or a format beside the reference that changes is reported.
	const anyModel = `"Any": {"description": "any value"}`
	r = Diff(
		parentSpec(t, `{"$ref": "#/components/schemas/Any", "type": "string", "format": "date"}`, "old", anyModel),
		parentSpec(t, `{"allOf": [{"$ref": "#/components/schemas/Any"}], "type": "integer", "format": "int64"}`, "old", anyModel),
	)
	require.Equal(t, []string{
		"GET /parent › response 200 | model Parent changed",
		"Parent › p | type changed from string to integer",
		"Parent › p | format changed from date to int64",
	}, changeLines(r))
}

// bodySpec is a 3.1 document whose POST /body takes Body, an object whose
// property f is a $ref to A, with the component schemas A and M given.
func bodySpec(t *testing.T, a, m string) *Document {
	t.Helper()
	doc, err := Parse([]byte(`{
  "openapi": "3.1.0",
  "info": {"title": "t", "version": "1"},
  "paths": {"/body": {"post": {
    "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Body"}}}},
    "responses": {"204": {"description": "none"}}
  }}},
  "components": {"schemas": {
    "Body": {"type": "object", "properties": {"f": {"$ref": "#/components/schemas/A"}}},
    "A": ` + a + `,
    "M": ` + m + `
  }}
}`))
	require.NoError(t, err)
	return doc
}

// TestDiffAliasGetsKeyword checks a component that only refers to a model
// and gets a keyword of its own, or loses the one it had: the keyword is
// reported once, with the component, and every use of the component carries
// one "model changed" entry for it and is not expanded, also when the model
// changes too.
func TestDiffAliasGetsKeyword(t *testing.T) {
	const (
		plain       = `{"type": "string", "maxLength": 20}`
		object      = `{"type": "object", "properties": {"a": {"type": "string"}}}`
		objectAfter = `{"type": "object", "properties": {"a": {"type": "integer"}}}`
		alias       = `{"allOf": [{"$ref": "#/components/schemas/M"}]}`
		described   = `{"allOf": [{"$ref": "#/components/schemas/M"}], "description": "alias"}`
		bare        = `{"$ref": "#/components/schemas/M"}`
		bounded     = `{"$ref": "#/components/schemas/M", "maxLength": 5}`
	)
	tests := []struct {
		name         string
		baseA, baseM string
		headA, headM string
		want         []string
		via          []string
		breaking     bool
	}{
		{"keyword added", alias, plain, described, plain, []string{
			"POST /body › request body | model Body changed (through A)",
			"Body › f | model A changed",
			"A | description changed",
		}, []string{"A"}, false},
		{"keyword removed", described, plain, alias, plain, []string{
			"POST /body › request body | model Body changed (through A)",
			"Body › f | model A changed",
			"A | description changed",
		}, []string{"A"}, false},
		{"keyword added and model changed", alias, object, described, objectAfter, []string{
			"POST /body › request body | model Body changed (through A, M)",
			"Body › f | model A changed",
			"A | description changed",
			"A | model M changed",
			"M › a | type changed from string to integer",
		}, []string{"A", "M"}, true},
		{"keyword removed and model changed", described, object, alias, objectAfter, []string{
			"POST /body › request body | model Body changed (through A, M)",
			"Body › f | model A changed",
			"A | description changed",
			"A | model M changed",
			"M › a | type changed from string to integer",
		}, []string{"A", "M"}, true},
		{"bound added beside a $ref", bare, plain, bounded, plain, []string{
			"POST /body › request body | model Body changed (through A)",
			"Body › f | model A changed",
			"A | maxLength lowered from 20 to 5",
		}, []string{"A"}, true},
		{"bound removed beside a $ref", bounded, plain, bare, plain, []string{
			"POST /body › request body | model Body changed (through A)",
			"Body › f | model A changed",
			"A | maxLength raised from 5 to 20",
		}, []string{"A"}, false},
		{"bound added beside a $ref and model changed", bare, object, `{"$ref": "#/components/schemas/M", "minProperties": 1}`, objectAfter, []string{
			"POST /body › request body | model Body changed (through A, M)",
			"Body › f | model A changed",
			"A | minProperties added: 1",
			"A | model M changed",
			"M › a | type changed from string to integer",
		}, []string{"A", "M"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := Diff(bodySpec(t, tt.baseA, tt.baseM), bodySpec(t, tt.headA, tt.headM))
			require.Equal(t, tt.want, changeLines(r), r.Markdown())
			od := r.Operation("POST", "/body")
			require.NotNil(t, od)
			require.Len(t, od.Changes, 1)
			require.Equal(t, "Body", od.Changes[0].model)
			require.Equal(t, tt.via, od.Changes[0].via)
			require.Equal(t, tt.breaking, od.Changes[0].Breaking)
			use := r.Model("Body").Changes
			require.Len(t, use, 1)
			require.Equal(t, "A", use[0].model)
			require.Equal(t, tt.breaking, use[0].Breaking)
			require.Equal(t, tt.breaking, r.Model("A").Breaking)
		})
	}
}

// reqSpec is a 3.1 document whose POST /req takes Req, an object whose
// property a is the schema a, with the other component schemas given.
func reqSpec(t *testing.T, a, schemas string) *Document {
	t.Helper()
	doc, err := Parse([]byte(`{
  "openapi": "3.1.0",
  "info": {"title": "t", "version": "1"},
  "paths": {"/req": {"post": {
    "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Req"}}}},
    "responses": {"204": {"description": "none"}}
  }}},
  "components": {"schemas": {
    "Req": {"type": "object", "properties": {"a": ` + a + `}},
    ` + schemas + `
  }}
}`))
	require.NoError(t, err)
	return doc
}

// TestDiffNestedWrappers checks that single-member allOf wrappers nested
// around a reference, with keywords on each, refer to the model without
// expanding it: a change inside the model is reported once, in the model,
// and a change to a wrapper's own keyword where it is used.
func TestDiffNestedWrappers(t *testing.T) {
	const nested = `{"allOf": [{"allOf": [{"$ref": "#/components/schemas/T"}], "description": "%s"}], "readOnly": true}`
	const date = `"T": {"type": "string", "format": "date"}`
	const dateTime = `"T": {"type": "string", "format": "date-time"}`

	r := Diff(reqSpec(t, fmt.Sprintf(nested, "Legacy"), date), reqSpec(t, fmt.Sprintf(nested, "Legacy"), dateTime))
	require.Equal(t, []string{
		"POST /req › request body | model Req changed (through T)",
		"Req › a | model T changed",
		"T | format changed from date to date-time",
	}, changeLines(r))

	r = Diff(reqSpec(t, fmt.Sprintf(nested, "Legacy"), date), reqSpec(t, fmt.Sprintf(nested, "Old"), date))
	require.Equal(t, []string{
		"POST /req › request body | model Req changed",
		"Req › a | description changed",
	}, changeLines(r))
}

// TestDiffWrapperWithStructure checks that a single-member allOf with
// properties beside it is more than the model it refers to: its own rows
// are marked on the page, and a change to a reference to another model is
// not reported as a change of model.
func TestDiffWrapperWithStructure(t *testing.T) {
	const schemas = `"Base": {"type": "object", "properties": {"id": {"type": "string"}}},
    "A": {"type": "object", "properties": {"id": {"type": "string"}}},
    "B": {"type": "object", "properties": {"id": {"type": "string"}}}`
	base := reqSpec(t, `{"type": "string"}`, `"Old": {"allOf": [{"$ref": "#/components/schemas/Base"}], "properties": {"x": {"type": "string"}}}, `+schemas)
	head := reqSpec(t, `{"type": "string"}`, `"Old": {"allOf": [{"$ref": "#/components/schemas/Base"}], "properties": {"x": {"type": "integer"}, "y": {"type": "string"}}}, `+schemas)
	r := Diff(base, head)
	require.Equal(t, []string{
		"Old › x | type changed from string to integer",
		"Old › y | property added",
	}, changeLines(r))
	old := head.Components.Schemas.Map["Old"]
	x := r.marks.rows[old.Properties.Map["x"]]
	require.NotNil(t, x)
	require.NotNil(t, x.old, "the old row of x is shown")
	y := r.marks.rows[old.Properties.Map["y"]]
	require.NotNil(t, y)
	require.True(t, y.added)

	r = Diff(
		reqSpec(t, `{"allOf": [{"$ref": "#/components/schemas/A"}], "properties": {"x": {"type": "string"}}}`, schemas),
		reqSpec(t, `{"$ref": "#/components/schemas/B"}`, schemas),
	)
	require.Equal(t, []string{
		"POST /req › request body | model Req changed",
		"Req › a › x | property removed",
	}, changeLines(r))
}

// TestDiffTypeBesideVariants checks that a type written beside a reference
// to a model that is a oneOf and states no type, or beside an allOf of one,
// is no change, while a type removed from a schema without variants is.
func TestDiffTypeBesideVariants(t *testing.T) {
	const m = `"M": {"oneOf": [{"type": "object", "properties": {"a": {"type": "string"}}}, {"type": "object", "properties": {"b": {"type": "string"}}}]}`
	pairs := [][2]string{
		{`{"$ref": "#/components/schemas/M"}`, `{"allOf": [{"$ref": "#/components/schemas/M", "type": "object"}]}`},
		{`{"allOf": [{"$ref": "#/components/schemas/M"}, {"description": "d"}]}`, `{"allOf": [{"$ref": "#/components/schemas/M"}, {"description": "d"}], "type": "object"}`},
	}
	for _, pair := range pairs {
		for _, p := range [][2]string{pair, {pair[1], pair[0]}} {
			r := Diff(keywordSpecWith(t, "", p[0], m), keywordSpecWith(t, "", p[1], m))
			require.True(t, r.Empty(), "%s to %s: %s", p[0], p[1], r.Markdown())
		}
	}

	r := Diff(keywordSpec(t, "", `{"type": "string"}`), keywordSpec(t, "", `{}`))
	od := r.Operation("POST", "/things")
	require.NotNil(t, od)
	require.Equal(t, []Change{{Kind: Changed, Path: "POST /things › request body › name", Detail: "type changed from string to any", Breaking: false}}, changesAt(od, "POST /things › request body › name"))
	require.Equal(t, []Change{{Kind: Changed, Path: "POST /things › response 200 › name", Detail: "type changed from string to any", Breaking: true}}, changesAt(od, "POST /things › response 200 › name"))
}

// TestDiffSiteKeywordsWithModelChange checks what a use of a model reports
// when its own keywords and the model change together: the bound that held
// there before against the one that holds after, nothing when what holds
// there is the same, and the model's own change once, in the model.
func TestDiffSiteKeywordsWithModelChange(t *testing.T) {
	const (
		query    = "POST /things › query › q"
		request  = "POST /things › request body › name"
		response = "POST /things › response 200 › name"
	)
	tests := []struct {
		name                 string
		baseUse, baseModel   string
		headUse, headModel   string
		detail               string // "" when the use reports nothing of its own
		narrows, modelChange bool
	}{
		{"site and model", `{"$ref": "#/components/schemas/Code"}`, `{"type": "string", "maxLength": 3}`, `{"$ref": "#/components/schemas/Code", "maxLength": 5}`, `{"type": "string", "maxLength": 10}`, "maxLength raised from 3 to 5", false, true},
		{"model only", `{"$ref": "#/components/schemas/Code", "maxLength": 5}`, `{"type": "string", "maxLength": 10}`, `{"$ref": "#/components/schemas/Code", "maxLength": 5}`, `{"type": "string", "maxLength": 3}`, "", false, true},
		{"site only", `{"$ref": "#/components/schemas/Code", "maxLength": 5}`, `{"type": "string", "maxLength": 10}`, `{"$ref": "#/components/schemas/Code", "maxLength": 3}`, `{"type": "string", "maxLength": 10}`, "maxLength lowered from 5 to 3", true, false},
		{"site and model, same at the site", `{"$ref": "#/components/schemas/Code"}`, `{"type": "string", "description": "a"}`, `{"$ref": "#/components/schemas/Code", "description": "a"}`, `{"type": "string", "description": "b"}`, "", false, true},
		{"description at the site and in the model", `{"$ref": "#/components/schemas/Code"}`, `{"type": "string", "description": "a"}`, `{"$ref": "#/components/schemas/Code", "description": "c"}`, `{"type": "string", "description": "b"}`, "description changed", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := Diff(keywordSpecWith(t, "", tt.baseUse, `"Code": `+tt.baseModel), keywordSpecWith(t, "", tt.headUse, `"Code": `+tt.headModel))
			require.Equal(t, tt.modelChange, r.Model("Code") != nil, r.Markdown())
			od := r.Operation("POST", "/things")
			require.NotNil(t, od, r.Markdown())
			for _, path := range []string{query, request, response} {
				var want []Change
				if tt.detail != "" {
					want = append(want, Change{Kind: Changed, Path: path, Detail: tt.detail, Breaking: tt.narrows == (path != response) && tt.detail != "description changed"})
				}
				var got []Change
				for _, c := range changesAt(od, path) {
					if c.model == "" {
						got = append(got, c)
					}
				}
				require.Equal(t, want, got, path)
			}
		})
	}
	r := Diff(
		keywordSpecWith(t, "", `{"$ref": "#/components/schemas/Code"}`, `"Code": {"type": "string", "maxLength": 3}`),
		keywordSpecWith(t, "", `{"$ref": "#/components/schemas/Code", "maxLength": 5}`, `"Code": {"type": "string", "maxLength": 10}`),
	)
	require.Equal(t, []Change{{Kind: Changed, Path: "Code", Detail: "maxLength raised from 3 to 10", Breaking: true}}, r.Model("Code").Changes)
	require.Equal(t, "model Code changed", changesAt(r.Operation("POST", "/things"), request)[1].Detail)
}

// TestDiffMarkdownModelNames checks that the markdown summary writes as
// code exactly the model names a detail holds, even names that are part of
// the words around them or hold a backtick.
func TestDiffMarkdownModelNames(t *testing.T) {
	spec := func(desc, eMax, p string) *Document {
		doc, err := Parse([]byte(`{
  "openapi": "3.1.0",
  "info": {"title": "t", "version": "1"},
  "paths": {
    "/x": {"get": {"responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/del"}}}}}}},
    "/z": {"get": {"responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/e"}}}}}}}
  },
  "components": {"schemas": {
    "model": {"type": "object", "properties": {"x": {"type": "string", "description": "` + desc + `"}}},
    "del": {"type": "object", "properties": {"m": {"$ref": "#/components/schemas/model"}}},
    "e": {"type": "string"` + eMax + `},
    "a` + "`" + `b": {"type": "string"},
    "Req": {"type": "object", "properties": {"p": {"$ref": "#/components/schemas/` + p + `"}}}
  }}
}`))
		require.NoError(t, err)
		return doc
	}
	r := Diff(spec("old", "", "e"), spec("new", `, "maxLength": 3`, "a`b"))
	md := r.Markdown()
	require.Contains(t, md, "- `GET /x` › response 200: model `del` changed (through `model`)\n")
	require.Contains(t, md, "- `GET /z` › response 200: model `e` changed\n")
	require.Contains(t, md, "- `model` › x: description changed\n")
	require.Contains(t, md, "- `Req` › p: schema changed from `e` to ``a`b``\n")
	html := string(markdown(md))
	require.Contains(t, html, "model <code>del</code> changed (through <code>model</code>)")
	require.Contains(t, html, "model <code>e</code> changed")
	require.Contains(t, html, "schema changed from <code>e</code> to <code>a`b</code>")
}

// wrapperSpec writes a document of n models, each referring to leaf
// models through every spelling of a reference Diff does not expand, and n
// operations that take and return them, with the maxLength of every tenth
// leaf set to limit.
func wrapperSpec(n, limit int) []byte {
	leaves := max(n/5, 6)
	ref := func(i int) string {
		return fmt.Sprintf(`"#/components/schemas/L%d"`, i%leaves)
	}
	var schemas, paths []string
	for i := range leaves {
		name := `"name": {"type": "string"}`
		if i%10 == 0 {
			name = fmt.Sprintf(`"name": {"type": "string", "maxLength": %d}`, limit)
		}
		schemas = append(schemas, fmt.Sprintf(`"L%d": {"type": "object", "properties": {"id": {"type": "string"}, %s}}`, i, name))
	}
	for i := range n {
		props := []string{
			`"id": {"type": "string", "maxLength": 36}`,
			`"count": {"type": "integer", "minimum": 0}`,
			fmt.Sprintf(`"a": {"$ref": %s}`, ref(i+1)),
			fmt.Sprintf(`"b": {"$ref": %s, "description": "b"}`, ref(i+2)),
			fmt.Sprintf(`"c": {"allOf": [{"$ref": %s}], "nullable": true, "type": "object"}`, ref(i+3)),
			fmt.Sprintf(`"d": {"allOf": [{"$ref": %s, "description": "d"}], "description": "outer"}`, ref(i+4)),
			fmt.Sprintf(`"e": {"type": "array", "items": {"allOf": [{"$ref": %s}], "nullable": true}}`, ref(i+5)),
		}
		schemas = append(schemas, fmt.Sprintf(`"M%d": {"type": "object", "required": ["id"], "properties": {%s}}`, i, strings.Join(props, ", ")))
		paths = append(paths, fmt.Sprintf(`"/m%d": {"post": {
  "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/M%d", "description": "in"}}}},
  "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {"allOf": [{"$ref": "#/components/schemas/M%d"}], "nullable": true, "type": "object"}}}}}
}}`, i, i, (i+1)%n))
	}
	return []byte(`{"openapi": "3.0.3", "info": {"title": "t", "version": "1"}, "paths": {` + strings.Join(paths, ",\n") + `}, "components": {"schemas": {` + strings.Join(schemas, ",\n") + `}}}`)
}

// BenchmarkDiffWrapperReferences measures Diff on a document whose models
// refer to leaf models through references with keywords beside them and
// single-member allOf wrappers, where a tenth of the leaves changed.
func BenchmarkDiffWrapperReferences(b *testing.B) {
	base, err := Parse(wrapperSpec(500, 50))
	require.NoError(b, err)
	head, err := Parse(wrapperSpec(500, 40))
	require.NoError(b, err)
	own := 0
	for _, md := range Diff(base, head).Models {
		if !md.Changes[0].inherited() {
			own++
		}
	}
	require.Equal(b, 10, own)
	b.ResetTimer()
	for range b.N {
		Diff(base, head)
	}
}

// TestDiffNullableMemberOfAllOf checks that nullable beside a $ref in one
// member of an allOf of two makes the schema nullable only when the other
// member admits null too, while nullable beside a single-member allOf, the
// 3.0 spelling of a nullable reference, still makes it nullable.
func TestDiffNullableMemberOfAllOf(t *testing.T) {
	const user = `"User": {"type": "object", "properties": {"id": {"type": "string"}}}`
	spec := func(x string) *Document {
		return keywordSpecWith(t, "3.0.3", `{"$ref": "#/components/schemas/X"}`, user+`, "X": `+x)
	}

	// null fails the second member, so X never accepted null.
	typed := `{"type": "object", "properties": {"a": {"type": "string"}}}`
	r := Diff(
		spec(`{"allOf": [{"$ref": "#/components/schemas/User", "nullable": true}, `+typed+`]}`),
		spec(`{"allOf": [{"$ref": "#/components/schemas/User"}, `+typed+`]}`),
	)
	require.True(t, r.Empty(), r.Markdown())

	// A member that states no type lets null through, so X did accept it.
	untyped := `{"properties": {"a": {"type": "string"}}}`
	r = Diff(
		spec(`{"allOf": [{"$ref": "#/components/schemas/User", "nullable": true}, `+untyped+`]}`),
		spec(`{"allOf": [{"$ref": "#/components/schemas/User"}, `+untyped+`]}`),
	)
	require.Equal(t, []Change{{Kind: Changed, Path: "X", Detail: "no longer nullable", Breaking: true}}, r.Model("X").Changes)

	r = Diff(
		spec(`{"allOf": [{"$ref": "#/components/schemas/User"}], "nullable": true, "type": "object"}`),
		spec(`{"allOf": [{"$ref": "#/components/schemas/User"}], "type": "object"}`),
	)
	require.Equal(t, []Change{{Kind: Changed, Path: "X", Detail: "no longer nullable", Breaking: true}}, r.Model("X").Changes)
}

// TestDiffNullableWithoutType checks that nullable removed from a schema
// that then states no type, which lets null through anyway, is not
// breaking, as no type is inferred from its properties for that verdict.
func TestDiffNullableWithoutType(t *testing.T) {
	r := Diff(
		keywordSpec(t, "", `{"type": ["object", "null"], "properties": {"a": {}}}`),
		keywordSpec(t, "", `{"properties": {"a": {}}}`),
	)
	od := r.Operation("POST", "/things")
	require.NotNil(t, od, r.Markdown())
	for _, c := range od.Changes {
		require.Equal(t, "no longer nullable", c.Detail, c.Path)
		require.False(t, c.Breaking, c.Path)
	}
	require.Len(t, od.Changes, 3)

	r = Diff(keywordSpec(t, "", `{"type": ["string", "null"]}`), keywordSpec(t, "", `{"type": "string"}`))
	require.Equal(t, []Change{{Kind: Changed, Path: "POST /things › request body › name", Detail: "no longer nullable", Breaking: true}}, changesAt(r.Operation("POST", "/things"), "POST /things › request body › name"))
}

// directionPaths are where keywordSpec puts the schema, and whether a
// change there breaks when it narrows (a request) or when it widens.
var directionPaths = map[string]bool{
	"POST /things › query › q":           true,
	"POST /things › request body › name": true,
	"POST /things › response 200 › name": false,
}

// requireDirection checks that diffing base to head reports, at each of
// keywordSpec's three places, under that path followed by sub, exactly
// detail, breaking the request side when narrows is set and the response
// side otherwise, and that the reverse diff, which widens instead, reports
// rdetail with the opposite flags.
func requireDirection(t *testing.T, base, head, sub, detail, rdetail string) {
	t.Helper()
	for i, pair := range [][2]string{{base, head}, {head, base}} {
		r := Diff(keywordSpec(t, "", pair[0]), keywordSpec(t, "", pair[1]))
		od := r.Operation("POST", "/things")
		require.NotNil(t, od, "%s to %s", pair[0], pair[1])
		want := detail
		narrows := i == 0
		if !narrows {
			want = rdetail
		}
		for path, request := range directionPaths {
			at := path + sub
			if sub == "[]" && strings.HasSuffix(path, " q") {
				at = path + "[]"
			}
			require.Equal(t, []Change{{Kind: Changed, Path: at, Detail: want, Breaking: request == narrows}}, changesAt(od, at), "%s to %s\n%s", pair[0], pair[1], r.Markdown())
		}
		require.Len(t, od.Changes, len(directionPaths), r.Markdown())
	}
}

// TestDiffNarrowingFromNone checks that a keyword stated where there was
// none, a type, an enum or the schema of the items, narrows what a schema
// accepts, which breaks a request, and that dropping it widens, which
// breaks a response.
func TestDiffNarrowingFromNone(t *testing.T) {
	requireDirection(t, `{"type": "object"}`, `{"type": "object", "additionalProperties": {"type": "string"}}`, " › <key>", "type changed from any to string", "type changed from string to any")
	requireDirection(t, `{"minLength": 2}`, `{"type": "string", "minLength": 2}`, "", "type changed from any to string", "type changed from string to any")
	requireDirection(t, `{"type": "string"}`, `{"type": "string", "enum": ["a", "b"]}`, "", `enum added: "a", "b"`, `enum removed (was "a", "b")`)
	requireDirection(t, `{"type": "array"}`, `{"type": "array", "items": {"type": "string"}}`, "[]", "type changed from any to string", "type changed from string to any")
}

// TestDiffTypeBesideModelWithoutType checks that a type written beside a
// reference to a model that states none and has no variants is compared:
// it narrows what the use accepts.
func TestDiffTypeBesideModelWithoutType(t *testing.T) {
	const m = `"M": {"maxLength": 5}`
	r := Diff(keywordSpecWith(t, "", `{"$ref": "#/components/schemas/M"}`, m), keywordSpecWith(t, "", `{"$ref": "#/components/schemas/M", "type": "string"}`, m))
	od := r.Operation("POST", "/things")
	require.NotNil(t, od, r.Markdown())
	for path, request := range directionPaths {
		require.Equal(t, []Change{{Kind: Changed, Path: path, Detail: "type changed from any to string", Breaking: request}}, changesAt(od, path), r.Markdown())
	}
	require.Len(t, od.Changes, len(directionPaths))
	require.Nil(t, r.Model("M"))
}

// TestDiffRequiredWithoutProperty checks that a required name the schema
// declares no property for is reported, breaking a request, and that one no
// longer required breaks a response, which may now leave it out.
func TestDiffRequiredWithoutProperty(t *testing.T) {
	r := Diff(keywordSpec(t, "", `{"type": "object"}`), keywordSpec(t, "", `{"type": "object", "required": ["b"]}`))
	od := r.Operation("POST", "/things")
	require.NotNil(t, od, r.Markdown())
	for path, request := range directionPaths {
		require.Equal(t, []Change{{Kind: Changed, Path: path + " › b", Detail: "made required", Breaking: request}}, changesAt(od, path+" › b"), r.Markdown())
	}
	require.Len(t, od.Changes, len(directionPaths))

	r = Diff(keywordSpec(t, "", `{"type": "object", "required": ["b"]}`), keywordSpec(t, "", `{"type": "object"}`))
	od = r.Operation("POST", "/things")
	for path, request := range directionPaths {
		require.Equal(t, []Change{{Kind: Changed, Path: path + " › b", Detail: "made optional", Breaking: !request}}, changesAt(od, path+" › b"), r.Markdown())
	}
	require.Len(t, r.Changes, len(directionPaths))
}

// TestDiffIntegerBounds checks that integer bounds that admit the same
// integers, in any spelling, compare equal, and that a number schema with
// the same keywords still reports the change.
func TestDiffIntegerBounds(t *testing.T) {
	same := [][3]string{
		{"", `"minimum": 1`, `"exclusiveMinimum": 0`},
		{"3.0.3", `"minimum": 1`, `"minimum": 0, "exclusiveMinimum": true`},
		{"", `"maximum": 5`, `"exclusiveMaximum": 6`},
		{"3.0.3", `"maximum": 5`, `"maximum": 6, "exclusiveMaximum": true`},
		{"", `"minimum": 0.5`, `"minimum": 1`},
		{"", `"exclusiveMinimum": 0.5`, `"minimum": 1`},
		{"", `"maximum": -0.5`, `"exclusiveMaximum": 0`},
		{"", `"exclusiveMaximum": 2.5`, `"maximum": 2`},
	}
	for _, c := range same {
		for _, p := range [][2]string{{c[1], c[2]}, {c[2], c[1]}} {
			r := Diff(keywordSpec(t, c[0], `{"type": "integer", `+p[0]+`}`), keywordSpec(t, c[0], `{"type": "integer", `+p[1]+`}`))
			require.True(t, r.Empty(), "%s to %s: %s", p[0], p[1], r.Markdown())
			r = Diff(keywordSpec(t, c[0], `{"type": "number", `+p[0]+`}`), keywordSpec(t, c[0], `{"type": "number", `+p[1]+`}`))
			require.False(t, r.Empty(), "number %s to %s", p[0], p[1])
		}
	}

	// A real change keeps the keywords as written in its detail.
	r := Diff(keywordSpec(t, "", `{"type": "integer", "exclusiveMinimum": 0}`), keywordSpec(t, "", `{"type": "integer", "minimum": 2}`))
	require.Equal(t, []Change{{Kind: Changed, Path: "POST /things › request body › name", Detail: "minimum raised from 0 exclusive to 2", Breaking: true}}, changesAt(r.Operation("POST", "/things"), "POST /things › request body › name"))
}

// TestSameKeywordsEveryField checks that sameKeywords tells apart two
// schemas that differ in any one field a use of a model can set, every
// field of Schema but the structure, $ref, allOf and extensions, so a field
// added to Schema is not missed there.
func TestSameKeywordsEveryField(t *testing.T) {
	structural := map[string]bool{"Properties": true, "Required": true, "Items": true, "Enum": true, "OneOf": true, "AnyOf": true, "Not": true, "Discriminator": true, "AdditionalProperties": true}
	typ := reflect.TypeOf(Schema{})
	for _, i := range keywordFields {
		name := typ.Field(i).Name
		if structural[name] {
			continue
		}
		var s Schema
		v := reflect.ValueOf(&s).Elem().Field(i)
		switch v.Kind() {
		case reflect.String:
			v.SetString("x")
		case reflect.Bool:
			v.SetBool(true)
		case reflect.Pointer:
			v.Set(reflect.New(v.Type().Elem()))
		case reflect.Slice:
			v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		case reflect.Interface:
			v.Set(reflect.ValueOf("x"))
		default:
			t.Fatalf("no value for %s", name)
		}
		require.False(t, sameKeywords(&Schema{}, &s), name)
		require.False(t, sameKeywords(&s, &Schema{}), name)
		require.True(t, sameKeywords(&s, &s), name)
	}
}

// requireAt checks that diffing base to head reports, at each of
// keywordSpec's three places, under that path followed by sub, exactly
// detail, breaking a request as request says and a response as response
// says.
func requireAt(t *testing.T, base, head, sub, detail string, request, response bool) {
	t.Helper()
	r := Diff(keywordSpec(t, "", base), keywordSpec(t, "", head))
	od := r.Operation("POST", "/things")
	require.NotNil(t, od, "%s to %s", base, head)
	for path, isRequest := range directionPaths {
		breaking := response
		if isRequest {
			breaking = request
		}
		require.Equal(t, []Change{{Kind: Changed, Path: path + sub, Detail: detail, Breaking: breaking}}, changesAt(od, path+sub), "%s to %s\n%s", base, head, r.Markdown())
	}
	require.Len(t, od.Changes, len(directionPaths), r.Markdown())
}

// TestDiffTypeBesideVariantsNotStated checks that a type beside oneOf or
// anyOf variants that do not all state it is compared: it narrows the
// variants it leaves out, written inline or beside a reference to a model
// that is such a oneOf, while a type every variant states is no change.
func TestDiffTypeBesideVariantsNotStated(t *testing.T) {
	const variants = `"anyOf": [{"type": "string"}, {"type": "integer"}]`
	requireDirection(t, `{`+variants+`}`, `{"type": "string", `+variants+`}`, "", "type changed from any to string", "type changed from string to any")

	const m = `"M": {"oneOf": [{"type": "string"}, {"type": "integer"}]}`
	r := Diff(keywordSpecWith(t, "", `{"$ref": "#/components/schemas/M"}`, m), keywordSpecWith(t, "", `{"$ref": "#/components/schemas/M", "type": "string"}`, m))
	od := r.Operation("POST", "/things")
	require.NotNil(t, od, r.Markdown())
	for path, request := range directionPaths {
		require.Equal(t, []Change{{Kind: Changed, Path: path, Detail: "type changed from any to string", Breaking: request}}, changesAt(od, path), r.Markdown())
	}
	require.Len(t, od.Changes, len(directionPaths))

	const restated = `"anyOf": [{"type": "string"}, {"type": "string", "format": "date"}]`
	for _, p := range [][2]string{{`{` + restated + `}`, `{"type": "string", ` + restated + `}`}, {`{"type": "string", ` + restated + `}`, `{` + restated + `}`}} {
		r := Diff(keywordSpec(t, "", p[0]), keywordSpec(t, "", p[1]))
		require.True(t, r.Empty(), "%s to %s: %s", p[0], p[1], r.Markdown())
	}
}

// TestDiffRefuseAll checks that a not, and the false schema spelled as
// items or additionalProperties, are compared: a not added narrows, one
// dropped widens, and a different one breaks both, while the same one is no
// change.
func TestDiffRefuseAll(t *testing.T) {
	requireDirection(t, `{"type": "array"}`, `{"type": "array", "items": false}`, "", "items no longer allowed", "items allowed")
	requireDirection(t, `{"type": "array", "items": {"type": "string"}}`, `{"type": "array", "items": {"not": {}}}`, "", "items no longer allowed", "items allowed")
	requireAt(t, `{"type": "object"}`, `{"type": "object", "additionalProperties": {"not": {}}}`, "", "additional properties no longer allowed", true, false)
	requireAt(t, `{"type": "object"}`, `{"type": "object", "additionalProperties": false}`, "", "additional properties no longer allowed", true, false)
	requireDirection(t, `{"type": "string"}`, `{"type": "string", "not": {"enum": ["a"]}}`, "", "not added", "not removed")
	requireAt(t, `{"type": "object", "not": {"required": ["a"]}}`, `{"type": "object", "not": {"required": ["b"]}}`, "", "not changed", true, true)

	same := `{"type": "object", "not": {"required": ["a"]}}`
	r := Diff(keywordSpec(t, "", same), keywordSpec(t, "", same))
	require.True(t, r.Empty(), r.Markdown())
}

// TestDiffMadeOptional checks that a declared property no longer required
// breaks a response, which may now leave it out, and not a request.
func TestDiffMadeOptional(t *testing.T) {
	const props = `"type": "object", "properties": {"a": {"type": "string"}}`
	requireAt(t, `{`+props+`, "required": ["a"]}`, `{`+props+`}`, " › a", "made optional", false, true)
}

// requireVariants checks that diffing base to head, with the models in
// schemas, reports at each of keywordSpec's three places one change of
// variants reading detail, breaking a request as request says and a
// response as response says.
func requireVariants(t *testing.T, base, head, schemas, detail string, request, response bool) {
	t.Helper()
	r := Diff(keywordSpecWith(t, "", base, schemas), keywordSpecWith(t, "", head, schemas))
	od := r.Operation("POST", "/things")
	require.NotNil(t, od, "%s to %s", base, head)
	for path, isRequest := range directionPaths {
		want := response
		if isRequest {
			want = request
		}
		var got []Change
		for _, c := range changesAt(od, path) {
			if strings.Contains(c.Detail, "variants") {
				got = append(got, c)
			}
		}
		require.Equal(t, []Change{{Kind: Changed, Path: path, Detail: detail, Breaking: want}}, got, "%s to %s\n%s", base, head, r.Markdown())
	}
}

// TestDiffVariantLists checks how a change of oneOf or anyOf variants that
// are not compared by position breaks, judged from the variants as
// written: variants only added widen and only removed narrow; variants
// given in place of a schema they still hold widen, and dropped back to it
// narrow; variants given beside other keywords narrow and dropped widen;
// anyOf turned into oneOf with the same variants narrows and the reverse
// widens; anything else breaks both, as the diff cannot tell which way it
// moved.
func TestDiffVariantLists(t *testing.T) {
	const pet = `"Pet": {"type": "object", "properties": {"kind": {"type": "string"}}}, "B": {"type": "object"}`
	str := `{"type": "string"}`
	tests := []struct {
		base, head, detail string
		request, response  bool
	}{
		// a: one keyword on both sides, another length.
		{`{"oneOf": [{"type": "string"}, {"type": "integer"}, {"type": "boolean"}]}`, `{"oneOf": [{"type": "string"}, {"type": "integer"}]}`, "one of variants removed", true, false},
		{`{"oneOf": [{"type": "string"}, {"type": "integer"}]}`, `{"oneOf": [{"type": "string", "description": "s"}, {"type": "integer"}, {"type": "boolean"}]}`, "one of variants added", false, true},
		{`{"anyOf": [{"type": "string"}]}`, `{"anyOf": [{"type": "integer"}, {"type": "boolean"}]}`, "any of variants changed", true, true},
		{`{"anyOf": [{"type": "string"}, {"type": "integer"}]}`, `{"anyOf": [{"type": "boolean"}]}`, "any of variants changed", true, true},
		{`{"anyOf": [{"type": "string", "maxLength": 10}]}`, `{"anyOf": [{"type": "string", "maxLength": 2}, {"type": "integer"}]}`, "any of variants changed", true, true},
		{`{"type": "string", "enum": ["a", "b", "c"], "anyOf": [{"enum": ["a"]}, {"enum": ["b"]}]}`, `{"type": "string", "enum": ["a", "b", "c"], "anyOf": [{"enum": ["a"]}]}`, "any of variants removed", true, false},
		{`{"type": "number", "anyOf": [{"type": "integer"}, {"type": "number"}]}`, `{"type": "number", "anyOf": [{"type": "integer"}]}`, "any of variants removed", true, false},
		// b: a schema replaced by variants, or the reverse.
		{`{"type": "string", "enum": ["a", "b"]}`, `{"anyOf": [{"type": "string", "enum": ["a", "b"]}, {"type": "array", "items": {"type": "string", "enum": ["a", "b"]}}], "description": "one or more"}`, "any of variants given", false, true},
		{`{"anyOf": [{"type": "string", "enum": ["a", "b"]}, {"type": "array", "items": {"type": "string", "enum": ["a", "b"]}}]}`, `{"type": "string", "enum": ["a", "b"]}`, "any of variants dropped", true, false},
		{`{"$ref": "#/components/schemas/Pet"}`, `{"oneOf": [{"$ref": "#/components/schemas/Pet"}, {"$ref": "#/components/schemas/B"}]}`, "one of variants given", false, true},
		{`{"oneOf": [{"$ref": "#/components/schemas/Pet"}, {"$ref": "#/components/schemas/B"}]}`, `{"$ref": "#/components/schemas/Pet"}`, "one of variants dropped", true, false},
		{`{"type": "object", "properties": {"a": {"type": "string"}, "b": {"type": "string"}}}`, `{"oneOf": [{"type": "object", "properties": {"a": {"type": "string"}}, "required": ["a"]}, {"type": "object", "properties": {"b": {"type": "string"}}, "required": ["b"]}]}`, "one of variants given", true, true},
		{`{"type": "string", "maxLength": 5}`, `{"anyOf": [{"type": "string", "maxLength": 3}]}`, "any of variants given", true, true},
		{`{}`, `{"anyOf": [` + str + `]}`, "any of variants given", true, true},
		{`{"anyOf": [` + str + `]}`, `{}`, "any of variants dropped", true, true},
		// c: variants given beside a schema that stays, or dropped from one.
		{`{"type": "object"}`, `{"type": "object", "anyOf": [{"required": ["a"]}, {"required": ["b"]}]}`, "any of variants given", true, false},
		{`{"type": "object", "anyOf": [{"required": ["a"]}, {"required": ["b"]}]}`, `{"type": "object"}`, "any of variants dropped", false, true},
		{`{"type": "integer", "maximum": 100}`, `{"type": "integer", "maximum": 100, "anyOf": [{"$ref": "#/components/schemas/B"}, {"type": "boolean"}]}`, "any of variants given", true, false},
		{`{"$ref": "#/components/schemas/Pet"}`, `{"$ref": "#/components/schemas/Pet", "oneOf": [{"properties": {"kind": {"type": "string"}}, "required": ["meow"]}, {"properties": {"kind": {"type": "string"}}, "required": ["bark"]}]}`, "one of variants given", true, false},
		{`{"anyOf": [{"type": "string"}, {"type": "integer"}]}`, `{"anyOf": [{"type": "string"}, {"type": "integer"}], "oneOf": [{"minLength": 1}, {"maxLength": 0}]}`, "one of variants given", true, false},
		// d: oneOf and anyOf swapped.
		{`{"type": "string", "anyOf": [{"minLength": 1}, {"maxLength": 0}]}`, `{"type": "string", "oneOf": [{"maxLength": 0}, {"minLength": 1}]}`, "any of variants changed to one of", true, false},
		{`{"type": "string", "oneOf": [{"minLength": 1}, {"maxLength": 0}]}`, `{"type": "string", "anyOf": [{"minLength": 1}, {"maxLength": 0}]}`, "one of variants changed to any of", false, true},
		{`{"type": "string", "anyOf": [{"minLength": 1}, {"maxLength": 0}]}`, `{"type": "string", "oneOf": [{"minLength": 2}, {"maxLength": 0}]}`, "any of variants changed to one of", true, true},
	}
	for _, tt := range tests {
		requireVariants(t, tt.base, tt.head, pet, tt.detail, tt.request, tt.response)
	}

	// The keywords beside variants given keep their own flags.
	r := Diff(
		keywordSpec(t, "", `{"type": "object", "properties": {"a": {"type": "string"}}}`),
		keywordSpec(t, "", `{"type": "object", "properties": {"a": {"type": "string", "maxLength": 3}}, "anyOf": [{"properties": {"a": {"type": "string"}}}, {"properties": {"a": {"type": "string"}}, "required": ["b"]}]}`),
	)
	od := r.Operation("POST", "/things")
	require.NotNil(t, od, r.Markdown())
	for path, request := range directionPaths {
		require.Equal(t, []Change{{Kind: Changed, Path: path + " › a", Detail: "maxLength added: 3", Breaking: request}}, changesAt(od, path+" › a"), r.Markdown())
	}
}

// TestDiffVariantUses checks that the models variants refer to count as
// used where the variants are not compared by position, so a model that
// changed reaches the operation with its own flag.
func TestDiffVariantUses(t *testing.T) {
	spec := func(maxLength, body string) *Document {
		doc, err := Parse([]byte(`{"openapi": "3.1.0", "info": {"title": "t", "version": "1"},
  "paths": {"/r": {"post": {"requestBody": {"content": {"application/json": {"schema": ` + body + `}}}, "responses": {"200": {"description": "ok"}}}}},
  "components": {"schemas": {"A": {"type": "string", "maxLength": ` + maxLength + `}}}}`))
		require.NoError(t, err)
		return doc
	}
	const variant = `{"type": "object", "properties": {"x": {"$ref": "#/components/schemas/A"}}}`
	r := Diff(spec("5", `{"anyOf": [`+variant+`]}`), spec("3", `{"anyOf": [`+variant+`, {"type": "integer"}]}`))
	require.Equal(t, []Change{
		{Kind: Changed, Path: "POST /r › request body", Detail: "any of variants added", Breaking: false},
		{Kind: Changed, Path: "POST /r › request body", Detail: "model A changed", Breaking: true},
	}, publicChanges(r.Operation("POST", "/r").Changes))
}

// publicChanges drops the fields of changes that only the renderer reads.
func publicChanges(changes []Change) []Change {
	out := make([]Change, len(changes))
	for i, c := range changes {
		out[i] = Change{Kind: c.Kind, Path: c.Path, Detail: c.Detail, Breaking: c.Breaking}
	}
	return out
}

// TestDiffRecursiveSchemas checks that unions and nots that refer back to
// themselves, and a removed property whose schema refers back to its
// object, are compared and the comparison ends.
func TestDiffRecursiveSchemas(t *testing.T) {
	models := func(schemas string) *Document {
		doc, err := Parse([]byte(`{"openapi": "3.1.0", "info": {"title": "t", "version": "1"}, "paths": {}, "components": {"schemas": {` + schemas + `}}}`))
		require.NoError(t, err)
		return doc
	}
	pairs := [][2]string{
		{`"A": {"anyOf": [{"$ref": "#/components/schemas/B"}, {"type": "string"}]}, "B": {"anyOf": [{"$ref": "#/components/schemas/A"}, {"type": "integer"}]}`,
			`"A": {"anyOf": [{"$ref": "#/components/schemas/B"}, {"type": "string"}, {"type": "boolean"}]}, "B": {"anyOf": [{"$ref": "#/components/schemas/A"}, {"type": "integer"}]}`},
		{`"Pet": {"anyOf": [{"type": "string"}, {"$ref": "#/components/schemas/Pet"}]}`,
			`"Pet": {"anyOf": [{"type": "string"}, {"$ref": "#/components/schemas/Pet"}, {"type": "integer"}]}`},
		{`"A": {"$ref": "#/components/schemas/A"}`,
			`"A": {"oneOf": [{"$ref": "#/components/schemas/A"}, {"type": "object"}]}`},
		{`"A": {"not": {"$ref": "#/components/schemas/A"}}`,
			`"A": {"not": {"$ref": "#/components/schemas/A"}, "description": "d"}`},
		{`"A": {"type": "string", "not": {"$ref": "#/components/schemas/B"}}, "B": {"type": "string", "not": {"$ref": "#/components/schemas/A"}, "maxLength": 5}`,
			`"A": {"type": "string", "not": {"$ref": "#/components/schemas/B"}}, "B": {"type": "string", "not": {"$ref": "#/components/schemas/A"}, "maxLength": 3}`},
		{`"A": {"type": "object", "not": {"properties": {"a": {"not": {"$ref": "#/components/schemas/A"}}}}}`,
			`"A": {"type": "object", "not": {"properties": {"a": {"not": {"$ref": "#/components/schemas/A"}, "maxLength": 1}}}}`},
		{`"A": {"type": "object", "properties": {"self": {"$ref": "#/components/schemas/A"}, "x": {"type": "string"}}, "additionalProperties": {"$ref": "#/components/schemas/A"}}`,
			`"A": {"type": "object", "properties": {"x": {"type": "string"}}, "additionalProperties": {"$ref": "#/components/schemas/A"}}`},
		{`"A": {"type": "object", "properties": {"self": {"$ref": "#/components/schemas/A"}}, "additionalProperties": {"type": "object", "properties": {"in": {"$ref": "#/components/schemas/A"}}}}`,
			`"A": {"type": "object", "additionalProperties": {"type": "object", "properties": {"in": {"$ref": "#/components/schemas/A"}}}}`},
	}
	for _, p := range pairs {
		r := Diff(models(p[0]), models(p[1]))
		require.NotEmpty(t, r.Changes, p[1])
	}
}

// TestDiffModelUnderNot checks that a model referred to under a not that
// changed is reported where it is used, breaking the other way.
func TestDiffModelUnderNot(t *testing.T) {
	spec := func(maxLength string) *Document {
		return keywordSpecWith(t, "", `{"type": "string", "not": {"$ref": "#/components/schemas/A"}}`, `"A": {"type": "string", "maxLength": `+maxLength+`}`)
	}
	r := Diff(spec("3"), spec("2"))
	od := r.Operation("POST", "/things")
	require.NotNil(t, od, r.Markdown())
	for path, request := range directionPaths {
		require.Equal(t, []Change{{Kind: Changed, Path: path, Detail: "not changed: model A changed", Breaking: !request}}, publicChanges(changesAt(od, path)), r.Markdown())
	}
}

// TestDiffInfiniteBounds checks that a minimum of minus infinity and a
// maximum of plus infinity, which restrict nothing, count as no bound,
// while the infinities that do restrict are compared.
func TestDiffInfiniteBounds(t *testing.T) {
	spec := func(s string) *Document {
		doc, err := Parse([]byte(`openapi: 3.1.0
info:
  title: t
  version: "1"
paths:
  /x:
    post:
      requestBody:
        content:
          application/json:
            schema: ` + s + `
      responses:
        "200":
          description: ok
`))
		require.NoError(t, err)
		return doc
	}
	for _, p := range [][2]string{
		{`{type: number, minimum: -.inf}`, `{type: number}`},
		{`{type: number}`, `{type: number, maximum: .inf}`},
		{`{type: number, exclusiveMinimum: -.inf}`, `{type: number, exclusiveMaximum: .inf}`},
	} {
		r := Diff(spec(p[0]), spec(p[1]))
		require.True(t, r.Empty(), "%s to %s: %s", p[0], p[1], r.Markdown())
	}
	r := Diff(spec(`{type: number}`), spec(`{type: number, minimum: .inf}`))
	require.Equal(t, []string{"POST /x › request body | minimum added: +Inf"}, changeLines(r))
}

// opsSpec parses a 3.1 document with the paths and component schemas given.
func opsSpec(t *testing.T, paths, schemas string) *Document {
	t.Helper()
	doc, err := Parse([]byte(`{"openapi": "3.1.0", "info": {"title": "t", "version": "1"}, "paths": {` + paths + `}, "components": {"schemas": {` + schemas + `}}}`))
	require.NoError(t, err)
	return doc
}

// bodyPath is a POST /w operation whose request body is body, and
// respPath a GET /r operation whose response is resp.
func bodyPath(body string) string {
	return `"/w": {"post": {"requestBody": {"content": {"application/json": {"schema": ` + body + `}}}, "responses": {"200": {"description": "ok"}}}}`
}

func respPath(resp string) string {
	return `"/r": {"get": {"responses": {"200": {"description": "ok", "content": {"application/json": {"schema": ` + resp + `}}}}}}`
}

// TestDiffVariantUsesKeptOnly checks that only the models that variants
// kept on both sides refer to count as used where variants are compared as
// written: a variant added or removed is judged by the variants change, and
// the change of a model only it refers to is not a change of the operation.
func TestDiffVariantUsesKeptOnly(t *testing.T) {
	const a = `"A": {"type": "object", "properties": {"a": {"type": "string"}}}, "C": {"type": "object", "properties": {"c": {"type": "string"}}}`
	b := func(maxLength string) string {
		return `"B": {"type": "object", "properties": {"t": {"type": "string", "maxLength": ` + maxLength + `}}}`
	}
	ref := func(name string) string {
		return `{"$ref": "#/components/schemas/` + name + `"}`
	}
	other := `"/other": {"get": {"responses": {"200": {"description": "ok", "content": {"application/json": {"schema": ` + ref("B") + `}}}}}}`
	tests := []struct {
		base, head string
		want       []Change
	}{
		{`{"oneOf": [` + ref("A") + `, ` + ref("C") + `]}`, `{"oneOf": [` + ref("A") + `, ` + ref("C") + `, ` + ref("B") + `]}`,
			[]Change{{Kind: Changed, Path: "POST /w › request body", Detail: "one of variants added", Breaking: false}}},
		{ref("A"), `{"oneOf": [` + ref("A") + `, ` + ref("B") + `]}`,
			[]Change{
				{Kind: Changed, Path: "POST /w › request body", Detail: "type changed from object to any", Breaking: false},
				{Kind: Removed, Path: "POST /w › request body › a", Detail: "property removed", Breaking: false},
				{Kind: Changed, Path: "POST /w › request body", Detail: "one of variants given", Breaking: false},
			}},
	}
	for _, tt := range tests {
		r := Diff(opsSpec(t, bodyPath(tt.base)+", "+other, a+", "+b("9")), opsSpec(t, bodyPath(tt.head)+", "+other, a+", "+b("4")))
		require.Equal(t, tt.want, publicChanges(r.Operation("POST", "/w").Changes), r.Markdown())
		require.Equal(t, []Change{{Kind: Changed, Path: "B › t", Detail: "maxLength lowered from 9 to 4", Breaking: false}}, publicChanges(r.Model("B").Changes), r.Markdown())
	}

	m := func(maxLength string) string {
		return `"M": {"type": "string", "maxLength": ` + maxLength + `}`
	}
	r := Diff(opsSpec(t, respPath(`{"oneOf": [{"type": "integer"}, `+ref("M")+`]}`), m("5")), opsSpec(t, respPath(`{"oneOf": [{"type": "integer"}]}`), m("10")))
	require.Equal(t, []Change{{Kind: Changed, Path: "GET /r › response 200", Detail: "one of variants removed", Breaking: false}}, publicChanges(r.Operation("GET", "/r").Changes), r.Markdown())
}

// TestDiffNotParity checks that a model referred to under nots inside
// variants compared as written is reported under the parity of the nots on
// its path: under one, as not changed, breaking the other way, and under
// two as the model itself.
func TestDiffNotParity(t *testing.T) {
	m := func(maxLength string) string {
		return `"M": {"type": "string", "maxLength": ` + maxLength + `}`
	}
	const notM = `{"not": {"$ref": "#/components/schemas/M"}}`
	r := Diff(
		opsSpec(t, bodyPath(`{"anyOf": [`+notM+`, {"type": "boolean"}]}`), m("5")),
		opsSpec(t, bodyPath(`{"anyOf": [`+notM+`, {"type": "boolean"}, {"type": "integer"}]}`), m("10")),
	)
	require.Contains(t, publicChanges(r.Operation("POST", "/w").Changes), Change{Kind: Changed, Path: "POST /w › request body", Detail: "not changed: model M changed", Breaking: true}, r.Markdown())

	r = Diff(
		opsSpec(t, respPath(`{"not": {"anyOf": [`+notM+`, {"type": "boolean"}]}}`), m("5")),
		opsSpec(t, respPath(`{"not": {"anyOf": [`+notM+`, {"type": "boolean"}, {"type": "integer"}]}}`), m("10")),
	)
	require.Contains(t, publicChanges(r.Operation("GET", "/r").Changes), Change{Kind: Changed, Path: "GET /r › response 200", Detail: "model M changed", Breaking: true}, r.Markdown())
}

// TestDiffNotRegressions checks two lists whose variants are the same as
// written, one of them listed more or fewer times, which break nothing
// themselves, while the model under a not in them changed: the model's
// change, turned around by the not, breaks.
func TestDiffNotRegressions(t *testing.T) {
	m3 := func(enum string) string {
		return `"M3": {"type": "string", "enum": [` + enum + `]}`
	}
	const notM3 = `{"not": {"$ref": "#/components/schemas/M3"}}`
	tests := []struct {
		base, head, baseEnum, headEnum string
		request, response              bool
	}{
		{`{"allOf": [{"maxLength": 4}, {"anyOf": [` + notM3 + `, {"not": {"type": "string"}}]}]}`,
			`{"allOf": [{"maxLength": 4}, {"anyOf": [` + notM3 + `, {"not": {"type": "string"}}, ` + notM3 + `]}]}`,
			`"a", "b"`, `"a"`, false, true},
		{`{"anyOf": [` + notM3 + `, ` + notM3 + `]}`, `{"anyOf": [` + notM3 + `]}`,
			`"a", "b"`, `"a", "b", "c"`, true, false},
	}
	for _, tt := range tests {
		for _, request := range []bool{true, false} {
			path := bodyPath
			op := "POST /w"
			at := "POST /w › request body"
			want := tt.request
			if !request {
				path = respPath
				op = "GET /r"
				at = "GET /r › response 200"
				want = tt.response
			}
			r := Diff(opsSpec(t, path(tt.base), m3(tt.baseEnum)), opsSpec(t, path(tt.head), m3(tt.headEnum)))
			method, route, _ := strings.Cut(op, " ")
			require.Equal(t, []Change{
				{Kind: Changed, Path: at, Detail: "any of variants changed", Breaking: false},
				{Kind: Changed, Path: at, Detail: "not changed: model M3 changed", Breaking: want},
			}, publicChanges(r.Operation(method, route).Changes), r.Markdown())
			require.Equal(t, want, r.Model("M3").Changes[0].Breaking, r.Markdown())
		}
	}
}

// TestDiffRepeatedVariants checks that lists of the same variants as
// written, one listed more or fewer times, read as changed and break
// nothing.
func TestDiffRepeatedVariants(t *testing.T) {
	for _, keyword := range []string{"anyOf", "oneOf"} {
		label := map[string]string{"anyOf": "any of", "oneOf": "one of"}[keyword]
		long := `{"` + keyword + `": [{"type": "string"}, {"type": "string"}, {"type": "integer"}]}`
		short := `{"` + keyword + `": [{"type": "string"}, {"type": "integer"}]}`
		requireVariants(t, long, short, `"B": {}`, label+" variants changed", false, false)
		requireVariants(t, short, long, `"B": {}`, label+" variants changed", false, false)
	}
}

// TestDiffNullableBesideVariants checks that nullable, written or folded
// from a {type: null} variant, is not a keyword beside variants: a schema
// replaced by variants that still hold it widens, and the null is reported
// on its own.
func TestDiffNullableBesideVariants(t *testing.T) {
	const models = `"A": {"type": "object", "properties": {"a": {"type": "string"}}}, "B": {"type": "object", "properties": {"b": {"type": "string"}}}`
	for _, tt := range []struct{ version, base, head, label string }{
		{"3.1.0", `{"$ref": "#/components/schemas/A"}`, `{"anyOf": [{"$ref": "#/components/schemas/A"}, {"$ref": "#/components/schemas/B"}, {"type": "null"}]}`, "any of"},
		{"3.0.3", `{"$ref": "#/components/schemas/A"}`, `{"oneOf": [{"$ref": "#/components/schemas/A"}, {"$ref": "#/components/schemas/B"}], "nullable": true}`, "one of"},
	} {
		r := Diff(keywordSpecWith(t, tt.version, tt.base, models), keywordSpecWith(t, tt.version, tt.head, models))
		od := r.Operation("POST", "/things")
		require.NotNil(t, od, r.Markdown())
		for path, request := range directionPaths {
			var got []Change
			for _, c := range changesAt(od, path) {
				if strings.Contains(c.Detail, "variants") || strings.Contains(c.Detail, "nullable") {
					got = append(got, c)
				}
			}
			require.Equal(t, []Change{
				{Kind: Changed, Path: path, Detail: "made nullable", Breaking: !request},
				{Kind: Changed, Path: path, Detail: tt.label + " variants given", Breaking: !request},
			}, got, "%s %s", tt.version, r.Markdown())
		}
	}
}

// TestDiffStrictUnderNot checks that a model referred to under a not is
// judged strictly there, as the schema under a not written in place is:
// enum values added to it narrow the use, so the use and the model's own
// change break a request.
func TestDiffStrictUnderNot(t *testing.T) {
	m := func(enum string) string {
		return `"M": {"type": "string", "enum": [` + enum + `]}`
	}
	const s = `{"type": "string", "not": {"$ref": "#/components/schemas/M"}}`
	r := Diff(opsSpec(t, bodyPath(s), m(`"a"`)), opsSpec(t, bodyPath(s), m(`"a", "b"`)))
	require.Equal(t, []Change{{Kind: Changed, Path: "POST /w › request body", Detail: "not changed: model M changed", Breaking: true}}, publicChanges(r.Operation("POST", "/w").Changes), r.Markdown())
	require.Equal(t, []Change{{Kind: Changed, Path: "M", Detail: `enum values added: "b"`, Breaking: true}}, publicChanges(r.Model("M").Changes), r.Markdown())

	r = Diff(opsSpec(t, respPath(s), m(`"a"`)), opsSpec(t, respPath(s), m(`"a", "b"`)))
	require.Equal(t, []Change{{Kind: Changed, Path: "GET /r › response 200", Detail: "not changed: model M changed", Breaking: false}}, publicChanges(r.Operation("GET", "/r").Changes), r.Markdown())
	require.Equal(t, []Change{{Kind: Changed, Path: "M", Detail: `enum values added: "b"`, Breaking: false}}, publicChanges(r.Model("M").Changes), r.Markdown())
}

// diffWithin returns Diff(base, head), failing t when it takes more than a
// minute, as a comparison that does not end would. The inputs take a few
// seconds at most under the race detector on a slow machine.
func diffWithin(t *testing.T, base, head *Document) *Report {
	t.Helper()
	done := make(chan *Report, 1)
	go func() {
		done <- Diff(base, head)
	}()
	select {
	case r := <-done:
		return r
	case <-time.After(time.Minute):
		t.Fatal("Diff did not return")
		return nil
	}
}

// TestDiffSelfReferenceThroughAbsentKeyword checks that a comparison ends
// where a component becomes another name for a model that refers back to
// it through a keyword absent on the other side, which is compared with
// the empty schema, for each keyword and with dangling references on either
// side.
func TestDiffSelfReferenceThroughAbsentKeyword(t *testing.T) {
	ref := func(name string) string {
		return `{"$ref": "#/components/schemas/` + name + `"}`
	}
	backs := []string{
		`"B": {"type": "object", "additionalProperties": ` + ref("A") + `}`,
		`"B": {"type": "array", "items": ` + ref("A") + `}`,
		`"B": {"type": "object", "properties": {"p": ` + ref("A") + `}}`,
		`"B": {"allOf": [` + ref("A") + `, {"type": "object", "additionalProperties": ` + ref("A") + `}]}`,
		`"B": {"type": "object", "not": {"type": "object", "additionalProperties": ` + ref("A") + `}}`,
		`"B": {"anyOf": [` + ref("A") + `, {"type": "object", "additionalProperties": ` + ref("A") + `}]}`,
		`"C": ` + ref("B") + `, "B": {"type": "object", "additionalProperties": ` + ref("C") + `}`,
		`"B": {"type": "object", "additionalProperties": ` + ref("A") + `, "properties": {"m": ` + ref("Missing") + `}}`,
	}
	bases := []string{`{"type": "object"}`, `{"type": "string"}`, ref("Missing"), `{"type": "array"}`}
	for _, back := range backs {
		for _, base := range bases {
			b := opsSpec(t, "", `"A": `+base)
			h := opsSpec(t, "", `"A": `+ref("B")+`, `+back)
			require.NotEmpty(t, diffWithin(t, b, h).Changes, back)
			require.NotEmpty(t, diffWithin(t, h, b).Changes, back)
		}
	}

	r := diffWithin(t, opsSpec(t, "", `"A": {"type": "object"}`), opsSpec(t, "", `"A": `+ref("B")+`, `+backs[0]))
	require.Equal(t, []string{"A › <key> | type changed from any to object", "B | model added"}, changeLines(r))
}

// TestDiffFoldedNullUnion checks that a 3.1 union of one schema and
// {type: null}, which Parse folds into that schema made nullable, is
// compared as the one-variant list it was written as against variants on
// the other side, as the 3.0 spelling is, with the null reported on its
// own.
func TestDiffFoldedNullUnion(t *testing.T) {
	const s = `{"type": "string"}`
	const i = `{"type": "integer"}`
	const null = `{"type": "null"}`
	const models = `"A": {"type": "object", "properties": {"a": {"type": "string"}}}, "B": {"type": "object"}`
	const a = `{"$ref": "#/components/schemas/A"}`
	const b = `{"$ref": "#/components/schemas/B"}`
	tests := []struct {
		base, head, detail string
		request, response  bool
	}{
		{`{"anyOf": [` + s + `, ` + null + `]}`, `{"anyOf": [` + s + `, ` + i + `, ` + null + `]}`, "any of variants added", false, true},
		{`{"anyOf": [` + s + `, ` + i + `, ` + null + `]}`, `{"anyOf": [` + s + `, ` + null + `]}`, "any of variants removed", true, false},
		{`{"oneOf": [` + s + `, ` + null + `]}`, `{"oneOf": [` + s + `, ` + i + `, ` + null + `]}`, "one of variants added", false, true},
		{`{"anyOf": [` + a + `, ` + null + `]}`, `{"anyOf": [` + a + `, ` + b + `, ` + null + `]}`, "any of variants added", false, true},
		{`{"anyOf": [` + s + `, ` + null + `]}`, `{"anyOf": [` + s + `]}`, "no longer nullable", true, false},
		{`{"anyOf": [` + s + `]}`, `{"anyOf": [` + s + `, ` + null + `]}`, "made nullable", false, true},
	}
	for _, tt := range tests {
		r := Diff(keywordSpecWith(t, "", tt.base, models), keywordSpecWith(t, "", tt.head, models))
		od := r.Operation("POST", "/things")
		require.NotNil(t, od, r.Markdown())
		for path, isRequest := range directionPaths {
			want := tt.response
			if isRequest {
				want = tt.request
			}
			require.Equal(t, []Change{{Kind: Changed, Path: path, Detail: tt.detail, Breaking: want}}, publicChanges(changesAt(od, path)), "%s to %s\n%s", tt.base, tt.head, r.Markdown())
		}
	}
}

// TestDiffStrictUnderTwoNots checks that a model under two nots is judged
// strictly, with its flags as they are, while the same schema written in
// place under two nots is compared as written, so its change breaks both.
func TestDiffStrictUnderTwoNots(t *testing.T) {
	m := func(enum string) string {
		return `"M": {"type": "string", "enum": [` + enum + `]}`
	}
	const s = `{"type": "string", "not": {"not": {"$ref": "#/components/schemas/M"}}}`
	r := Diff(keywordSpecWith(t, "", s, m(`"a"`)), keywordSpecWith(t, "", s, m(`"a", "b"`)))
	od := r.Operation("POST", "/things")
	require.NotNil(t, od, r.Markdown())
	for path, request := range directionPaths {
		require.Equal(t, []Change{{Kind: Changed, Path: path, Detail: "model M changed", Breaking: !request}}, publicChanges(changesAt(od, path)), r.Markdown())
	}

	inline := func(enum string) string {
		return `{"type": "string", "not": {"not": {"type": "string", "enum": [` + enum + `]}}}`
	}
	requireAt(t, inline(`"a"`), inline(`"a", "b"`), "", "not changed", true, true)
}

// TestDiffThroughListCapped checks that a model reached through more than
// maxVia changed models names the first maxVia of them and counts the
// others.
func TestDiffThroughListCapped(t *testing.T) {
	spec := func(maxLength int) *Document {
		var props, models []string
		for i := range maxVia + 10 {
			props = append(props, fmt.Sprintf(`"p%03d": {"$ref": "#/components/schemas/M%03d"}`, i, i))
			models = append(models, fmt.Sprintf(`"M%03d": {"type": "string", "maxLength": %d}`, i, maxLength))
		}
		models = append(models, `"R": {"type": "object", "properties": {`+strings.Join(props, ", ")+`}}`)
		return opsSpec(t, respPath(`{"$ref": "#/components/schemas/R"}`), strings.Join(models, ", "))
	}
	r := Diff(spec(5), spec(10))
	changes := r.Operation("GET", "/r").Changes
	require.Len(t, changes, 1)
	require.True(t, strings.HasPrefix(changes[0].Detail, "model R changed (through M000, M001, "), changes[0].Detail)
	require.True(t, strings.HasSuffix(changes[0].Detail, ", M049, and 10 more)"), changes[0].Detail)
	require.Contains(t, r.Markdown(), "`M049`, and 10 more)")
}

// TestDiffPropertyRemovedFromClosedObject checks that a property removed
// from an object that allows no other properties breaks a request, which
// may still send it, as well as a response, while one removed from an open
// object breaks only a response.
func TestDiffPropertyRemovedFromClosedObject(t *testing.T) {
	const props = `"type": "object", "properties": {"a": {"type": "string"}, "b": {"type": "string"}}`
	const head = `"type": "object", "properties": {"a": {"type": "string"}}`
	for _, closed := range []bool{true, false} {
		extra := ""
		if closed {
			extra = `, "additionalProperties": false`
		}
		r := Diff(keywordSpec(t, "", `{`+props+extra+`}`), keywordSpec(t, "", `{`+head+extra+`}`))
		od := r.Operation("POST", "/things")
		require.NotNil(t, od, r.Markdown())
		for path, request := range directionPaths {
			require.Equal(t, []Change{{Kind: Removed, Path: path + " › b", Detail: "property removed", Breaking: closed || !request}}, changesAt(od, path+" › b"), r.Markdown())
		}
		require.Len(t, od.Changes, len(directionPaths), r.Markdown())
	}
}

// TestDiffBoundsTypeChange checks that a bound compared across a change
// between integer and number states the bound that held for integers, so
// the detail does not contradict its numbers.
func TestDiffBoundsTypeChange(t *testing.T) {
	const request = "POST /things › request body › name"
	tests := [][3]string{
		{`{"type": "integer", "minimum": 0.5}`, `{"type": "number", "minimum": 0.7}`, "minimum lowered from 0.5 (1 for integers) to 0.7"},
		{`{"type": "integer", "maximum": 2.5}`, `{"type": "number", "maximum": 2.2}`, "maximum raised from 2.5 (2 for integers) to 2.2"},
		{`{"type": "number", "maximum": 2.2}`, `{"type": "integer", "maximum": 2.5}`, "maximum lowered from 2.2 to 2.5 (2 for integers)"},
	}
	for _, tt := range tests {
		r := Diff(keywordSpec(t, "", tt[0]), keywordSpec(t, "", tt[1]))
		var details []string
		for _, c := range changesAt(r.Operation("POST", "/things"), request) {
			details = append(details, c.Detail)
		}
		require.Contains(t, details, tt[2], r.Markdown())
	}
}

// TestDiffTypeOnlyNull checks that a type of only null is read as the type
// null and nullable: given where there was no type it narrows the schema to
// null, beside a reference it makes the use nullable, and an allOf of it and
// a string flattens to a nullable string in either order.
func TestDiffTypeOnlyNull(t *testing.T) {
	r := Diff(keywordSpec(t, "", `{}`), keywordSpec(t, "", `{"type": "null"}`))
	od := r.Operation("POST", "/things")
	require.NotNil(t, od, r.Markdown())
	for path, request := range directionPaths {
		require.Equal(t, []Change{
			{Kind: Changed, Path: path, Detail: "type changed from any to null", Breaking: request},
			{Kind: Changed, Path: path, Detail: "made nullable", Breaking: false},
		}, changesAt(od, path), r.Markdown())
	}

	const m = `"M": {"type": "string"}`
	r = Diff(keywordSpecWith(t, "", `{"$ref": "#/components/schemas/M"}`, m), keywordSpecWith(t, "", `{"$ref": "#/components/schemas/M", "type": "null"}`, m))
	od = r.Operation("POST", "/things")
	require.NotNil(t, od, r.Markdown())
	for path, request := range directionPaths {
		require.Equal(t, []Change{{Kind: Changed, Path: path, Detail: "made nullable", Breaking: !request}}, changesAt(od, path), r.Markdown())
	}

	for _, x := range []string{
		`{"allOf": [{"type": "null"}, {"type": "string"}]}`,
		`{"allOf": [{"type": "string"}, {"type": "null"}]}`,
	} {
		doc, err := Parse([]byte(`{"openapi": "3.1.0", "info": {"title": "t", "version": "1"}, "paths": {}, "components": {"schemas": {"X": ` + x + `}}}`))
		require.NoError(t, err)
		s, _ := doc.Components.Schemas.Get("X")
		f := doc.Flatten(s)
		require.Equal(t, TypeList{"string"}, f.Type, x)
		require.True(t, f.Nullable, x)
	}
}

// TestDiffTypeBesideNullableVariants checks that a type beside variants
// that let null through, where the type does not, narrows what the schema
// accepts, in 3.1 and 3.0 spellings and beside a reference to a model that
// is such an anyOf, while number beside integer variants restates them.
func TestDiffTypeBesideNullableVariants(t *testing.T) {
	cases := [][3]string{
		{"", `{"anyOf": [{"type": ["string", "null"]}]}`, `{"type": "string", "anyOf": [{"type": ["string", "null"]}]}`},
		{"3.0.3", `{"anyOf": [{"type": "string", "nullable": true}]}`, `{"type": "string", "anyOf": [{"type": "string", "nullable": true}]}`},
	}
	for _, c := range cases {
		r := Diff(keywordSpec(t, c[0], c[1]), keywordSpec(t, c[0], c[2]))
		od := r.Operation("POST", "/things")
		require.NotNil(t, od, "%s to %s", c[1], c[2])
		for path, request := range directionPaths {
			require.Equal(t, []Change{{Kind: Changed, Path: path, Detail: "type changed from any to string", Breaking: request}}, changesAt(od, path), r.Markdown())
		}
	}

	const m = `"M": {"anyOf": [{"type": "string", "nullable": true}]}`
	r := Diff(keywordSpecWith(t, "3.0.3", `{"$ref": "#/components/schemas/M"}`, m), keywordSpecWith(t, "3.0.3", `{"$ref": "#/components/schemas/M", "type": "string"}`, m))
	od := r.Operation("POST", "/things")
	require.NotNil(t, od, r.Markdown())
	for path, request := range directionPaths {
		require.Equal(t, []Change{{Kind: Changed, Path: path, Detail: "type changed from any to string", Breaking: request}}, changesAt(od, path), r.Markdown())
	}

	const ints = `"anyOf": [{"type": "integer"}, {"type": "integer", "minimum": 5}]`
	r = Diff(keywordSpec(t, "", `{`+ints+`}`), keywordSpec(t, "", `{"type": "number", `+ints+`}`))
	require.True(t, r.Empty(), r.Markdown())
}

// TestDiffNotAsWritten checks that the schemas under two nots are compared
// as written: the same, they report nothing, also where only an extension
// differs; different only in annotations, not changed breaks neither; and
// different otherwise, not changed breaks both, a $ref renamed included,
// as are a not nested in the not and one in a variant.
func TestDiffNotAsWritten(t *testing.T) {
	for _, s := range []string{
		`{"type": "string", "not": {"enum": ["x"]}}`,
		`{"type": "object", "not": {"properties": {"a": {"not": {"$ref": "#/components/schemas/Code"}}}}}`,
	} {
		r := Diff(keywordSpec(t, "", s), keywordSpec(t, "", s))
		require.True(t, r.Empty(), r.Markdown())
	}
	r := Diff(keywordSpec(t, "", `{"type": "object", "not": {"enum": ["x"], "x-a": 1}}`), keywordSpec(t, "", `{"type": "object", "not": {"enum": ["x"], "x-a": 2}}`))
	require.True(t, r.Empty(), r.Markdown())

	requireAt(t, `{"type": "object", "not": {"enum": ["x"], "description": "one"}}`, `{"type": "object", "not": {"enum": ["x"], "description": "two"}}`, "", "not changed", false, false)
	requireAt(t, `{"type": "object", "not": {"enum": ["x"], "title": "one"}}`, `{"type": "object", "not": {"enum": ["x"], "title": "two", "examples": ["x"]}}`, "", "not changed", false, false)
	for _, pair := range [][2]string{
		{`{"type": "string", "not": {"enum": ["x"]}}`, `{"type": "string", "not": {"enum": ["x", "y"]}}`},
		{`{"type": "string", "not": {"type": "string", "maxLength": 3}}`, `{"type": "string", "not": {"type": "string", "maxLength": 5}}`},
		{`{"type": "string", "not": {"type": "string"}}`, `{"type": "string", "not": {"type": "string", "format": "date"}}`},
		{`{"type": "string", "not": {"enum": ["x"]}}`, `{"type": "string", "not": {"enum": ["y"]}}`},
		{`{"type": "object", "not": {"required": ["a"]}}`, `{"type": "object", "not": {"required": ["b"]}}`},
		{`{"type": "string", "not": {"$ref": "#/components/schemas/Code"}}`, `{"type": "string", "not": {"$ref": "#/components/schemas/Code2"}}`},
		{`{"type": "string", "not": {"not": {"enum": ["a"]}}}`, `{"type": "string", "not": {"not": {"enum": ["a", "b"]}}}`},
	} {
		requireAt(t, pair[0], pair[1], "", "not changed", true, true)
		requireAt(t, pair[1], pair[0], "", "not changed", true, true)
	}
	requireAt(t, `{"anyOf": [{"not": {"enum": ["x"]}}, {"type": "integer"}]}`, `{"anyOf": [{"not": {"enum": ["x", "y"]}}, {"type": "integer"}]}`, " › any of #1", "not changed", true, true)
}

// TestDiffNotInVariantLikeEmma checks the shape of the nots in Emma's
// document, a not of a required name in a oneOf variant: the same, it
// reports nothing, and with the name changed, not changed breaks both.
func TestDiffNotInVariantLikeEmma(t *testing.T) {
	spec := func(name string) string {
		return `{"oneOf": [{"type": "object", "properties": {"space_id": {"type": "string"}}, "required": ["space_id"]}, {"type": "object", "properties": {"space_id": {"type": "string"}, "unit_id": {"type": "string"}}, "required": ["unit_id"], "not": {"required": ["` + name + `"]}}]}`
	}
	r := Diff(keywordSpec(t, "", spec("space_id")), keywordSpec(t, "", spec("space_id")))
	require.True(t, r.Empty(), r.Markdown())
	requireAt(t, spec("space_id"), spec("space"), " › one of #2", "not changed", true, true)
}

// TestDiffNotInAllOfMember checks that a not on an allOf member is compared.
func TestDiffNotInAllOfMember(t *testing.T) {
	requireAt(t, `{"allOf": [{"type": "string"}, {"not": {"enum": ["x"]}}]}`, `{"allOf": [{"type": "string"}, {"not": {"enum": ["x", "y"]}}]}`, "", "not changed", true, true)
	requireDirection(t, `{"allOf": [{"type": "string"}, {}]}`, `{"allOf": [{"type": "string"}, {"not": {"enum": ["x"]}}]}`, "", "not added", "not removed")
}

// TestDiffSpellingsOfAnything checks that spellings of one schema read the
// same: additionalProperties false and {not: {}}, absent and {}, and
// {not: {}} with an annotation beside it or under it.
func TestDiffSpellingsOfAnything(t *testing.T) {
	same := [][2]string{
		{`{"additionalProperties": false}`, `{"additionalProperties": {"not": {}}}`},
		{`{}`, `{"additionalProperties": {}}`},
		{`{}`, `{"additionalProperties": true}`},
		{`{}`, `{"items": {}}`},
		{`{"type": "object", "additionalProperties": false}`, `{"type": "object", "additionalProperties": {"not": {"description": "anything"}, "description": "nothing"}}`},
		{`{"type": "array", "items": false}`, `{"type": "array", "items": {"not": {}, "title": "none"}}`},
	}
	for _, p := range same {
		for _, pair := range [][2]string{p, {p[1], p[0]}} {
			r := Diff(keywordSpec(t, "", pair[0]), keywordSpec(t, "", pair[1]))
			require.True(t, r.Empty(), "%s to %s: %s", pair[0], pair[1], r.Markdown())
		}
	}
	requireAt(t, `{"type": "object"}`, `{"type": "object", "additionalProperties": {"not": {}, "description": "none"}}`, "", "additional properties no longer allowed", true, false)
}

// TestDiffNaNBound checks that a NaN bound, which YAML can write and no
// number can be compared with, is read as no bound: two documents that
// hold it are the same, and the example ignores it.
func TestDiffNaNBound(t *testing.T) {
	spec := func(s string) *Document {
		doc, err := Parse([]byte(`openapi: 3.1.0
info:
  title: t
  version: "1"
paths:
  /x:
    post:
      requestBody:
        content:
          application/json:
            schema: ` + s + `
      responses:
        "200":
          description: ok
`))
		require.NoError(t, err)
		return doc
	}
	for _, s := range []string{`{type: number, minimum: .nan}`, `{type: integer, maximum: .nan, exclusiveMinimum: .nan}`} {
		r := Diff(spec(s), spec(s))
		require.True(t, r.Empty(), "%s: %s", s, r.Markdown())
	}
	r := Diff(spec(`{type: number, minimum: .nan}`), spec(`{type: number, minimum: 3}`))
	require.Equal(t, []string{"POST /x › request body | minimum added: 3"}, changeLines(r))

	doc := &Document{}
	nan := math.NaN()
	require.Equal(t, 0.0, doc.ExampleValue(&Schema{Type: TypeList{"number"}, Minimum: &nan}, ForRequest))
	require.Equal(t, -3.0, doc.ExampleValue(&Schema{Type: TypeList{"number"}, Minimum: &nan, Maximum: ptrTo(-3.0)}, ForRequest))
	require.Equal(t, int64(0), doc.ExampleValue(&Schema{Type: TypeList{"integer"}, Maximum: &nan}, ForRequest))
}

func ptrTo[T any](v T) *T {
	return &v
}

// TestDiffPropertyRemovedIntoAdditional checks that a property removed from
// an object whose additional properties limit values breaks a request,
// which may still send it, unless their schema is written as the
// property's was, give or take annotations, a reference naming the same
// model, and that one removed from an object that allows any other property
// does not. A schema written otherwise breaks a request even where it
// accepts every value the property did.
func TestDiffPropertyRemovedIntoAdditional(t *testing.T) {
	tests := []struct {
		property, additional string
		request              bool
	}{
		{`{"type": "string"}`, `false`, true},
		{`{"type": "string"}`, `{"not": {}}`, true},
		{`{"type": "string"}`, ``, false},
		{`{"type": "string"}`, `true`, false},
		{`{"type": "string"}`, `{"description": "any"}`, false},
		{`{"type": "string", "maxLength": 3}`, `{"type": "string", "maxLength": 3}`, false},
		{`{"type": "string", "maxLength": 3, "description": "a"}`, `{"type": "string", "maxLength": 3}`, false},
		{`{"$ref": "#/components/schemas/Code"}`, `{"$ref": "#/components/schemas/Code"}`, false},
		{`{"type": "string", "maxLength": 3}`, `{"type": "string"}`, true},
		{`{"type": "integer"}`, `{"type": "string"}`, true},
		{`{"$ref": "#/components/schemas/Code"}`, `{"$ref": "#/components/schemas/Code2"}`, true},
	}
	for _, tt := range tests {
		additional := ``
		if tt.additional != `` {
			additional = `, "additionalProperties": ` + tt.additional
		}
		r := Diff(
			keywordSpec(t, "", `{"type": "object", "properties": {"a": `+tt.property+`}`+additional+`}`),
			keywordSpec(t, "", `{"type": "object"`+additional+`}`),
		)
		od := r.Operation("POST", "/things")
		require.NotNil(t, od, r.Markdown())
		for path, request := range directionPaths {
			require.Equal(t, []Change{{Kind: Removed, Path: path + " › a", Detail: "property removed", Breaking: tt.request || !request}}, changesAt(od, path+" › a"), "%s into %s\n%s", tt.property, tt.additional, r.Markdown())
		}
	}
}

// variantSpec writes a document whose request body is an anyOf of n
// object variants.
func variantSpec(n int) []byte {
	variants := make([]string, n)
	for i := range n {
		variants[i] = fmt.Sprintf(`{"type": "object", "properties": {"v%d": {"type": "string"}}, "required": ["v%d"]}`, i, i)
	}
	return []byte(`{"openapi": "3.1.0", "info": {"title": "t", "version": "1"}, "paths": {"/r": {"post": {
  "requestBody": {"content": {"application/json": {"schema": {"anyOf": [` + strings.Join(variants, ", ") + `]}}}},
  "responses": {"200": {"description": "ok"}}}}}}`)
}

// nestedVariants writes an anyOf of k variants, each an anyOf of k, depth
// levels deep, with leaf variants at the bottom, one more of them in each
// bottom list when extra is set.
func nestedVariants(k, depth int, extra bool) string {
	n := k
	if depth == 1 && extra {
		n++
	}
	variants := make([]string, n)
	for i := range n {
		if depth == 1 {
			variants[i] = fmt.Sprintf(`{"type": "string", "maxLength": %d}`, i)
		} else {
			variants[i] = nestedVariants(k, depth-1, extra)
		}
	}
	return `{"anyOf": [` + strings.Join(variants, ", ") + `]}`
}

// BenchmarkDiffVariantLists measures Diff on a list of 3000 variants that
// gained one, and on lists nested 5 deep, 4 variants each, whose bottom
// lists gained one.
func BenchmarkDiffVariantLists(b *testing.B) {
	base, err := Parse(variantSpec(3000))
	require.NoError(b, err)
	head, err := Parse(variantSpec(3001))
	require.NoError(b, err)
	require.Equal(b, "any of variants added", Diff(base, head).Changes[0].Detail)
	b.Run("long", func(b *testing.B) {
		for range b.N {
			Diff(base, head)
		}
	})
	nested := func(extra bool) *Document {
		doc, err := Parse([]byte(`{"openapi": "3.1.0", "info": {"title": "t", "version": "1"}, "paths": {"/r": {"post": {
  "requestBody": {"content": {"application/json": {"schema": ` + nestedVariants(4, 5, extra) + `}}},
  "responses": {"200": {"description": "ok"}}}}}}`))
		require.NoError(b, err)
		return doc
	}
	nb := nested(false)
	nh := nested(true)
	require.Len(b, Diff(nb, nh).Changes, 256)
	b.Run("nested", func(b *testing.B) {
		for range b.N {
			Diff(nb, nh)
		}
	})
}

// TestWrittenKeyEveryField checks that writtenKey tells apart two schemas
// that differ in any field but an annotation, so a field added to Schema is
// not missed there, and ignores the annotations.
func TestWrittenKeyEveryField(t *testing.T) {
	typ := reflect.TypeOf(Schema{})
	written := map[int]bool{}
	for _, i := range writtenFields {
		written[i] = true
	}
	for i := range typ.NumField() {
		name := typ.Field(i).Name
		var s Schema
		v := reflect.ValueOf(&s).Elem().Field(i)
		switch {
		case name == "Properties":
			s.Properties.Set("x", &Schema{})
		case name == "Extensions":
			s.Extensions = map[string]any{"x-a": 1}
		case v.Kind() == reflect.String:
			v.SetString("x")
		case v.Kind() == reflect.Bool:
			v.SetBool(true)
		case v.Kind() == reflect.Pointer:
			v.Set(reflect.New(v.Type().Elem()))
		case v.Kind() == reflect.Slice:
			v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		case v.Kind() == reflect.Interface:
			v.Set(reflect.ValueOf("x"))
		default:
			t.Fatalf("no value for %s", name)
		}
		if written[i] {
			require.NotEqual(t, writtenKey(&Schema{}), writtenKey(&s), name)
		} else {
			require.Equal(t, writtenKey(&Schema{}), writtenKey(&s), name)
		}
	}
	require.NotEqual(t, writtenKey(&Schema{Ref: "#/components/schemas/A"}), writtenKey(&Schema{Ref: "#/components/schemas/B"}))
	require.NotEqual(t, writtenKey(&Schema{Minimum: ptrTo(1.0)}), writtenKey(&Schema{Maximum: ptrTo(1.0)}))
}

// linkedSpec writes n models, each a oneOf of five object variants and three
// references to other models picked by a fixed sequence, with one more
// object variant in every other model when extra is set, and n/5
// operations, each sending one model and receiving another.
func linkedSpec(n int, extra bool) []byte {
	var sb strings.Builder
	sb.WriteString(`{"openapi": "3.1.0", "info": {"title": "t", "version": "1"}, "paths": {`)
	next := uint64(1)
	pick := func() int {
		next = next*6364136223846793005 + 1442695040888963407
		return int(next>>33) % n
	}
	for i := range n / 5 {
		if i > 0 {
			sb.WriteString(", ")
		}
		fmt.Fprintf(&sb, `"/o%d": {"post": {"requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/M%d"}}}}, "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/M%d"}}}}}}}`, i, pick(), pick())
	}
	sb.WriteString(`}, "components": {"schemas": {`)
	for i := range n {
		if i > 0 {
			sb.WriteString(", ")
		}
		fmt.Fprintf(&sb, `"M%d": {"oneOf": [`, i)
		for j := range 5 {
			fmt.Fprintf(&sb, `{"type": "object", "properties": {"f%d": {"type": "string"}}, "required": ["f%d"]}, `, j, j)
		}
		fmt.Fprintf(&sb, `{"$ref": "#/components/schemas/M%d"}, {"$ref": "#/components/schemas/M%d"}, {"$ref": "#/components/schemas/M%d"}`, pick(), pick(), pick())
		if extra && i%2 == 0 {
			sb.WriteString(`, {"type": "object", "properties": {"extra": {"type": "integer"}}}`)
		}
		sb.WriteString(`]}`)
	}
	sb.WriteString(`}}}`)
	return []byte(sb.String())
}

// BenchmarkDiffLinkedModels measures Diff on 4000 models that refer to each
// other through oneOf variants, half of them gaining a variant, so most
// operations and models reach changed models through many others.
func BenchmarkDiffLinkedModels(b *testing.B) {
	base, err := Parse(linkedSpec(4000, false))
	require.NoError(b, err)
	head, err := Parse(linkedSpec(4000, true))
	require.NoError(b, err)
	require.True(b, Diff(base, head).Breaking())
	b.ResetTimer()
	for range b.N {
		Diff(base, head)
	}
}

// repeatedSpec writes a document whose request body is body, with models
// A0 to Ak and B0 to Bk: A_i an object whose properties x and y both refer
// to A_i+1, written under a not when not is set, B_i an object whose
// additional properties, or the schema under its not, refer to B_i+1, and
// A_k and B_k strings.
func repeatedSpec(k int, body string, not bool) []byte {
	var models []string
	for i := range k {
		x := fmt.Sprintf(`{"$ref": "#/components/schemas/A%d"}`, i+1)
		b := fmt.Sprintf(`"B%d": {"type": "object", "additionalProperties": {"$ref": "#/components/schemas/B%d"}}`, i, i+1)
		if not {
			x = `{"not": ` + x + `}`
			b = fmt.Sprintf(`"B%d": {"type": "object", "properties": {"x": {"not": {"$ref": "#/components/schemas/B%d"}}, "y": {"not": {"$ref": "#/components/schemas/B%d"}}}}`, i, i+1, i+1)
		}
		models = append(models, fmt.Sprintf(`"A%d": {"type": "object", "properties": {"x": %s, "y": %s}}`, i, x, x), b)
	}
	models = append(models, fmt.Sprintf(`"A%d": {"type": "string"}, "B%d": {"type": "string"}`, k, k))
	return []byte(`{"openapi": "3.1.0", "info": {"title": "t", "version": "1"}, "paths": {` + bodyPath(body) + `}, "components": {"schemas": {` + strings.Join(models, ", ") + `}}}`)
}

// BenchmarkDiffRepeatedReferences measures Diff where every level of 22 has
// two properties that refer to the next level's model: a removed property
// against the schema of additional properties, and a not, each judged from
// what is written, so the time is linear in the levels.
func BenchmarkDiffRepeatedReferences(b *testing.B) {
	for _, tt := range []struct {
		name       string
		base, head string
		not        bool
	}{
		{"removed", `{"type": "object", "properties": {"p": {"$ref": "#/components/schemas/A0"}}}`, `{"type": "object", "additionalProperties": {"$ref": "#/components/schemas/B0"}}`, false},
		{"not", `{"not": {"$ref": "#/components/schemas/A0"}}`, `{"not": {"$ref": "#/components/schemas/B0"}}`, true},
	} {
		base, err := Parse(repeatedSpec(22, tt.base, tt.not))
		require.NoError(b, err)
		head, err := Parse(repeatedSpec(22, tt.head, tt.not))
		require.NoError(b, err)
		b.Run(tt.name, func(b *testing.B) {
			for range b.N {
				Diff(base, head)
			}
		})
	}
}

// routePath is the paths entry of an operation at route: a POST whose
// request body is schema, or, where response is set, a GET whose 200
// response is schema.
func routePath(route, schema string, response bool) string {
	if response {
		return `"` + route + `": {"get": {"responses": {"200": {"description": "ok", "content": {"application/json": {"schema": ` + schema + `}}}}}}`
	}
	return `"` + route + `": {"post": {"requestBody": {"content": {"application/json": {"schema": ` + schema + `}}}, "responses": {"200": {"description": "ok"}}}}`
}

// orderOp is an operation of an operation order case: its route, its
// schema in each document, and whether that schema is its response.
type orderOp struct {
	route, base, head string
	response          bool
}

// operationOrders compares documents with ops in each order that puts one
// of them first, and with each op alone, and checks that each operation
// gets the same changes in every run.
func operationOrders(t *testing.T, ops []orderOp, baseSchemas, headSchemas string) map[string][]Change {
	t.Helper()
	orders := [][]orderOp{}
	for i := range ops {
		orders = append(orders, []orderOp{ops[i]})
		rest := slices.Concat(ops[:i], ops[i+1:])
		orders = append(orders, append([]orderOp{ops[i]}, rest...))
	}
	seen := map[string][]Change{}
	for _, order := range orders {
		var bp, hp []string
		for _, op := range order {
			bp = append(bp, routePath(op.route, op.base, op.response))
			hp = append(hp, routePath(op.route, op.head, op.response))
		}
		r := diffWithin(t, opsSpec(t, strings.Join(bp, ", "), baseSchemas), opsSpec(t, strings.Join(hp, ", "), headSchemas))
		for _, op := range order {
			method := "POST"
			if op.response {
				method = "GET"
			}
			var changes []Change
			od := r.Operation(method, op.route)
			if od != nil {
				changes = publicChanges(od.Changes)
			}
			want, ok := seen[op.route]
			if !ok {
				seen[op.route] = changes
				continue
			}
			require.Equal(t, want, changes, "%s with %d operations", op.route, len(order))
		}
	}
	return seen
}

// breakingIn reports whether any of changes is breaking.
func breakingIn(changes []Change) bool {
	for _, c := range changes {
		if c.Breaking {
			return true
		}
	}
	return false
}

// TestDiffOperationOrder checks that the changes of an operation do not
// depend on the operations compared before it, on documents whose models
// refer to themselves or to each other under nots, each of which gave an
// operation other flags by where it stood while what a not held was compared
// in a walk of its own and what that walk found kept.
func TestDiffOperationOrder(t *testing.T) {
	ref := func(name string) string {
		return `{"$ref": "#/components/schemas/` + name + `"}`
	}
	not := func(name string) string {
		return `{"not": ` + ref(name) + `}`
	}
	t.Run("a model that refers to itself under a not", func(t *testing.T) {
		model := func(name, max string) string {
			return `"` + name + `": {"type": "object", "maxProperties": ` + max + `, "properties": {"n": ` + not(name) + `}}`
		}
		got := operationOrders(t, []orderOp{
			{"/a", ref("A"), ref("B"), true},
			{"/b", not("A"), not("B"), true},
		}, model("A", "5"), model("B", "3"))
		require.True(t, breakingIn(got["/b"]), "%v", got["/b"])
		require.Contains(t, got["/a"], Change{Kind: Changed, Path: "GET /a › response 200 › n", Detail: "not changed", Breaking: true})
	})
	t.Run("two models that refer to each other under nots", func(t *testing.T) {
		models := func(p, r, max string) string {
			return `"` + p + `": {"type": "object", "maxProperties": ` + max + `, "not": ` + ref(r) + `}, "` + r + `": {"type": "object", "properties": {"x": ` + not(p) + `}}`
		}
		got := operationOrders(t, []orderOp{
			{"/a", not("P"), not("Q"), true},
			{"/b", not("R"), not("S"), true},
		}, models("P", "R", "3"), models("Q", "S", "5"))
		require.True(t, breakingIn(got["/b"]), "%v", got["/b"])
	})
	t.Run("a cycle through additional properties and nested nots", func(t *testing.T) {
		models := func(suffix, extra string) string {
			a, b, n := "A"+suffix, "B"+suffix, "N"+suffix
			return `"` + a + `": {"additionalProperties": ` + ref(b) + `}, "` + b + `": {"properties": {"c": {"not": ` + not(a) + `}}, "additionalProperties": ` + not(n) + `}, "` + n + `": {` + extra + `"additionalProperties": ` + ref(a) + `}`
		}
		got := operationOrders(t, []orderOp{
			{"/a", not("N"), not("Nr"), false},
			{"/b", ref("A"), ref("Ar"), false},
		}, models("", ""), models("r", `"maxLength": 3, `))
		require.Equal(t, []Change{{Kind: Changed, Path: "POST /a › request body", Detail: "not changed", Breaking: true}}, got["/a"])
		require.Contains(t, got["/b"], Change{Kind: Changed, Path: "POST /b › request body › <key> › c", Detail: "not changed", Breaking: true})
	})
	t.Run("an odd cycle of nots entered at two models", func(t *testing.T) {
		models := func(n, max string, k bool) string {
			a, b, c := "A"+n, "B"+n, "C"+n
			props := ""
			if k {
				props = `"properties": {"k": ` + ref("K") + `}, `
			}
			return `"` + a + `": {"type": "string", "maxLength": ` + max + `, ` + props + `"not": ` + ref(b) + `}, "` + b + `": ` + not(c) + `, "` + c + `": ` + not(a)
		}
		for _, k := range []bool{false, true} {
			base := models("1", "5", k) + `, "K": {"type": "string", "maxLength": 5}`
			head := models("2", "3", k) + `, "K": {"type": "string", "maxLength": 3}`
			got := operationOrders(t, []orderOp{
				{"/a", ref("A1"), ref("A2"), false},
				{"/c", ref("C1"), ref("C2"), false},
			}, base, head)
			require.Equal(t, []Change{
				{Kind: Changed, Path: "POST /c › request body", Detail: "schema changed from C1 to C2"},
				{Kind: Changed, Path: "POST /c › request body", Detail: "not changed", Breaking: true},
			}, got["/c"])
		}
	})
	t.Run("a property beside the not", func(t *testing.T) {
		model := func(name, max string) string {
			return `"` + name + `": {"type": "object", "properties": {"q": ` + not(name) + `, "x": {"type": "string", "maxLength": ` + max + `}}}`
		}
		models := model("A", "5") + ", " + model("B", "10")
		got := operationOrders(t, []orderOp{
			{"/a", `{"properties": {"p": ` + ref("A") + `}}`, `{"properties": {"p": ` + ref("B") + `}}`, false},
			{"/b", not("A"), not("B"), false},
		}, models, models)
		require.True(t, breakingIn(got["/b"]), "%v", got["/b"])
	})
}

// notRing is n models, prefix0 to prefix(n-1), each a not of a reference to
// the next and the last of the first.
func notRing(prefix string, n int) string {
	var models []string
	for i := range n {
		models = append(models, fmt.Sprintf(`"%s%d": {"not": {"$ref": "#/components/schemas/%s%d"}}`, prefix, i, prefix, (i+1)%n))
	}
	return strings.Join(models, ", ")
}

// itemsChain is s as the items of n arrays, one inside the other.
func itemsChain(s *Schema, n int) *Schema {
	for range n {
		s = &Schema{Items: s}
	}
	return s
}

// setSchema sets the request body of POST route, or the 200 response of GET
// route where response is set, to s.
func setSchema(t *testing.T, doc *Document, route string, response bool, s *Schema) {
	t.Helper()
	item, ok := doc.Paths.Get(route)
	require.True(t, ok)
	var media *MediaType
	if response {
		resp, ok := item.Get.Responses.Get("200")
		require.True(t, ok)
		media, ok = resp.Content.Get("application/json")
	} else {
		media, ok = item.Post.RequestBody.Content.Get("application/json")
	}
	require.True(t, ok)
	media.Schema = s
}

// TestDiffFoldedUnionOfUnion checks that a 3.1 union of one schema and
// {type: null} whose one schema is itself a union is compared as that
// union: against the same document it reports nothing, and against a list
// of variants written out it reads as variants added or removed.
func TestDiffFoldedUnionOfUnion(t *testing.T) {
	const s = `{"type": "string"}`
	const i = `{"type": "integer"}`
	const null = `{"type": "null"}`
	ref := func(name string) string {
		return `{"$ref": "#/components/schemas/` + name + `"}`
	}
	const pets = `"Cat": {"type": "object", "properties": {"c": {"type": "string"}}}, "Dog": {"type": "object", "properties": {"d": {"type": "string"}}}`
	const models = `"P": {"type": "object", "properties": {"p": {"type": "string"}}}, "Q": {"type": "object", "properties": {"q": {"type": "string"}}}, "R": {"type": "object", "properties": {"r": {"type": "string"}}}, "U": {"anyOf": [{"$ref": "#/components/schemas/P"}, {"$ref": "#/components/schemas/Q"}]}, "V": {"oneOf": [{"$ref": "#/components/schemas/P"}, {"$ref": "#/components/schemas/Q"}]}`
	for _, version := range []string{"3.0.3", "3.1.0"} {
		for _, same := range []string{
			`{"anyOf": [{"anyOf": [` + s + `, ` + i + `]}, ` + null + `]}`,
			`{"oneOf": [{"oneOf": [` + ref("Cat") + `, ` + ref("Dog") + `]}, ` + null + `]}`,
			ref("NPet"),
			`{"anyOf": [` + ref("U") + `, ` + null + `]}`,
		} {
			schemas := pets + `, "NPet": {"oneOf": [{"oneOf": [` + ref("Cat") + `, ` + ref("Dog") + `]}, ` + null + `]}, ` + models
			r := Diff(keywordSpecWith(t, version, same, schemas), keywordSpecWith(t, version, same, schemas))
			require.True(t, r.Empty(), "%s %s\n%s", version, same, r.Markdown())
		}
	}

	format := func(f string) string {
		return `{"oneOf": [{"oneOf": [` + s + `, {"type": "number"` + f + `}]}, ` + null + `]}`
	}
	r := Diff(keywordSpecWith(t, "", format(`, "format": "date"`), ""), keywordSpecWith(t, "", format(""), ""))
	od := r.Operation("POST", "/things")
	require.NotNil(t, od, r.Markdown())
	for path := range directionPaths {
		require.Empty(t, changesAt(od, path), r.Markdown())
		require.Equal(t, []Change{{Kind: Changed, Path: path + " › one of #2", Detail: "format date removed"}}, publicChanges(changesAt(od, path+" › one of #2")), r.Markdown())
	}

	tests := []struct {
		base, head, detail string
		request, response  bool
	}{
		{`{"anyOf": [` + ref("U") + `, ` + null + `]}`, `{"anyOf": [` + ref("P") + `, ` + ref("Q") + `, ` + ref("R") + `, ` + null + `]}`, "any of variants added", false, true},
		{`{"anyOf": [` + ref("P") + `, ` + ref("Q") + `, ` + ref("R") + `, ` + null + `]}`, `{"anyOf": [` + ref("U") + `, ` + null + `]}`, "any of variants removed", true, false},
		{`{"anyOf": [{"anyOf": [` + ref("P") + `, ` + ref("Q") + `]}, ` + null + `]}`, `{"anyOf": [` + ref("P") + `, ` + ref("Q") + `, ` + ref("R") + `, ` + null + `]}`, "any of variants added", false, true},
		{`{"anyOf": [` + ref("P") + `, ` + ref("Q") + `, ` + ref("R") + `, ` + null + `]}`, `{"anyOf": [{"anyOf": [` + ref("P") + `, ` + ref("Q") + `]}, ` + null + `]}`, "any of variants removed", true, false},
		{`{"oneOf": [` + ref("V") + `, ` + null + `]}`, `{"oneOf": [` + ref("P") + `, ` + ref("Q") + `, ` + ref("R") + `, ` + null + `]}`, "one of variants added", false, true},
		{`{"oneOf": [` + ref("P") + `, ` + ref("Q") + `, ` + ref("R") + `, ` + null + `]}`, `{"oneOf": [{"oneOf": [` + ref("P") + `, ` + ref("Q") + `]}, ` + null + `]}`, "one of variants removed", true, false},
	}
	for _, tt := range tests {
		r := Diff(keywordSpecWith(t, "", tt.base, models), keywordSpecWith(t, "", tt.head, models))
		od := r.Operation("POST", "/things")
		require.NotNil(t, od, r.Markdown())
		for path, isRequest := range directionPaths {
			want := tt.response
			if isRequest {
				want = tt.request
			}
			require.Equal(t, []Change{{Kind: Changed, Path: path, Detail: tt.detail, Breaking: want}}, publicChanges(changesAt(od, path)), "%s to %s\n%s", tt.base, tt.head, r.Markdown())
		}
	}
}

// TestDiffUseFlagsJoined checks that a model used twice at one path, once
// plainly and once under two nots, which judge its changes as under a not,
// reports one model M changed with the flags of both uses, in either order
// of the variants.
func TestDiffUseFlagsJoined(t *testing.T) {
	const m = `{"$ref": "#/components/schemas/M"}`
	const twice = `{"not": {"not": ` + m + `}}`
	for _, variants := range [][2]string{{m, twice}, {twice, m}} {
		base := `{"anyOf": [` + variants[0] + `, ` + variants[1] + `]}`
		head := `{"anyOf": [` + variants[0] + `, ` + variants[1] + `, {"type": "boolean"}]}`
		r := Diff(opsSpec(t, respPath(base), `"M": {"type": "string", "enum": ["a"]}`), opsSpec(t, respPath(head), `"M": {"type": "string", "enum": ["a", "b"]}`))
		od := r.Operation("GET", "/r")
		require.NotNil(t, od, r.Markdown())
		require.Equal(t, []Change{
			{Kind: Changed, Path: "GET /r › response 200", Detail: "any of variants added", Breaking: true},
			{Kind: Changed, Path: "GET /r › response 200", Detail: "model M changed", Breaking: true},
		}, publicChanges(od.Changes), r.Markdown())
	}
}

// TestDiffNullableWhereNullStaysValid checks that nullable dropped or given
// breaks nothing where the schema lets null through without it: an enum
// that lists null, a const of null, which Parse does not tell from no const,
// and variants of which one lets null through. An enum or variants that
// refuse null still make nullable dropped break a request and given break a
// response.
func TestDiffNullableWhereNullStaysValid(t *testing.T) {
	tests := []struct {
		schema   string
		breaking bool
	}{
		{`"enum": ["a", null]`, false},
		{`"enum": [null]`, false},
		{`"const": null`, false},
		{`"oneOf": [{"type": "string"}, {"type": "integer", "nullable": true}]`, false},
		{`"anyOf": [{"type": "string"}, {"enum": ["a", null]}]`, false},
		{`"oneOf": [{"type": "string"}], "anyOf": [{"type": "integer", "nullable": true}]`, true},
		{`"enum": ["a"]`, true},
		{`"oneOf": [{"type": "string"}, {"type": "integer"}]`, true},
		{`"type": "string", "enum": ["a", null]`, true},
	}
	for _, tt := range tests {
		with := keywordSpec(t, "3.0.3", `{`+tt.schema+`, "nullable": true}`)
		without := keywordSpec(t, "3.0.3", `{`+tt.schema+`}`)
		for i, pair := range [][2]*Document{{with, without}, {without, with}} {
			r := Diff(pair[0], pair[1])
			od := r.Operation("POST", "/things")
			require.NotNil(t, od, tt.schema)
			detail := "no longer nullable"
			if i == 1 {
				detail = "made nullable"
			}
			for path, request := range directionPaths {
				want := tt.breaking && request == (i == 0)
				require.Equal(t, []Change{{Kind: Changed, Path: path, Detail: detail, Breaking: want}}, changesAt(od, path), "%s\n%s", tt.schema, r.Markdown())
			}
		}
	}
}

// TestDiffStructureBesideRef checks that a $ref with structure beside it,
// the same on both sides, is compared in place, so a change of the model
// it names reads where it applies there, together with the structure
// beside the reference: a property the model dropped breaks a request
// where additional properties are refused beside it, though not in the
// model, which allows them and is used in that request, and nullable the
// model dropped breaks one where a type is stated beside it. A chain of
// models referred to that way, and arrays nested 5,100 deep around such a
// reference to a model nested as deep, compare in time.
func TestDiffStructureBesideRef(t *testing.T) {
	const m = `"M": {"type": "object", "properties": {"a": {"type": "string"}, "b": {"type": "string"}}}`
	const mWithoutA = `"M": {"type": "object", "properties": {"b": {"type": "string"}}}`
	for _, beside := range []string{`"additionalProperties": false`, `"additionalProperties": {"type": "integer"}`} {
		site := bodyPath(`{"type": "object", "properties": {"p": {"$ref": "#/components/schemas/M", ` + beside + `}}}`)
		r := Diff(opsSpec(t, site, m), opsSpec(t, site, mWithoutA))
		require.Equal(t, []Change{
			{Kind: Removed, Path: "POST /w › request body › p › a", Detail: "property removed", Breaking: true},
			{Kind: Removed, Path: "M › a", Detail: "property removed"},
		}, publicChanges(r.Changes), r.Markdown())
	}

	site := bodyPath(`{"type": "object", "properties": {"p": {"$ref": "#/components/schemas/M", "type": "object", "properties": {"z": {"type": "string"}}}}}`)
	r := Diff(opsSpec(t, site, `"M": {"nullable": true, "maxLength": 3}`), opsSpec(t, site, `"M": {"maxLength": 3}`))
	require.Equal(t, []Change{
		{Kind: Changed, Path: "POST /w › request body › p", Detail: "no longer nullable", Breaking: true},
		{Kind: Changed, Path: "M", Detail: "no longer nullable"},
	}, publicChanges(r.Changes), r.Markdown())

	site = bodyPath(`{"type": "object", "properties": {"p": {"$ref": "#/components/schemas/A", "properties": {"z": {"type": "string"}}}}}`)
	r = Diff(opsSpec(t, site, `"A": {"$ref": "#/components/schemas/M"}, "M": {"type": "object", "properties": {"a": {"type": "string"}}}`), opsSpec(t, site, `"A": {"$ref": "#/components/schemas/M"}, "M": {"type": "object", "properties": {"a": {"type": "integer"}}}`))
	require.Equal(t, []Change{
		{Kind: Changed, Path: "POST /w › request body › p › a", Detail: "type changed from string to integer", Breaking: true},
		{Kind: Changed, Path: "A", Detail: "model M changed", Breaking: true},
		{Kind: Changed, Path: "M › a", Detail: "type changed from string to integer", Breaking: true},
	}, publicChanges(r.Changes), r.Markdown())

	chain := func(n int) string {
		var models []string
		for i := range n {
			models = append(models, fmt.Sprintf(`"M%d": {"type": "object", "properties": {"next": {"$ref": "#/components/schemas/M%d", "properties": {"z": {}}}}}`, i, i+1))
		}
		models = append(models, fmt.Sprintf(`"M%d": {"type": "string"}`, n))
		return strings.Join(models, ", ")
	}
	body := bodyPath(`{"$ref": "#/components/schemas/M0"}`)
	r = diffWithin(t, opsSpec(t, body, chain(1000)), opsSpec(t, body, chain(1000)))
	require.True(t, r.Empty(), r.Markdown())

	docs := [2]*Document{}
	for i := range docs {
		docs[i] = opsSpec(t, bodyPath(`{"$ref": "#/components/schemas/A"}`), `"A": {}, "B": {}`)
		end := &Schema{Ref: "#/components/schemas/B", Properties: Ordered[*Schema]{}}
		end.Properties.Set("z", &Schema{})
		docs[i].Components.Schemas.Set("A", itemsChain(end, 5100))
		docs[i].Components.Schemas.Set("B", itemsChain(&Schema{Type: TypeList{"string"}}, 5100))
	}
	r = diffWithin(t, docs[0], docs[1])
	require.True(t, r.Empty(), "%v", changeLines(r))
}

// TestDiffNotsReferringBack checks that models that refer to themselves or
// to each other under nots compare in time, as a not is compared as
// written and never walked: one model whose ten properties each hold a not
// of an allOf of the model and a bound, the same split over two models,
// six properties each, two rings of 997 and 1,009 models renamed between
// the documents, each a not of the next, and two models whose properties
// hold nots of each other with a description, renamed and with one
// description changed, which reads not changed, breaking both, at every
// property, as the references under the nots name other models.
func TestDiffNotsReferringBack(t *testing.T) {
	ref := func(name string) string {
		return `{"$ref": "#/components/schemas/` + name + `"}`
	}
	joins := func(name, target string, n int) string {
		var props []string
		for i := range n {
			props = append(props, fmt.Sprintf(`"p%d": {"not": {"allOf": [%s, {"maxLength": %d}]}}`, i, ref(target), i+1))
		}
		return `"` + name + `": {"type": "object", "properties": {` + strings.Join(props, ", ") + `}}`
	}
	self := opsSpec(t, bodyPath(ref("M")), joins("M", "M", 10))
	r := diffWithin(t, self, self)
	require.True(t, r.Empty(), r.Markdown())
	pair := opsSpec(t, bodyPath(ref("A")), joins("A", "B", 6)+", "+joins("B", "A", 6))
	r = diffWithin(t, pair, pair)
	require.True(t, r.Empty(), r.Markdown())

	r = diffWithin(t, opsSpec(t, respPath(ref("A0")), notRing("A", 997)), opsSpec(t, respPath(ref("B0")), notRing("B", 1009)))
	require.Equal(t, []Change{
		{Kind: Changed, Path: "GET /r › response 200", Detail: "schema changed from A0 to B0"},
		{Kind: Changed, Path: "GET /r › response 200", Detail: "not changed", Breaking: true},
	}, publicChanges(r.Operation("GET", "/r").Changes), r.Markdown())

	described := func(a, b string, changed bool) string {
		var pa, pb []string
		for i := range 7 {
			d := fmt.Sprintf("d%d", i)
			if changed && i == 0 {
				d = "changed"
			}
			pa = append(pa, fmt.Sprintf(`"p%d": {"not": {"$ref": "#/components/schemas/%s", "description": "%s"}}`, i, b, d))
			pb = append(pb, fmt.Sprintf(`"p%d": {"not": {"$ref": "#/components/schemas/%s", "description": "%s"}}`, i, a, d))
		}
		return `"` + a + `": {"type": "object", "properties": {` + strings.Join(pa, ", ") + `}}, "` + b + `": {"type": "object", "properties": {` + strings.Join(pb, ", ") + `}}`
	}
	r = diffWithin(t, opsSpec(t, bodyPath(ref("A")), described("A", "B", false)), opsSpec(t, bodyPath(ref("C")), described("C", "D", true)))
	want := []Change{{Kind: Changed, Path: "POST /w › request body", Detail: "schema changed from A to C"}}
	for i := range 7 {
		want = append(want, Change{Kind: Changed, Path: fmt.Sprintf("POST /w › request body › p%d", i), Detail: "not changed", Breaking: true})
	}
	require.Equal(t, want, publicChanges(r.Operation("POST", "/w").Changes), r.Markdown())
}

// TestDiffPropertyRemovedIntoOpenedObject checks that a property removed
// from a closed object that now accepts it through the schema of its
// additional properties, written as the property's was, leaves the models
// that schema names used there: their changes reach the use, and a
// narrowing one breaks the request. Opening a closed object that listed no
// such property gives the models no use, as no value reached them before.
func TestDiffPropertyRemovedIntoOpenedObject(t *testing.T) {
	const s = `{"$ref": "#/components/schemas/S"}`
	spec := func(body, maxLength string) *Document {
		return opsSpec(t, bodyPath(`{"$ref": "#/components/schemas/Body"}`)+", "+respPath(s),
			`"Body": `+body+`, "S": {"type": "string", "maxLength": `+maxLength+`}`)
	}
	r := Diff(
		spec(`{"type": "object", "properties": {"p": `+s+`}, "additionalProperties": false}`, "10"),
		spec(`{"type": "object", "additionalProperties": `+s+`}`, "5"),
	)
	require.True(t, r.Breaking(), r.Markdown())
	require.Equal(t, []Change{
		{Kind: Removed, Path: "Body › p", Detail: "property removed"},
		{Kind: Changed, Path: "Body", Detail: "additional properties allowed"},
		{Kind: Changed, Path: "Body › <key>", Detail: "model S changed", Breaking: true},
	}, publicChanges(r.Model("Body").Changes), r.Markdown())
	require.Equal(t, []Change{{Kind: Changed, Path: "S", Detail: "maxLength lowered from 10 to 5", Breaking: true}}, publicChanges(r.Model("S").Changes), r.Markdown())

	r = Diff(
		spec(`{"type": "object", "properties": {"q": {"type": "string"}}, "additionalProperties": false}`, "10"),
		spec(`{"type": "object", "properties": {"q": {"type": "string"}}, "additionalProperties": `+s+`}`, "5"),
	)
	require.False(t, r.Breaking(), r.Markdown())
}

// TestDiffSetsUnderNot checks that properties, required names and enum
// values written in another order are written the same: under a not they
// are no change, and a variant that only orders its properties otherwise is
// the same variant, so a variant added beside it reads variants added.
func TestDiffSetsUnderNot(t *testing.T) {
	pairs := [][2]string{
		{`{"properties": {"a": {"type": "string"}, "b": {"type": "integer"}}}`, `{"properties": {"b": {"type": "integer"}, "a": {"type": "string"}}}`},
		{`{"required": ["a", "b"]}`, `{"required": ["b", "a"]}`},
		{`{"enum": ["a", "b"]}`, `{"enum": ["b", "a"]}`},
	}
	for _, pair := range pairs {
		for _, route := range []func(string) string{bodyPath, respPath} {
			r := Diff(
				opsSpec(t, route(`{"type": "object", "not": `+pair[0]+`}`), ``),
				opsSpec(t, route(`{"type": "object", "not": `+pair[1]+`}`), ``),
			)
			require.True(t, r.Empty(), "%s to %s\n%s", pair[0], pair[1], r.Markdown())
		}
	}

	r := Diff(
		opsSpec(t, bodyPath(`{"oneOf": [{"type": "object", "properties": {"a": {"type": "string"}, "b": {"type": "integer"}}}, {"type": "string"}]}`), ``),
		opsSpec(t, bodyPath(`{"oneOf": [{"type": "object", "properties": {"b": {"type": "integer"}, "a": {"type": "string"}}}, {"type": "string"}, {"type": "integer"}]}`), ``),
	)
	require.Equal(t, []Change{{Kind: Changed, Path: "POST /w › request body", Detail: "one of variants added"}}, publicChanges(r.Operation("POST", "/w").Changes), r.Markdown())
}

// TestDiffModelUsedThroughAnotherName checks that a model whose enum loses
// a value, used only from a response, breaks nothing, and from a request
// breaks it, where the use reaches the model through a component that is
// only another name for it, an allOf of its reference or a bare $ref, or
// is compared in place: through such a component on the base side only,
// which the head dropped for the model itself, joined to another member by
// an allOf, or with structure beside its reference. The entries for the
// model's changes, its own and the other name's, follow the use.
func TestDiffModelUsedThroughAnotherName(t *testing.T) {
	const m = `{"$ref": "#/components/schemas/M"}`
	model := func(values string) string {
		return `"M": {"type": "string", "enum": [` + values + `]}`
	}
	tests := []struct {
		name               string
		baseUse, headUse   string
		baseExtra, headExt string
	}{
		{"allOf of the reference", `{"$ref": "#/components/schemas/W"}`, `{"$ref": "#/components/schemas/W"}`, `, "W": {"allOf": [` + m + `]}`, `, "W": {"allOf": [` + m + `]}`},
		{"bare $ref", `{"$ref": "#/components/schemas/W"}`, `{"$ref": "#/components/schemas/W"}`, `, "W": ` + m, `, "W": ` + m},
		{"other name dropped", `{"type": "object", "properties": {"m": {"$ref": "#/components/schemas/X"}}}`, `{"type": "object", "properties": {"m": ` + m + `}}`, `, "X": ` + m, ``},
		{"joined by an allOf", `{"allOf": [` + m + `, {"description": "d", "maxLength": 9}]}`, `{"allOf": [` + m + `, {"description": "d", "maxLength": 9}]}`, ``, ``},
		{"structure beside the reference", `{"$ref": "#/components/schemas/M", "properties": {"z": {}}}`, `{"$ref": "#/components/schemas/M", "properties": {"z": {}}}`, ``, ``},
	}
	for _, tt := range tests {
		for _, response := range []bool{true, false} {
			route := bodyPath
			if response {
				route = respPath
			}
			r := Diff(
				opsSpec(t, route(tt.baseUse), model(`"a", "b"`)+tt.baseExtra),
				opsSpec(t, route(tt.headUse), model(`"a"`)+tt.headExt),
			)
			require.Equal(t, !response, r.Breaking(), "%s, response %t\n%s", tt.name, response, r.Markdown())
			for _, md := range r.Models {
				for _, c := range md.Changes {
					if c.Kind == Changed {
						require.Equal(t, !response, c.Breaking, "%s, response %t: %s %s\n%s", tt.name, response, c.Path, c.Detail, r.Markdown())
					}
				}
			}
		}
	}
}

// TestDiffTypeToOnlyNull checks that a type changed to only null narrows,
// which breaks a request, and one changed from it widens, which breaks a
// response, beside the change of nullable, which breaks the other side, and
// that only null given a second type widens alone.
func TestDiffTypeToOnlyNull(t *testing.T) {
	r := Diff(keywordSpec(t, "", `{"type": "string"}`), keywordSpec(t, "", `{"type": "null"}`))
	od := r.Operation("POST", "/things")
	require.NotNil(t, od, r.Markdown())
	for path, request := range directionPaths {
		require.Equal(t, []Change{
			{Kind: Changed, Path: path, Detail: "type changed from string to null", Breaking: request},
			{Kind: Changed, Path: path, Detail: "made nullable", Breaking: !request},
		}, changesAt(od, path), r.Markdown())
	}

	r = Diff(keywordSpec(t, "", `{"type": "null"}`), keywordSpec(t, "", `{"type": "string"}`))
	od = r.Operation("POST", "/things")
	require.NotNil(t, od, r.Markdown())
	for path, request := range directionPaths {
		require.Equal(t, []Change{
			{Kind: Changed, Path: path, Detail: "type changed from null to string", Breaking: !request},
			{Kind: Changed, Path: path, Detail: "no longer nullable", Breaking: request},
		}, changesAt(od, path), r.Markdown())
	}

	r = Diff(keywordSpec(t, "", `{"type": "null"}`), keywordSpec(t, "", `{"type": ["string", "null"]}`))
	od = r.Operation("POST", "/things")
	require.NotNil(t, od, r.Markdown())
	for path, request := range directionPaths {
		require.Equal(t, []Change{{Kind: Changed, Path: path, Detail: "type changed from null to string", Breaking: !request}}, changesAt(od, path), r.Markdown())
	}
}

// TestDiffSwitchToAnotherName checks that a request body switched from a
// model to a component that is only another name for it, an allOf of its
// reference or a bare $ref, used in a response on both sides, makes that
// component used in the request: its entry for the model's change that
// narrows breaks.
func TestDiffSwitchToAnotherName(t *testing.T) {
	model := func(maxLength string) string {
		return `"M": {"type": "object", "properties": {"v": {"type": "string", "maxLength": ` + maxLength + `}}}`
	}
	for _, name := range []string{`{"allOf": [{"$ref": "#/components/schemas/M"}]}`, `{"$ref": "#/components/schemas/M"}`} {
		other := `"W": ` + name
		w := `{"$ref": "#/components/schemas/W"}`
		r := Diff(
			opsSpec(t, bodyPath(`{"$ref": "#/components/schemas/M"}`)+", "+respPath(w), model("10")+", "+other),
			opsSpec(t, bodyPath(w)+", "+respPath(w), model("5")+", "+other),
		)
		require.Equal(t, []Change{{Kind: Changed, Path: "W", Detail: "model M changed", Breaking: true}}, publicChanges(r.Model("W").Changes), "%s\n%s", name, r.Markdown())
	}
}

// TestDiffNullOnlySpellings checks that only null written as {type: null}
// or as a oneOf or anyOf of only {type: null} members is the same schema.
func TestDiffNullOnlySpellings(t *testing.T) {
	for _, s := range []string{`{"anyOf": [{"type": "null"}]}`, `{"oneOf": [{"type": "null"}]}`} {
		for _, route := range []func(string) string{bodyPath, respPath} {
			for _, pair := range [][2]string{{`{"type": "null"}`, s}, {s, `{"type": "null"}`}} {
				r := Diff(opsSpec(t, route(pair[0]), ``), opsSpec(t, route(pair[1]), ``))
				require.True(t, r.Empty(), "%s to %s\n%s", pair[0], pair[1], r.Markdown())
			}
		}
	}
}

// TestDiffTypeListOrder checks that a type list written in another order is
// no change, and that one with other types reads them as written.
func TestDiffTypeListOrder(t *testing.T) {
	r := Diff(opsSpec(t, bodyPath(`{"type": ["string", "integer"]}`), ``), opsSpec(t, bodyPath(`{"type": ["integer", "string"]}`), ``))
	require.True(t, r.Empty(), r.Markdown())
	r = Diff(opsSpec(t, bodyPath(`{"type": ["string", "integer"]}`), ``), opsSpec(t, bodyPath(`{"type": ["integer", "boolean"]}`), ``))
	require.Equal(t, []Change{{Kind: Changed, Path: "POST /w › request body", Detail: "type changed from string | integer to integer | boolean", Breaking: true}}, publicChanges(r.Operation("POST", "/w").Changes), r.Markdown())
}

// TestDiffAllOfMergedKeywords checks that a type narrowed by a later allOf
// member or beside a $ref, and a property, items or additional properties
// that two allOf members define, are compared as what applies, every
// definition together, rather than as the first or the last one.
func TestDiffAllOfMergedKeywords(t *testing.T) {
	const amount = `"Amount": {"type": "number"}, "C0": {"type": "integer"}`
	object := func(v string) string {
		return amount + `, "M": {"type": "object", "properties": {"v": ` + v + `}}`
	}
	members := func(a, b string) string {
		return `"M": {"allOf": [` + a + `, ` + b + `]}`
	}
	for _, c := range []struct {
		name       string
		base, head string
		want       []Change
	}{
		{
			name: "a reference to a number narrowed to integer by a later member",
			base: object(`{"$ref": "#/components/schemas/Amount"}`),
			head: object(`{"allOf": [{"$ref": "#/components/schemas/Amount"}, {"type": "integer"}]}`),
			want: []Change{{Kind: Changed, Path: "M › v", Detail: "type changed from number to integer", Breaking: true}},
		},
		{
			name: "a number narrowed to integer by a later member",
			base: object(`{"type": "number"}`),
			head: object(`{"allOf": [{"type": "number"}, {"type": "integer"}]}`),
			want: []Change{{Kind: Changed, Path: "M › v", Detail: "type changed from number to integer", Breaking: true}},
		},
		{
			name: "a number narrowed to integer by the model beside it",
			base: object(`{"type": "number"}`),
			head: object(`{"$ref": "#/components/schemas/C0", "type": "number"}`),
			want: []Change{{Kind: Changed, Path: "M › v", Detail: "type changed from number to integer", Breaking: true}},
		},
		{
			name: "types that share none keep the first",
			base: members(`{"type": "object"}`, `{"type": "string"}`),
			head: members(`{"type": "object"}`, `{"type": "integer"}`),
			want: []Change{},
		},
		{
			name: "a property both members define, lowered in the second",
			base: members(`{"properties": {"name": {"type": "string", "maxLength": 50}}}`, `{"properties": {"name": {"type": "string", "maxLength": 40}}}`),
			head: members(`{"properties": {"name": {"type": "string", "maxLength": 50}}}`, `{"properties": {"name": {"type": "string", "maxLength": 30}}}`),
			want: []Change{{Kind: Changed, Path: "M › name", Detail: "maxLength lowered from 40 to 30", Breaking: true}},
		},
		{
			name: "a property both members define, lowered in the first",
			base: members(`{"properties": {"name": {"type": "string", "maxLength": 50}}}`, `{"properties": {"name": {"type": "string"}}}`),
			head: members(`{"properties": {"name": {"type": "string", "maxLength": 30}}}`, `{"properties": {"name": {"type": "string"}}}`),
			want: []Change{{Kind: Changed, Path: "M › name", Detail: "maxLength lowered from 50 to 30", Breaking: true}},
		},
		{
			name: "items both members give, lowered in the second",
			base: members(`{"type": "array", "items": {"type": "string", "maxLength": 50}}`, `{"items": {"type": "string", "maxLength": 40}}`),
			head: members(`{"type": "array", "items": {"type": "string", "maxLength": 50}}`, `{"items": {"type": "string", "maxLength": 30}}`),
			want: []Change{{Kind: Changed, Path: "M[]", Detail: "maxLength lowered from 40 to 30", Breaking: true}},
		},
		{
			name: "additional properties both members give, lowered in the second",
			base: members(`{"type": "object", "additionalProperties": {"type": "string", "maxLength": 50}}`, `{"additionalProperties": {"type": "string", "maxLength": 40}}`),
			head: members(`{"type": "object", "additionalProperties": {"type": "string", "maxLength": 50}}`, `{"additionalProperties": {"type": "string", "maxLength": 30}}`),
			want: []Change{{Kind: Changed, Path: "M › <key>", Detail: "maxLength lowered from 40 to 30", Breaking: true}},
		},
		{
			name: "additional properties refused by the second member",
			base: members(`{"type": "object", "additionalProperties": {"type": "string"}}`, `{"type": "object"}`),
			head: members(`{"type": "object", "additionalProperties": {"type": "string"}}`, `{"type": "object", "additionalProperties": false}`),
			want: []Change{{Kind: Changed, Path: "M", Detail: "additional properties no longer allowed", Breaking: true}},
		},
	} {
		r := diffWithin(t, opsSpec(t, ``, c.base), opsSpec(t, ``, c.head))
		require.Equal(t, c.want, publicChanges(r.Changes), "%s\n%s", c.name, r.Markdown())
	}
}

// TestDiffDiscriminator checks the changes of a discriminator and of its
// mapping, used in a request and in a response, with the reason for each
// flag.
func TestDiffDiscriminator(t *testing.T) {
	const variants = `"Cat": {"type": "object", "properties": {"kind": {"type": "string"}, "type": {"type": "string"}}}, "Dog": {"type": "object", "properties": {"kind": {"type": "string"}, "type": {"type": "string"}}}, "Wolf": {"type": "object"}`
	pet := func(discriminator string) string {
		s := `"Pet": {"oneOf": [{"$ref": "#/components/schemas/Cat"}, {"$ref": "#/components/schemas/Dog"}]`
		if discriminator != "" {
			s += `, "discriminator": ` + discriminator
		}
		return variants + ", " + s + "}"
	}
	const paths = `"/w": {"post": {"requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Pet"}}}}, "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Pet"}}}}}}}`
	for _, c := range []struct {
		why        string
		base, head string
		detail     string
		req, resp  bool
	}{
		{"a property must now pick the variant, so a request that left it out is refused", ``, `{"propertyName": "kind"}`, "discriminator added: kind", true, false},
		{"a response no longer says which variant it is by that property", `{"propertyName": "kind"}`, ``, "discriminator removed (was kind)", false, true},
		{"clients write and read the variant in another property", `{"propertyName": "kind"}`, `{"propertyName": "type"}`, "discriminator property changed from kind to type", true, true},
		{"a value is added, as enum values are, which no request breaks on and a response only carries more", `{"propertyName": "kind"}`, `{"propertyName": "kind", "mapping": {"cat": "#/components/schemas/Cat"}}`, `discriminator mapping added: "cat" → Cat`, false, false},
		{"a value a request could send is refused", `{"propertyName": "kind", "mapping": {"cat": "#/components/schemas/Cat"}}`, `{"propertyName": "kind"}`, `discriminator mapping removed: "cat" → Cat`, true, false},
		{"a value now picks another variant, which way it moves is not known", `{"propertyName": "kind", "mapping": {"dog": "#/components/schemas/Dog"}}`, `{"propertyName": "kind", "mapping": {"dog": "Wolf"}}`, `discriminator mapping "dog" changed from Dog to Wolf`, true, true},
	} {
		r := Diff(opsSpec(t, paths, pet(c.base)), opsSpec(t, paths, pet(c.head)))
		require.Equal(t, []Change{
			{Kind: Changed, Path: "POST /w › request body", Detail: "model Pet changed", Breaking: c.req},
			{Kind: Changed, Path: "POST /w › response 200", Detail: "model Pet changed", Breaking: c.resp},
			{Kind: Changed, Path: "Pet", Detail: c.detail, Breaking: c.req || c.resp},
		}, publicChanges(r.Changes), "%s\n%s", c.why, r.Markdown())
	}

	// A mapping value written as a name or as a reference to the same
	// component is the same.
	r := Diff(opsSpec(t, paths, pet(`{"propertyName": "kind", "mapping": {"cat": "Cat"}}`)), opsSpec(t, paths, pet(`{"propertyName": "kind", "mapping": {"cat": "#/components/schemas/Cat"}}`)))
	require.True(t, r.Empty(), r.Markdown())
}

// TestDiffParameterSerialization checks that a parameter's style or explode
// changed is reported, compared with the defaults of its location filled
// in, with the reason for each flag.
func TestDiffParameterSerialization(t *testing.T) {
	param := func(in, schema, extra string) string {
		required := ""
		if in == "path" {
			required = `, "required": true`
		}
		return `"/r/{p}": {"get": {"parameters": [{"name": "p", "in": "` + in + `"` + required + `, "schema": ` + schema + extra + `}], "responses": {"204": {"description": "ok"}}}}`
	}
	const array = `{"type": "array", "items": {"type": "string"}}`
	const integer = `{"type": "integer"}`
	for _, c := range []struct {
		why        string
		in, schema string
		base, head string
		want       []string
		breaking   bool
	}{
		{"an array written id=1&id=2 is now sent as id=1,2, which the server reads as one value", "query", array, `, "explode": true`, `, "explode": false`, []string{"explode changed from true to false"}, true},
		{"form explodes by default, so false is the same change", "query", array, ``, `, "explode": false`, []string{"explode changed from true to false"}, true},
		{"the default written out is no change", "query", array, ``, `, "style": "form", "explode": true`, nil, false},
		{"the default of a path parameter is simple without explode", "path", array, ``, `, "style": "simple", "explode": false`, nil, false},
		{"a header parameter is simple by default", "header", array, ``, `, "style": "form"`, []string{"style changed from simple to form", "explode changed from false to true"}, true},
		{"items are now separated by | where they were by ,", "query", array, `, "explode": false`, `, "style": "pipeDelimited"`, []string{"style changed from form to pipeDelimited"}, true},
		{"a path value is now written .5 where it was 5", "path", integer, ``, `, "style": "label"`, []string{"style changed from simple to label"}, true},
		{"one integer is written the same exploded or not", "query", integer, ``, `, "explode": false`, []string{"explode changed from true to false"}, false},
		{"a schema of no type may hold an array", "query", `{}`, ``, `, "explode": false`, []string{"explode changed from true to false"}, true},
	} {
		r := Diff(opsSpec(t, param(c.in, c.schema, c.base), ``), opsSpec(t, param(c.in, c.schema, c.head), ``))
		want := []Change{}
		for _, detail := range c.want {
			want = append(want, Change{Kind: Changed, Path: "GET /r/{p} › " + c.in + " › p", Detail: detail, Breaking: c.breaking})
		}
		require.Equal(t, want, publicChanges(r.Changes), "%s\n%s", c.why, r.Markdown())
	}

	// A parameter described by content has no style.
	content := func(explode string) string {
		return `"/r": {"get": {"parameters": [{"name": "p", "in": "query", "content": {"application/json": {"schema": {"type": "object"}}}` + explode + `}], "responses": {"204": {"description": "ok"}}}}`
	}
	r := Diff(opsSpec(t, content(``), ``), opsSpec(t, content(`, "explode": false`), ``))
	require.True(t, r.Empty(), r.Markdown())
}
