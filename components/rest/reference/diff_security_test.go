package reference

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// securitySchemesJSON defines the schemes the security tests name.
const securitySchemesJSON = `{
	"k": {"type": "apiKey", "in": "header", "name": "X-Key"},
	"o": {"type": "apiKey", "in": "header", "name": "X-Other"},
	"oauth": {"type": "oauth2", "flows": {"clientCredentials": {"tokenUrl": "https://auth.example.com/token", "scopes": {"read": "r", "write": "w"}}}}
}`

// securityDoc is a document with one operation, GET /a, whose security is
// op, unset when empty, under the document security global, unset when
// empty, with schemes as components.securitySchemes.
func securityDoc(t *testing.T, global, op, schemes string) []byte {
	t.Helper()
	doc := `{"openapi": "3.1.0", "info": {"title": "t", "version": "1"},`
	if global != "" {
		doc += `"security": ` + global + `,`
	}
	opSecurity := ""
	if op != "" {
		opSecurity = `"security": ` + op + `,`
	}
	doc += `"paths": {"/a": {"get": {` + opSecurity + `"responses": {"200": {"description": "ok"}}}}},`
	doc += `"components": {"securitySchemes": ` + schemes + `}}`
	return []byte(doc)
}

func parseSecurityDoc(t *testing.T, global, op, schemes string) *Document {
	t.Helper()
	doc, err := Parse(securityDoc(t, global, op, schemes))
	require.NoError(t, err)
	return doc
}

