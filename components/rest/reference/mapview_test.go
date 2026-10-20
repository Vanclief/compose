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
	require.Contains(t, page, `<a href="#map" data-mode="map">Schema Map</a>`, "the app bar opens the map")

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
		Nodes []*Node  `json:"nodes"`
		Tags  []mapTag `json:"tags"`
		Spec  string   `json:"spec"`
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
	var top map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(matches[0][1]), &top))
	require.NotContains(t, top, "default", "the map opens on all models")
	require.NotEmpty(t, data.Spec)

	hubs := 0
	for _, n := range data.Nodes {
		if n.Hub {
			hubs++
		}
	}
	require.Equal(t, 6, hubs, "hub flags reach the page")
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

func TestSchemaMapDiff(t *testing.T) {
	base := `{
  "openapi": "3.1.0",
  "info": {"title": "t", "version": "1.0"},
  "paths": {},
  "components": {"schemas": {
    "Pet": {"type": "object", "properties": {
      "id": {"type": "string"},
      "legacy": {"type": "string"},
      "owner": {"$ref": "#/components/schemas/Owner"},
      "relic": {"$ref": "#/components/schemas/Legacy"}
    }},
    "Owner": {"type": "object", "properties": {"id": {"type": "string"}, "name": {"type": "string"}}},
    "Legacy": {"type": "object", "properties": {"id": {"type": "string"}, "code": {"type": "string"}}}
  }}
}`
	head := `{
  "openapi": "3.1.0",
  "info": {"title": "t", "version": "1.1"},
  "paths": {},
  "components": {"schemas": {
    "Pet": {"type": "object", "properties": {
      "id": {"type": "integer"},
      "owner": {"$ref": "#/components/schemas/Owner"},
      "tag": {"$ref": "#/components/schemas/Tag"}
    }},
    "Owner": {"type": "object", "properties": {"id": {"type": "string"}, "name": {"type": "string"}, "email": {"type": "string"}}},
    "Tag": {"type": "object", "properties": {"id": {"type": "string"}}}
  }}
}`
	doc, err := Parse([]byte(head))
	require.NoError(t, err)

	plain := renderPage(t, doc)
	matches := mapDataPattern.FindAllStringSubmatch(plain, -1)
	require.Len(t, matches, 1)
	require.NotContains(t, matches[0][1], `"diff"`, "no diff states without a base")
	require.NotContains(t, plain, `lg-diff`)
	require.Contains(t, plain, `data-map-fold-all`, "cards show all their fields or collapse, all at once from the toolbar")

	out, err := Render(doc, Options{Base: []byte(base)})
	require.NoError(t, err)
	page := string(out)
	require.Contains(t, page, `lg-diff`, "the legend explains the diff colors")
	matches = mapDataPattern.FindAllStringSubmatch(page, -1)
	require.Len(t, matches, 1)
	var data struct {
		Nodes []*Node `json:"nodes"`
		Edges []*Edge `json:"edges"`
	}
	require.NoError(t, json.Unmarshal([]byte(matches[0][1]), &data))

	nodes := map[string]*Node{}
	var order []string
	for _, n := range data.Nodes {
		nodes[n.Name] = n
		order = append(order, n.Name)
	}
	require.Equal(t, []string{"Pet", "Owner", "Tag", "Legacy"}, order, "removed entities follow the head ones")
	require.Equal(t, diffChanged, nodes["Pet"].Diff)
	require.Equal(t, diffChanged, nodes["Owner"].Diff)
	require.Equal(t, diffAdded, nodes["Tag"].Diff)
	require.Equal(t, diffRemoved, nodes["Legacy"].Diff)
	require.Equal(t, "model/Legacy", nodes["Legacy"].Anchor, "a removed entity opens its removed model card")

	fields := func(n *Node) []string {
		var out []string
		for _, f := range n.Fields {
			out = append(out, f.Name+":"+f.Diff)
		}
		return out
	}
	require.Equal(t, []string{"id:c", "legacy:d", "owner:", "relic:d", "tag:a"}, fields(nodes["Pet"]), "removed fields stay where they were")
	require.Equal(t, []string{"id:", "name:", "email:a"}, fields(nodes["Owner"]))
	require.Equal(t, []string{"id:"}, fields(nodes["Tag"]), "an added entity's fields are not marked one by one")

	edges := map[string]string{}
	for _, e := range data.Edges {
		edges[e.From+">"+e.To] = e.Diff
	}
	require.Equal(t, map[string]string{
		"Pet>Owner":  "",
		"Pet>Tag":    diffAdded,
		"Pet>Legacy": diffRemoved,
	}, edges)
}
