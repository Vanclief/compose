package reference

import (
	"bytes"
	"encoding/json"
	"html/template"
	"strings"
)

// mapView is the schema map: the entity graph, written once into the page
// as JSON that reference.js lays out and draws.
type mapView struct {
	JSON template.JS
}

// mapData is the JSON payload. Tags lists, per operation tag, the entities
// its operations send or receive, for the scope selector.
type mapData struct {
	*Graph
	Tags []mapTag `json:"tags,omitempty"`
}

type mapTag struct {
	Name  string   `json:"name"`
	Nodes []string `json:"nodes"`
}

// escapeScriptJSON keeps a JSON payload from closing its <script> element
// or opening an HTML comment inside it. Both rewrites are valid JSON: an
// escaped slash, and the exclamation mark as a unicode escape.
func escapeScriptJSON(s string) string {
	s = strings.ReplaceAll(s, "</", `<\/`)
	return strings.ReplaceAll(s, "<!--", `<\u0021--`)
}

// buildMap returns nil when the document has no entities, which leaves the
// map and its view switcher out of the page.
func (b *builder) buildMap(tags []*tagView) *mapView {
	g := b.d.Graph()
	if len(g.Nodes) == 0 {
		return nil
	}

	entities := map[string]bool{}
	for _, n := range g.Nodes {
		entities[n.Name] = true
		anchor, ok := b.models[n.Name]
		if ok {
			n.Anchor = anchor
		}
	}

	data := mapData{Graph: g, Tags: b.mapTags(tags, entities)}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	err := enc.Encode(data)
	if err != nil {
		return nil
	}
	out := escapeScriptJSON(strings.TrimSpace(buf.String()))
	return &mapView{JSON: template.JS(out)}
}

// mapTags collects the entities each tag's operations reach through their
// request bodies and responses, in the order the page lists the tags.
// Walking stops at an entity: what it points at is one hop away on the map.
func (b *builder) mapTags(tags []*tagView, entities map[string]bool) []mapTag {
	reached := map[string][]string{}
	for _, ref := range b.d.Operations() {
		names := b.operationEntities(ref.Operation, entities)
		if len(names) == 0 {
			continue
		}
		opTags := ref.Operation.Tags
		if len(opTags) == 0 {
			opTags = []string{"Other"}
		}
		for _, tag := range opTags {
			reached[tag] = appendUnique(reached[tag], names...)
		}
	}

	var out []mapTag
	for _, tv := range tags {
		names := reached[tv.Name]
		if len(names) > 0 {
			out = append(out, mapTag{Name: tv.Name, Nodes: names})
		}
	}
	return out
}

func (b *builder) operationEntities(op *Operation, entities map[string]bool) []string {
	var out []string
	seen := map[*Schema]bool{}
	visit := func(content Ordered[*MediaType]) {
		for _, key := range content.Keys {
			mt := content.Map[key]
			if mt != nil {
				out = b.schemaEntities(mt.Schema, entities, seen, out, 0)
			}
		}
	}

	body := b.d.RequestBody(op.RequestBody)
	if body != nil {
		visit(body.Content)
	}
	for _, status := range op.Responses.Keys {
		res := b.d.Response(op.Responses.Map[status])
		if res != nil {
			visit(res.Content)
		}
	}
	return out
}

func (b *builder) schemaEntities(s *Schema, entities map[string]bool, seen map[*Schema]bool, out []string, depth int) []string {
	if s == nil || depth > 8 {
		return out
	}
	name := b.d.SchemaName(s)
	if entities[name] {
		return appendUnique(out, name)
	}
	f := b.d.Flatten(s)
	if f == nil || seen[f] {
		return out
	}
	seen[f] = true

	for _, key := range f.Properties.Keys {
		out = b.schemaEntities(f.Properties.Map[key], entities, seen, out, depth+1)
	}
	out = b.schemaEntities(f.Items, entities, seen, out, depth+1)
	out = b.schemaEntities(f.AdditionalSchema(), entities, seen, out, depth+1)
	for _, list := range [][]*Schema{f.OneOf, f.AnyOf} {
		for _, member := range list {
			out = b.schemaEntities(member, entities, seen, out, depth+1)
		}
	}
	return out
}

func appendUnique(list []string, values ...string) []string {
	for _, v := range values {
		found := false
		for _, existing := range list {
			if existing == v {
				found = true
				break
			}
		}
		if !found {
			list = append(list, v)
		}
	}
	return list
}