// TestDiffSecurity checks that a change of an operation's security breaks
// a request unless every alternative the base accepted is still enough in
// the head, and that alternatives, schemes and scopes are compared as sets.
func TestDiffSecurity(t *testing.T) {
	tests := []struct {
		name               string
		baseGlobal, baseOp string
		headGlobal, headOp string
		// detail is the one change expected, "" for none.
		detail   string
		breaking bool
	}{
		{name: "required key added", headOp: `[{"k": []}]`, detail: "security changed from none to k", breaking: true},
		{name: "required key removed", baseOp: `[{"k": []}]`, detail: "security changed from k to none"},
		{name: "second way in added", baseOp: `[{"k": []}]`, headOp: `[{"k": []}, {"o": []}]`, detail: "security changed from k to k or o"},
		{name: "way in removed", baseOp: `[{"k": []}, {"o": []}]`, headOp: `[{"k": []}]`, detail: "security changed from k or o to k", breaking: true},
		{name: "scheme required together", baseOp: `[{"k": []}]`, headOp: `[{"k": [], "o": []}]`, detail: "security changed from k to k + o", breaking: true},
		{name: "scheme no longer required together", baseOp: `[{"k": [], "o": []}]`, headOp: `[{"k": []}]`, detail: "security changed from k + o to k"},
		{name: "scheme replaced", baseOp: `[{"k": []}]`, headOp: `[{"o": []}]`, detail: "security changed from k to o", breaking: true},
		{name: "scope added", baseOp: `[{"oauth": ["read"]}]`, headOp: `[{"oauth": ["read", "write"]}]`, detail: "security changed from oauth (read) to oauth (read, write)", breaking: true},
		{name: "scope dropped", baseOp: `[{"oauth": ["read", "write"]}]`, headOp: `[{"oauth": ["read"]}]`, detail: "security changed from oauth (read, write) to oauth (read)"},
		{name: "scope replaced", baseOp: `[{"oauth": ["read"]}]`, headOp: `[{"oauth": ["write"]}]`, detail: "security changed from oauth (read) to oauth (write)", breaking: true},
		{name: "alternatives reordered", baseOp: `[{"k": []}, {"o": []}]`, headOp: `[{"o": []}, {"k": []}]`},
		{name: "schemes reordered", baseOp: `[{"k": [], "o": []}]`, headOp: `[{"o": [], "k": []}]`},
		{name: "scopes reordered", baseOp: `[{"oauth": ["read", "write"]}]`, headOp: `[{"oauth": ["write", "read"]}]`},
		{name: "scope repeated", baseOp: `[{"oauth": ["read"]}]`, headOp: `[{"oauth": ["read", "read"]}]`},
		{name: "alternative repeated", baseOp: `[{"k": []}]`, headOp: `[{"k": []}, {"k": []}]`},
		{name: "empty list and empty alternative", baseOp: `[]`, headOp: `[{}]`},
		{name: "optional added", baseOp: `[{"k": []}]`, headOp: `[{}, {"k": []}]`, detail: "security changed from k to optional or k"},
		{name: "optional removed", baseOp: `[{}, {"k": []}]`, headOp: `[{"k": []}]`, detail: "security changed from optional or k to k", breaking: true},
		{name: "redundant alternative added", baseOp: `[{"k": []}]`, headOp: `[{"k": []}, {"k": [], "o": []}]`, detail: "security changed from k to k or k + o"},
		{name: "document default changed", baseGlobal: `[{"k": []}]`, headGlobal: `[{"o": []}]`, detail: "security changed from k to o", breaking: true},
		{name: "document default loosened", baseGlobal: `[{"k": [], "o": []}]`, headGlobal: `[{"k": []}]`, detail: "security changed from k + o to k"},
		{name: "document default changed under an override", baseGlobal: `[{"k": []}]`, baseOp: `[{"k": []}]`, headGlobal: `[{"o": []}]`, headOp: `[{"k": []}]`},
		{name: "override replaces the same default", baseGlobal: `[{"k": []}]`, headGlobal: `[{"k": []}]`, headOp: `[{"k": []}]`},
		{name: "explicit empty list under a default", baseGlobal: `[{"k": []}]`, headGlobal: `[{"k": []}]`, headOp: `[]`, detail: "security changed from k to none"},
		{name: "explicit empty list dropped under a default", baseGlobal: `[{"k": []}]`, baseOp: `[]`, headGlobal: `[{"k": []}]`, detail: "security changed from none to k", breaking: true},
		{name: "default moved to the operation", baseGlobal: `[{"k": []}]`, headOp: `[{"k": []}]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := parseSecurityDoc(t, tt.baseGlobal, tt.baseOp, securitySchemesJSON)
			head := parseSecurityDoc(t, tt.headGlobal, tt.headOp, securitySchemesJSON)
			r := Diff(base, head)
			if tt.detail == "" {
				require.Empty(t, changeLines(r))
				return
			}
			require.Equal(t, []string{"GET /a | " + tt.detail}, changeLines(r))
			c := r.Changes[0]
			require.Equal(t, Changed, c.Kind)
			require.Equal(t, tt.breaking, c.Breaking)
			require.Equal(t, tt.breaking, r.Breaking())
			require.Equal(t, tt.breaking, r.Operation("GET", "/a").Breaking)
		})
	}
}

// TestDiffSecuritySchemes checks that a change of what a client sends for
// a scheme both sides of an operation name is reported under the
// operation, breaking where a client sending what it sent before is
// refused, and that the flows of an oauth2 scheme and the openIdConnectUrl
// are not compared.
func TestDiffSecuritySchemes(t *testing.T) {
	const op = `[{"s": []}]`
	const oauth = `{"type": "oauth2", "flows": {"clientCredentials": {"tokenUrl": "https://a.example.com/token", "scopes": {"read": "r"}}}}`
	type line struct {
		path, detail string
		breaking     bool
	}
	tests := []struct {
		name string
		b, h string
		want []line
	}{
		{
			name: "header name changed",
			b:    `{"type": "apiKey", "in": "header", "name": "X-Key"}`,
			h:    `{"type": "apiKey", "in": "header", "name": "X-Api-Key"}`,
			want: []line{{"", "name changed from X-Key to X-Api-Key", true}},
		},
		{
			name: "header name case changed",
			b:    `{"type": "apiKey", "in": "header", "name": "X-Key"}`,
			h:    `{"type": "apiKey", "in": "header", "name": "x-key"}`,
		},
		{
			name: "query name case changed",
			b:    `{"type": "apiKey", "in": "query", "name": "key"}`,
			h:    `{"type": "apiKey", "in": "query", "name": "Key"}`,
			want: []line{{"", "name changed from key to Key", true}},
		},
		{
			name: "moved to the query",
			b:    `{"type": "apiKey", "in": "header", "name": "key"}`,
			h:    `{"type": "apiKey", "in": "query", "name": "key"}`,
			want: []line{{"", "in changed from header to query", true}},
		},
		{
			name: "type changed",
			b:    `{"type": "apiKey", "in": "header", "name": "X-Key"}`,
			h:    `{"type": "http", "scheme": "bearer"}`,
			want: []line{{"", "type changed from apiKey to http", true}},
		},
		{
			name: "http scheme changed",
			b:    `{"type": "http", "scheme": "basic"}`,
			h:    `{"type": "http", "scheme": "bearer"}`,
			want: []line{{"", "scheme changed from basic to bearer", true}},
		},
		{
			name: "http scheme case changed",
			b:    `{"type": "http", "scheme": "Bearer"}`,
			h:    `{"type": "http", "scheme": "bearer"}`,
		},
		{
			name: "bearerFormat given and description changed",
			b:    `{"type": "http", "scheme": "bearer", "description": "a"}`,
			h:    `{"type": "http", "scheme": "bearer", "bearerFormat": "JWT", "description": "b"}`,
			want: []line{{"", "bearerFormat added: JWT", false}},
		},
		{
			name: "openIdConnectUrl changed, not compared",
			b:    `{"type": "openIdConnect", "openIdConnectUrl": "https://a.example.com/.well-known/openid-configuration"}`,
			h:    `{"type": "openIdConnect", "openIdConnectUrl": "https://b.example.com/.well-known/openid-configuration"}`,
		},
		{
			name: "oauth2 flow URL changed, not compared",
			b:    oauth,
			h:    strings.ReplaceAll(oauth, "a.example.com", "b.example.com"),
		},
		{
			name: "oauth2 to apiKey",
			b:    oauth,
			h:    `{"type": "apiKey", "in": "header", "name": "X-Key"}`,
			want: []line{{"", "type changed from oauth2 to apiKey", true}},
		},
		{
			name: "moved behind a $ref, which Parse reads without its fields, not compared",
			b:    `{"type": "apiKey", "in": "header", "name": "X-Key"}`,
			h:    `{"$ref": "#/components/securitySchemes/other"}`,
		},
		{
			name: "moved out of a $ref, not compared",
			b:    `{"$ref": "#/components/securitySchemes/other"}`,
			h:    `{"type": "apiKey", "in": "header", "name": "X-Key"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := parseSecurityDoc(t, "", op, `{"s": `+tt.b+`}`)
			head := parseSecurityDoc(t, "", op, `{"s": `+tt.h+`}`)
			r := Diff(base, head)
			var got []line
			for _, c := range r.Changes {
				where, ok := strings.CutPrefix(c.Path, "GET /a › security scheme s")
				require.True(t, ok, c.Path)
				got = append(got, line{where, c.Detail, c.Breaking})
			}
			require.Equal(t, tt.want, got)
			breaking := false
			for _, l := range tt.want {
				breaking = breaking || l.breaking
			}
			require.Equal(t, breaking, r.Breaking())
		})
	}
}

