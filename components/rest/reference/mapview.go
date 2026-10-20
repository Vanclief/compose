package reference

import (
	"bytes"
	"encoding/json"
	"html/template"
	"reflect"
	"strings"
)

// mapView is the schema map: the entity graph, written once into the page
// as JSON that reference.js lays out and draws. Diff is true in diff mode,
// where the graph carries the changes since the base document.
type mapView struct {
	JSON template.JS
	Diff bool
}

// Map diff states, see Node.Diff.
const (
	diffAdded   = "a"
	diffChanged = "c"
	diffRemoved = "d"
)

// mapData is the JSON payload. Tags lists, per operation tag, the entities
// its operations send or receive: the scopes of the map besides all
// entities, which it opens on. Spec names the document (title and version)
// so saved card positions stay with it.
type mapData struct {
	*Graph
	Tags []mapTag `json:"tags,omitempty"`
	Spec string   `json:"spec"`
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
func (b *builder) buildMap() *mapView {
	g := b.d.graph(b.short)
	if b.diff != nil {
		b.diffGraph(g)
	}
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

	data := mapData{Graph: g, Tags: b.mapTags(entities)}
	data.Spec = strings.TrimSpace(b.d.Info.Title + " " + b.d.Info.Version)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	err := enc.Encode(data)
	if err != nil {
		return nil
	}
	out := escapeScriptJSON(strings.TrimSpace(buf.String()))
	return &mapView{JSON: template.JS(out), Diff: b.diff != nil}
}

// diffGraph marks what changed since the base document: the entities the
// report counts as added, changed or removed, the fields added, changed or
// removed on a changed entity, and the relations added or removed. The
// removed entities, fields and relations come from the base graph, so the
// map can still draw them.
func (b *builder) diffGraph(g *Graph) {
	base := b.diff.base.graph(b.short)
	old := map[string]*Node{}
	for _, n := range base.Nodes {
		old[n.Name] = n
	}

	state := map[string]string{}
	for _, n := range g.Nodes {
		md := b.diff.report.Model(n.Name)
		if md == nil {
			continue
		}
		n.Diff = diffKind(md.Kind)
		state[n.Name] = n.Diff
		prev := old[n.Name]
		if md.Kind == Changed && prev != nil {
			n.Fields = diffFields(prev.Fields, n.Fields)
		}
	}
	shown := map[string]bool{}
	for _, n := range g.Nodes {
		shown[n.Name] = true
	}
	for _, name := range b.diff.removedModels(b.d) {
		n := old[name]
		if n == nil {
			continue
		}
		n.Diff = diffRemoved
		n.Hub = false
		shown[name] = true
		g.Nodes = append(g.Nodes, n)
	}

	// A relation is added when both its ends were on the base map or are
	// new models: a model that only now became an entity brings its
	// relations along unmarked.
	known := func(name string) bool {
		return old[name] != nil || state[name] == diffAdded
	}
	was := map[string]bool{}
	for _, e := range base.Edges {
		was[edgeKey(e)] = true
	}
	is := map[string]bool{}
	for _, e := range g.Edges {
		key := edgeKey(e)
		is[key] = true
		if !was[key] && known(e.From) && known(e.To) {
			e.Diff = diffAdded
		}
	}
	for _, e := range base.Edges {
		if !is[edgeKey(e)] && shown[e.From] && shown[e.To] {
			e.Diff = diffRemoved
			g.Edges = append(g.Edges, e)
		}
	}
}

func diffKind(k ChangeKind) string {
	switch k {
	case Added:
		return diffAdded
	case Removed:
		return diffRemoved
	}
	return diffChanged
}

// diffFields returns the head fields marked against the base ones, with the
// removed fields back in place: each one after the field that preceded it
// in the base document.
func diffFields(base, head []Field) []Field {
	before := map[string]Field{}
	for _, f := range base {
		before[f.Name] = f
	}
	after := map[string]bool{}
	for _, f := range head {
		after[f.Name] = true
	}

	// gone[name] lists the removed fields that follow the base field name
	// ("" for those before the first one).
	gone := map[string][]Field{}
	prev := ""
	for _, f := range base {
		if after[f.Name] {
			prev = f.Name
			continue
		}
		f.Diff = diffRemoved
		gone[prev] = append(gone[prev], f)
	}

	out := append([]Field{}, gone[""]...)
	for _, f := range head {
		was, ok := before[f.Name]
		if !ok {
			f.Diff = diffAdded
		} else if !reflect.DeepEqual(was, f) {
			f.Diff = diffChanged
		}
		out = append(out, f)
		out = append(out, gone[f.Name]...)
	}
	return out
}

// mapTags collects the entities each tag's operations reach through their
// request bodies and responses, in the document's tag order, see tagNames.
// Walking stops at an entity: what it points at is one hop away on the map.
func (b *builder) mapTags(entities map[string]bool) []mapTag {
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
	for _, tag := range b.d.tagNames() {
		names := reached[tag]
		if len(names) > 0 {
			out = append(out, mapTag{Name: tag, Nodes: names})
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
