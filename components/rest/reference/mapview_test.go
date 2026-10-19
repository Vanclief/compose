package reference

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

var mapDataPattern = regexp.MustCompile(`(?s)<script type="application/json" id="schema-map-data">(.*?)</script>`)

func renderPage(t *testing.T, doc *Document) string {
	t.Helper()
	out, err := Render(doc, Options{AssetsURL: "/assets"})
	require.NoError(t, err)
	return string(out)
}

func TestRenderSchemaMap(t *testing.T) {
	page := renderPage(t, loadEmma(t, "emma.yaml"))

	matches := mapDataPattern.FindAllStringSubmatch(page, -1)
	require.Len(t, matches, 1, "the graph is written once")
	require.Contains(t, page, `<section class="map" id="section/map"`)
	require.Contains(t, page, `data-view="map"`)
	require.Contains(t, page, `<a class="nav-link nav-map" href="#map" data-view="map">`, "the sidebar opens the map too")

	var data struct {
		Nodes []*Node  `json:"nodes"`
		Edges []*Edge  `json:"edges"`
		Tags  []mapTag `json:"tags"`
	}
	require.NoError(t, json.Unmarshal([]byte(matches[0][1]), &data))
	require.NotEmpty(t, data.Edges)

	var ticket *Node
	for _, n := range data.Nodes {
		if n.Name == "application_models_coordination_Ticket" {
			ticket = n
		}
		require.Contains(t, page, `id="`+n.Anchor+`"`, "map anchors point at model cards")
	}
	require.NotNil(t, ticket)
	require.Equal(t, "Ticket", ticket.Short)

	tags := map[string][]string{}
	for _, tag := range data.Tags {
		tags[tag.Name] = tag.Nodes
	}
	require.NotEmpty(t, tags)
	found := false
	for _, nodes := range tags {
		for _, name := range nodes {
			if name == ticket.Name {
				found = true
			}
		}
	}
	require.True(t, found, "some tag reaches Ticket")
}

func TestSchemaMapScopes(t *testing.T) {
	page := renderPage(t, loadEmma(t, "emma.yaml"))
	matches := mapDataPattern.FindAllStringSubmatch(page, -1)
	require.Len(t, matches, 1)

	var data struct {
		Nodes   []*Node  `json:"nodes"`
		Tags    []mapTag `json:"tags"`
		Default string   `json:"default"`
		Spec    string   `json:"spec"`
	}
	require.NoError(t, json.Unmarshal([]byte(matches[0][1]), &data))

	// Scopes are the operation tags in page order; module groups such as
	// hik or leads are not scopes.
	var names []string
	for _, tag := range data.Tags {
		names = append(names, tag.Name)
		require.NotEmpty(t, tag.Nodes)
	}
	require.Equal(t, []string{
		"Access Control", "Amenities", "CRM", "Finance", "Operations", "Organizations",
		"Packages", "People", "Property", "Safety", "Users", "Visitors",
	}, names)
	require.Equal(t, "Access Control", data.Default)
	require.NotEmpty(t, data.Spec)

	hubs := 0
	for _, n := range data.Nodes {
		if n.Hub {
			hubs++
		}
	}
	require.Equal(t, 6, hubs, "hub flags reach the page")
}

func TestDefaultScope(t *testing.T) {
	require.Equal(t, "", defaultScope(nil))
	require.Equal(t, "", defaultScope([]mapTag{{Name: "a", Nodes: []string{"X"}}}))
	require.Equal(t, "b", defaultScope([]mapTag{
		{Name: "a", Nodes: []string{"X"}},
		{Name: "b", Nodes: []string{"X", "Y"}},
		{Name: "c", Nodes: []string{"X", "Y", "Z"}},
	}))
}

func TestRenderSchemaMapEscapesScript(t *testing.T) {
	doc, err := Parse([]byte(`{
  "openapi": "3.1.0",
  "info": {"title": "t", "version": "1"},
  "components": {"schemas": {
    "Thing": {"type": "object", "properties": {
      "id": {"type": "string"},
      "kind": {"type": "string", "enum": ["</script><script>alert(1)</script>", "<!-- x", "a & b"]}
    }}
  }}
}`))
	require.NoError(t, err)
	page := renderPage(t, doc)

	matches := mapDataPattern.FindAllStringSubmatch(page, -1)
	require.Len(t, matches, 1)
	payload := matches[0][1]
	require.NotContains(t, payload, "</")
	require.NotContains(t, payload, "<!--")

	var data Graph
	require.NoError(t, json.Unmarshal([]byte(payload), &data))
	require.Equal(t, []string{"</script><script>alert(1)</script>", "<!-- x", "a & b"}, data.Nodes[0].Fields[1].Enum)
}

func TestRenderWithoutEntitiesHasNoMap(t *testing.T) {
	doc, err := Parse([]byte(`{
  "openapi": "3.1.0",
  "info": {"title": "t", "version": "1"},
  "components": {"schemas": {"Status": {"type": "string", "enum": ["a"]}}}
}`))
	require.NoError(t, err)
	page := renderPage(t, doc)
	require.NotContains(t, page, "schema-map-data")
	require.NotContains(t, page, "view-switch")
	require.False(t, strings.Contains(page, `id="section/map"`))
}