// TestDiffSecuritySchemesWhereUsed checks that a scheme's definition is
// compared only for operations whose security names it on both sides.
func TestDiffSecuritySchemesWhereUsed(t *testing.T) {
	schemes := func(name string) string {
		return `{"k": {"type": "apiKey", "in": "header", "name": "` + name + `"}, "o": {"type": "apiKey", "in": "header", "name": "X-Other"}}`
	}

	// Defined but named by no operation: nothing to report.
	r := Diff(parseSecurityDoc(t, "", "", schemes("X-Key")), parseSecurityDoc(t, "", "", schemes("X-New")))
	require.Empty(t, changeLines(r))

	// Named only by the base: the security change says it all.
	r = Diff(parseSecurityDoc(t, "", `[{"k": []}]`, schemes("X-Key")), parseSecurityDoc(t, "", `[{"o": []}]`, schemes("X-New")))
	require.Equal(t, []string{"GET /a | security changed from k to o"}, changeLines(r))

	// Under another name in the head, with the definition it had: a scheme
	// is known by its name, so it reads as replaced.
	renamed := `{"k2": {"type": "apiKey", "in": "header", "name": "X-Key"}}`
	r = Diff(parseSecurityDoc(t, "", `[{"k": []}]`, schemes("X-Key")), parseSecurityDoc(t, "", `[{"k2": []}]`, renamed))
	require.Equal(t, []string{"GET /a | security changed from k to k2"}, changeLines(r))
	require.True(t, r.Changes[0].Breaking)

	// Named by the document default on both sides, and as one alternative
	// of two in the head.
	r = Diff(parseSecurityDoc(t, `[{"k": []}]`, "", schemes("X-Key")), parseSecurityDoc(t, `[{"k": []}, {"o": []}]`, "", schemes("X-New")))
	require.Equal(t, []string{
		"GET /a | security changed from k to k or o",
		"GET /a › security scheme k | name changed from X-Key to X-New",
	}, changeLines(r))
	require.False(t, r.Changes[0].Breaking)
	require.True(t, r.Changes[1].Breaking)
}

// TestDiffSecurityMalformedFlows checks that a security scheme's flows,
// which Parse does not read, do not fail it however they are written.
func TestDiffSecurityMalformedFlows(t *testing.T) {
	for _, flows := range []string{`[1]`, `"x"`, `{"implicit": [1]}`, `{"implicit": {"scopes": {"a": 1}}}`} {
		_, err := Parse(securityDoc(t, "", `[{"s": []}]`, `{"s": {"type": "oauth2", "flows": `+flows+`}}`))
		require.NoError(t, err, flows)
	}
	_, err := Parse(securityDoc(t, "", `[{"s": []}]`, `{"s": {"type": "openIdConnect", "openIdConnectUrl": 5}}`))
	require.NoError(t, err)
}

// TestDiffSecurityKeptPerList checks that the security comparison kept per
// pair of lists depends only on the two lists: operations whose lists have
// the same length but differ, the document's and their own, each get their
// own result.
func TestDiffSecurityKeptPerList(t *testing.T) {
	doc := func(global, own1, own2 string) *Document {
		d, err := Parse([]byte(`{"openapi": "3.1.0", "info": {"title": "t", "version": "1"}, "security": ` + global + `, "paths": {
			"/a": {"get": {"responses": {"200": {"description": "ok"}}}},
			"/b": {"get": {"security": ` + own1 + `, "responses": {"200": {"description": "ok"}}}},
			"/c": {"get": {"security": ` + own2 + `, "responses": {"200": {"description": "ok"}}}},
			"/d": {"get": {"responses": {"200": {"description": "ok"}}}}}}`))
		require.NoError(t, err)
		return d
	}
	base := doc(`[{"k": []}]`, `[{"o": []}]`, `[{"k": []}]`)
	head := doc(`[{"o": []}]`, `[{"o": []}]`, `[{"k": [], "o": []}]`)
	r := Diff(base, head)
	require.Equal(t, []string{
		"GET /a | security changed from k to o",
		"GET /c | security changed from k to k + o",
		"GET /d | security changed from k to o",
	}, changeLines(r))
	for _, c := range r.Changes {
		require.True(t, c.Breaking, c.Path)
	}

	r = Diff(head, base)
	require.Equal(t, []string{
		"GET /a | security changed from o to k",
		"GET /c | security changed from k + o to k",
		"GET /d | security changed from o to k",
	}, changeLines(r))
	require.True(t, r.Changes[0].Breaking)
	require.False(t, r.Changes[1].Breaking)
	require.True(t, r.Changes[2].Breaking)
}

// TestDiffSecurityMarkdown checks the security lines in the markdown
// summary and on the reference page.
func TestDiffSecurityMarkdown(t *testing.T) {
	base := securityDoc(t, "", `[{"oauth": ["read"]}]`, securitySchemesJSON)
	head := securityDoc(t, "", `[{"oauth": ["read", "write"]}, {"k": []}]`, securitySchemesJSON)
	baseDoc, err := Parse(base)
	require.NoError(t, err)
	headDoc, err := Parse(head)
	require.NoError(t, err)

	md := Diff(baseDoc, headDoc).Markdown()
	require.Contains(t, md, "\n## Breaking changes\n\n### Other\n\n- `GET /a`: security changed from oauth (read) to oauth (read, write) or k\n")
	require.NotContains(t, md, "## Other changes")

	out, err := Render(headDoc, Options{Base: base, AssetsURL: "/assets"})
	require.NoError(t, err)
	changes := section(t, string(out), `id="section/changes"`)
	require.Contains(t, changes, `<span class="diff-detail">security changed from oauth (read) to oauth (read, write) or k</span> <span class="diff-badge diff-breaking">BREAKING</span>`)
}

// TestDiffSecurityManyAlternatives checks that comparing security with
// many alternatives of many schemes each stays cheap, where no alternative
// is in the other side as it is: on an operation of its own, and on 5,000
// operations that inherit the document's security, which is compared once.
// Compared once for each operation, the lists would take several times the
// bound.
func TestDiffSecurityManyAlternatives(t *testing.T) {
	reqs := func(prefix string) string {
		var alts []map[string][]string
		for i := range 200 {
			alt := map[string][]string{"s0": {"x", prefix + strconv.Itoa(i)}}
			for j := 1; j < 20; j++ {
				alt["s"+strconv.Itoa(j)] = []string{"x"}
			}
			alts = append(alts, alt)
		}
		data, err := json.Marshal(alts)
		require.NoError(t, err)
		return string(data)
	}
	doc := func(prefix string) *Document {
		security := reqs(prefix)
		paths := []string{`"/own": {"get": {"security": ` + security + `, "responses": {"200": {"description": "ok"}}}}`}
		for i := range 5000 {
			paths = append(paths, `"/a`+strconv.Itoa(i)+`": {"get": {"responses": {"200": {"description": "ok"}}}}`)
		}
		d, err := Parse([]byte(`{"openapi": "3.1.0", "info": {"title": "t", "version": "1"}, "security": ` + security + `, "paths": {` + strings.Join(paths, ",") + `}}`))
		require.NoError(t, err)
		return d
	}
	base := doc("b")
	head := doc("h")

	start := time.Now()
	r := diffWithin(t, base, head)
	require.Less(t, time.Since(start), 5*time.Second)
	require.Len(t, r.Changes, 5001)
	for _, c := range r.Changes {
		require.True(t, c.Breaking)
	}

	start = time.Now()
	r = diffWithin(t, base, base)
	require.Less(t, time.Since(start), 5*time.Second)
	require.Empty(t, r.Changes)
}

// TestDiffSecurityCoveredLate checks the costliest lists to compare: every
// base alternative is enough for one head alternative only, the one that
// sorts last, so each base alternative scans the whole head list. Within
// maxSecurityChecks pairs the lists are compared in full and break nothing;
// past it the comparison stops and the change reads as breaking, in a time
// that does not grow with the product of the lists.
func TestDiffSecurityCoveredLate(t *testing.T) {
	doc := func(n int, head bool) *Document {
		var alts []map[string][]string
		for i := range n {
			if head {
				alts = append(alts, map[string][]string{"h" + strconv.Itoa(i): {}})
				continue
			}
			alts = append(alts, map[string][]string{"b" + strconv.Itoa(i): {}, "zz": {}})
		}
		if head {
			alts = append(alts, map[string][]string{"zz": {}})
		}
		data, err := json.Marshal(alts)
		require.NoError(t, err)
		return parseSecurityDoc(t, "", string(data), `{}`)
	}

	// 200 base alternatives against 201 head ones: 40,200 pairs.
	r := diffWithin(t, doc(200, false), doc(200, true))
	require.Len(t, r.Changes, 1)
	require.False(t, r.Changes[0].Breaking)

	// 20,000 against 20,001: 400 million pairs, not compared to the end.
	start := time.Now()
	r = diffWithin(t, doc(20000, false), doc(20000, true))
	require.Less(t, time.Since(start), 5*time.Second)
	require.Len(t, r.Changes, 1)
	require.True(t, r.Changes[0].Breaking)

}

// TestDiffSecurityManySchemes checks that what an operation reports for
// changed scheme definitions does not grow with the schemes it names: the
// first 16 changed schemes by name are listed and the others counted in
// one line, breaking when one of them is, also on each of 2,000 operations
// that inherit a list of 1,000 changed schemes.
func TestDiffSecurityManySchemes(t *testing.T) {
	doc := func(schemes, operations int, definition func(i int) string) *Document {
		alt := map[string][]string{}
		var defs []string
		for i := range schemes {
			name := fmt.Sprintf("s%04d", i)
			alt[name] = []string{}
			defs = append(defs, `"`+name+`": `+definition(i))
		}
		security, err := json.Marshal([]map[string][]string{alt})
		require.NoError(t, err)
		var paths []string
		for i := range operations {
			paths = append(paths, `"/a`+strconv.Itoa(i)+`": {"get": {"responses": {"200": {"description": "ok"}}}}`)
		}
		d, err := Parse([]byte(`{"openapi": "3.1.0", "info": {"title": "t", "version": "1"}, "security": ` + string(security) +
			`, "paths": {` + strings.Join(paths, ",") + `}, "components": {"securitySchemes": {` + strings.Join(defs, ",") + `}}}`))
		require.NoError(t, err)
		return d
	}
	header := func(name string) func(int) string {
		return func(int) string {
			return `{"type": "apiKey", "in": "header", "name": "` + name + `"}`
		}
	}
	bearer := func(format func(i int) string, scheme func(i int) string) func(int) string {
		return func(i int) string {
			return `{"type": "http", "scheme": "` + scheme(i) + `", "bearerFormat": "` + format(i) + `"}`
		}
	}
	always := func(v string) func(int) string {
		return func(int) string {
			return v
		}
	}

	// 17 changed schemes: 16 listed, one counted.
	r := Diff(doc(17, 1, header("X-Old")), doc(17, 1, header("X-New")))
	lines := changeLines(r)
	require.Len(t, lines, 17)
	require.Equal(t, "GET /a0 › security scheme s0000 | name changed from X-Old to X-New", lines[0])
	require.Equal(t, "GET /a0 › security scheme s0015 | name changed from X-Old to X-New", lines[15])
	require.Equal(t, "GET /a0 | 1 more security scheme changed", lines[16])
	require.True(t, r.Changes[16].Breaking)

	// The counted schemes break nothing: every bearerFormat changed.
	r = Diff(doc(20, 1, bearer(always("a"), always("bearer"))), doc(20, 1, bearer(always("b"), always("bearer"))))
	lines = changeLines(r)
	require.Len(t, lines, 17)
	require.Equal(t, "GET /a0 | 4 more security schemes changed", lines[16])
	require.False(t, r.Breaking())

	// One of the counted schemes breaks: the last one changed its scheme.
	last := func(i int) string {
		if i == 19 {
			return "basic"
		}
		return "bearer"
	}
	r = Diff(doc(20, 1, bearer(always("a"), always("bearer"))), doc(20, 1, bearer(always("b"), last)))
	lines = changeLines(r)
	require.Len(t, lines, 17)
	require.Equal(t, "GET /a0 | 4 more security schemes changed", lines[16])
	require.True(t, r.Changes[16].Breaking)
	for _, c := range r.Changes[:16] {
		require.False(t, c.Breaking)
	}

	// Unchanged schemes are not counted, wherever they sort.
	some := func(i int) string {
		if i%2 == 0 {
			return "b"
		}
		return "a"
	}
	r = Diff(doc(20, 1, bearer(always("a"), always("bearer"))), doc(20, 1, bearer(some, always("bearer"))))
	require.Len(t, r.Changes, 10)

	start := time.Now()
	r = diffWithin(t, doc(1000, 2000, header("X-Old")), doc(1000, 2000, header("X-New")))
	require.Less(t, time.Since(start), 10*time.Second)
	require.Len(t, r.Changes, 2000*17)
	require.Equal(t, "984 more security schemes changed", r.Changes[16].Detail)
}

// TestDiffSecurityListReadOnce checks that what a requirement list is
// compared by is kept per list, so a long list that many operations
// compare with lists of their own is read once, and that two lists of
// equal contents are still two lists.
func TestDiffSecurityListReadOnce(t *testing.T) {
	doc := parseSecurityDoc(t, `[{"oauth": ["write", "read"]}, {"k": [], "o": []}]`, `[{"k": []}]`, securitySchemesJSON)
	d := &differ{securityLists: map[securityListKey]*securityList{}}

	l := d.securityList(doc.Security)
	require.Same(t, l, d.securityList(doc.Security))
	require.Equal(t, "oauth (read, write) or k + o", l.text)
	require.Len(t, l.alts, 2)
	require.Len(t, l.keys, 2)
	require.Equal(t, map[string]bool{"k": true, "o": true, "oauth": true}, l.names)

	same := []map[string][]string{{"oauth": {"write", "read"}}, {"k": {}, "o": {}}}
	other := d.securityList(same)
	require.NotSame(t, l, other)
	require.Equal(t, l.text, other.text)

	// Every empty list requires nothing and is one list.
	require.Same(t, d.securityList(nil), d.securityList([]map[string][]string{}))
}
