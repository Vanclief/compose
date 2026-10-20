package reference

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Graph is the entity-relationship view of a document's component schemas:
// one node per entity and one edge per relation between two entities.
type Graph struct {
	Nodes []*Node `json:"nodes"`
	Edges []*Edge `json:"edges"`
}

// Node is one entity. Name is the component name, Short its display name,
// Group the module it belongs to ("" when the names carry none) and Anchor
// the id of its model card in the reference page. Table is the database
// table the schema declares with x-table ("" when it declares none). Hub
// marks an entity that most others point at (see hubSources); the map leaves
// its edges out until asked.
type Node struct {
	Name   string  `json:"name"`
	Short  string  `json:"short"`
	Group  string  `json:"group,omitempty"`
	Anchor string  `json:"anchor"`
	Table  string  `json:"table,omitempty"`
	Hub    bool    `json:"hub,omitempty"`
	Fields []Field `json:"fields"`
}

// Field is one property of an entity. Optional means it is not listed in
// required. Ref names the entity the field points at, either as an embedded
// object or array or as a foreign key.
type Field struct {
	Name       string   `json:"name"`
	Type       string   `json:"type,omitempty"`
	Optional   bool     `json:"optional,omitempty"`
	Nullable   bool     `json:"nullable,omitempty"`
	ReadOnly   bool     `json:"readOnly,omitempty"`
	WriteOnly  bool     `json:"writeOnly,omitempty"`
	Deprecated bool     `json:"deprecated,omitempty"`
	Enum       []string `json:"enum,omitempty"`
	Ref        string   `json:"ref,omitempty"`
}

// Edge kinds.
const (
	EdgeHasOne  = "has-one"
	EdgeHasMany = "has-many"
	EdgeFK      = "fk"
)

// Edge is a relation from the entity holding the field to the entity it
// points at. Inferred is true when the relation was guessed from a foo_id
// field name rather than declared by a $ref or an x-ref extension.
type Edge struct {
	From        string `json:"from"`
	To          string `json:"to"`
	FromField   string `json:"fromField,omitempty"`
	Kind        string `json:"kind"`
	Cardinality string `json:"cardinality"`
	Inferred    bool   `json:"inferred,omitempty"`
}

// hubSources is the number of distinct entities that may point at an entity
// before it counts as a hub: Organization, Complex or User, which nearly
// every entity carries a key to.
const hubSources = 8

// wrapperSuffixes mark request and response shapes, which are not entities
// even when they carry an id.
var wrapperSuffixes = []string{"Request", "Response", "Params", "Query", "Body"}

// Graph builds the entity graph of the component schemas.
//
// When any component schema declares the table it is stored in with an
// x-table extension (see Document.Table), the entities are exactly those
// schemas. Otherwise they are guessed: an entity is a component schema that
// flattens to an object (not an enum) and either has an id property or is
// referenced by a property of another entity. Names ending in Request,
// Response, Params, Query or Body are never guessed entities, and a wrapper
// that only operations use has no id and no entity pointing at it, so it is
// left out as well.
//
// Edges come, in order of confidence, from an x-ref extension on a property
// (a $ref string naming the target), from a property that is a $ref to an
// entity (has-one) or an array of them (has-many), from a foo_id property
// next to such a foo property (the foreign key of that relation), and last
// from a foo_id property alone, matched by name to an entity called Foo
// (marked Inferred).
func (d *Document) Graph() *Graph {
	short := shortNames(d.Components.Schemas.Keys)
	declared := d.declaresTables()
	objects := d.graphObjects(declared)
	entities := d.graphEntities(objects, declared)

	g := &Graph{}
	nodes := map[string]*Node{}
	for _, name := range d.Components.Schemas.Keys {
		if !entities[name] {
			continue
		}
		node := &Node{
			Name:   name,
			Short:  short[name],
			Group:  schemaGroup(name),
			Anchor: "model/" + modelSlug(name),
			Table:  d.Table(name),
		}
		nodes[name] = node
		g.Nodes = append(g.Nodes, node)
	}

	index := newEntityIndex(g.Nodes)
	seen := map[string]bool{}
	for _, node := range g.Nodes {
		f := objects[node.Name]
		node.Fields = d.graphFields(f, short)
		for _, e := range d.graphEdges(node, f, entities, index) {
			key := e.From + "\x00" + e.To + "\x00" + e.FromField + "\x00" + e.Kind
			if seen[key] {
				continue
			}
			seen[key] = true
			g.Edges = append(g.Edges, e)
		}
	}

	markHubs(g)
	return g
}

// markHubs sets Hub on the entities that more than hubSources other
// entities point at. Self references do not count.
func markHubs(g *Graph) {
	sources := map[string]map[string]bool{}
	for _, e := range g.Edges {
		if e.From == e.To {
			continue
		}
		if sources[e.To] == nil {
			sources[e.To] = map[string]bool{}
		}
		sources[e.To][e.From] = true
	}
	for _, n := range g.Nodes {
		n.Hub = len(sources[n.Name]) > hubSources
	}
}

// Table returns the database table the component schema called name is
// stored in, as declared by its x-table extension, or "" when it declares
// none. Schemas without one are shapes that only travel over HTTP.
func (d *Document) Table(name string) string {
	s := d.Components.Schemas.Map[name]
	if s == nil {
		return ""
	}
	table, _ := s.Extensions["x-table"].(string)
	return strings.TrimSpace(table)
}

// declaresTables reports whether any component schema declares an x-table.
func (d *Document) declaresTables() bool {
	for _, name := range d.Components.Schemas.Keys {
		if d.Table(name) != "" {
			return true
		}
	}
	return false
}

// graphObjects returns the flattened component schemas that can be entities:
// objects that are not enums and, unless the document declares its tables,
// whose name is not a wrapper name.
func (d *Document) graphObjects(declared bool) map[string]*Schema {
	out := map[string]*Schema{}
	for _, name := range d.Components.Schemas.Keys {
		s := d.Components.Schemas.Map[name]
		if s == nil || (!declared && isWrapperName(name)) {
			continue
		}
		f := d.Flatten(s)
		if f == nil || len(f.Enum) > 0 || f.Const != nil || isArray(f) || !isObject(f) {
			continue
		}
		out[name] = f
	}
	return out
}

func isWrapperName(name string) bool {
	for _, suffix := range wrapperSuffixes {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

// graphEntities picks the objects that declare a table when the document
// declares any. Otherwise it picks the objects with an id property, then
// keeps adding the objects a property of an entity points at until nothing
// changes.
func (d *Document) graphEntities(objects map[string]*Schema, declared bool) map[string]bool {
	entities := map[string]bool{}
	if declared {
		for name := range objects {
			if d.Table(name) != "" {
				entities[name] = true
			}
		}
		return entities
	}
	var queue []string
	for _, name := range d.Components.Schemas.Keys {
		f, ok := objects[name]
		if !ok {
			continue
		}
		_, hasID := f.Properties.Get("id")
		if hasID {
			entities[name] = true
			queue = append(queue, name)
		}
	}

	for len(queue) > 0 {
		f := objects[queue[0]]
		queue = queue[1:]
		for _, key := range f.Properties.Keys {
			target, _ := d.propertyTarget(f.Properties.Map[key])
			_, ok := objects[target]
			if !ok || entities[target] {
				continue
			}
			entities[target] = true
			queue = append(queue, target)
		}
	}
	return entities
}

// propertyTarget returns the component a property points at and whether it
// holds many of them: the $ref itself (through single-member allOf
// wrappers), or the items of an array.
func (d *Document) propertyTarget(p *Schema) (string, bool) {
	if p == nil {
		return "", false
	}
	f := d.Flatten(p)
	if f == nil {
		return "", false
	}
	if isArray(f) {
		if f.Items == nil {
			return "", false
		}
		return d.SchemaName(f.Items), true
	}
	return d.SchemaName(p), false
}

// schemaGroup returns the module of a generated name such as
// application_models_coordination_Ticket (coordination): the segment before
// the last one, when the last one is a type name (starts upper case) and the
// segment is a package name (lower case). Other names have no group.
func schemaGroup(name string) string {
	parts := strings.Split(name, "_")
	if len(parts) < 3 {
		return ""
	}
	last := parts[len(parts)-1]
	group := parts[len(parts)-2]
	first, _ := utf8.DecodeRuneInString(last)
	if !unicode.IsUpper(first) || group == "" || strings.ToLower(group) != group {
		return ""
	}
	return group
}

// graphFields lists the properties of f, id first and the rest in document
// order.
func (d *Document) graphFields(f *Schema, short map[string]string) []Field {
	var keys []string
	_, hasID := f.Properties.Get("id")
	if hasID {
		keys = append(keys, "id")
	}
	for _, key := range f.Properties.Keys {
		if key != "id" {
			keys = append(keys, key)
		}
	}

	out := make([]Field, 0, len(keys))
	for _, key := range keys {
		p := f.Properties.Map[key]
		field := Field{Name: key, Optional: !f.IsRequired(key)}
		if p != nil {
			pf := d.Flatten(p)
			if pf == nil {
				pf = p
			}
			field.Type = d.graphType(p, short, 0)
			field.Nullable = p.Nullable || pf.Nullable
			field.ReadOnly = p.ReadOnly || pf.ReadOnly
			field.WriteOnly = p.WriteOnly || pf.WriteOnly
			field.Deprecated = p.Deprecated || pf.Deprecated
			enum := pf.Enum
			if len(enum) == 0 && isArray(pf) && pf.Items != nil {
				items := d.Flatten(pf.Items)
				if items != nil {
					enum = items.Enum
				}
			}
			for _, v := range enum {
				field.Enum = append(field.Enum, fmt.Sprint(v))
			}
		}
		out = append(out, field)
	}
	return out
}

// graphType is the short type label of a field: the display name of a
// component, the format or type of a primitive, with [] for arrays.
func (d *Document) graphType(p *Schema, short map[string]string, depth int) string {
	f := d.Flatten(p)
	if f == nil || depth > 4 {
		return ""
	}
	if isArray(f) {
		if f.Items == nil {
			return "array"
		}
		return d.graphType(f.Items, short, depth+1) + "[]"
	}
	name := d.SchemaName(p)
	if name != "" {
		label := short[name]
		if label == "" {
			label = name
		}
		return label
	}
	if f.Format != "" {
		return f.Format
	}
	primary := f.Type.Primary()
	if primary != "" {
		return primary
	}
	if isObject(f) {
		return "object"
	}
	if len(f.OneOf) > 0 || len(f.AnyOf) > 0 {
		return "union"
	}
	return "any"
}

// graphEdges returns the relations of one entity, filling Field.Ref on the
// fields they start from.
func (d *Document) graphEdges(node *Node, f *Schema, entities map[string]bool, index *entityIndex) []*Edge {
	fields := map[string]*Field{}
	for i := range node.Fields {
		fields[node.Fields[i].Name] = &node.Fields[i]
	}

	var out []*Edge
	byField := map[string]*Edge{}

	// x-ref and $ref properties.
	for _, key := range f.Properties.Keys {
		p := f.Properties.Map[key]
		field := fields[key]
		if p == nil || field == nil {
			continue
		}

		target := d.extensionRef(p)
		if entities[target] {
			field.Ref = target
			e := &Edge{From: node.Name, To: target, FromField: key, Kind: EdgeFK, Cardinality: fkCardinality(field)}
			out = append(out, e)
			byField[key] = e
			continue
		}

		target, many := d.propertyTarget(p)
		if !entities[target] {
			continue
		}
		field.Ref = target
		e := &Edge{From: node.Name, To: target, FromField: key, Kind: EdgeHasOne, Cardinality: "1 : 1"}
		if many {
			e.Kind = EdgeHasMany
			e.Cardinality = "1 : many"
		} else if field.Nullable || field.Optional {
			e.Cardinality = "1 : 0..1"
		}
		out = append(out, e)
		byField[key] = e
	}

	// foo_id properties: the key of a sibling foo relation, or a guess.
	dropped := map[*Edge]bool{}
	for _, key := range f.Properties.Keys {
		base := foreignKeyBase(key)
		field := fields[key]
		if base == "" || field == nil {
			continue
		}

		sibling := byField[base]
		declared := byField[key]
		if declared != nil {
			// An x-ref key and its foo object are one relation.
			if sibling != nil && sibling.Kind == EdgeHasOne && sibling.To == declared.To {
				dropped[sibling] = true
			}
			continue
		}
		if sibling != nil && sibling.Kind == EdgeHasOne {
			sibling.Kind = EdgeFK
			sibling.FromField = key
			sibling.Cardinality = fkCardinality(field)
			field.Ref = sibling.To
			continue
		}
		if sibling != nil {
			continue
		}

		target := index.match(base, node.Group)
		if target == "" {
			continue
		}
		field.Ref = target
		out = append(out, &Edge{From: node.Name, To: target, FromField: key, Kind: EdgeFK, Cardinality: fkCardinality(field), Inferred: true})
	}

	if len(dropped) == 0 {
		return out
	}
	kept := out[:0]
	for _, e := range out {
		if !dropped[e] {
			kept = append(kept, e)
		}
	}
	return kept
}

// extensionRef returns the component named by an x-ref extension, which is a
// $ref string (#/components/schemas/Foo) or a bare component name.
func (d *Document) extensionRef(p *Schema) string {
	value, ok := p.Extensions["x-ref"].(string)
	if !ok || value == "" {
		return ""
	}
	name := RefName(value, refSchemas)
	if name != "" {
		return name
	}
	return value
}

// fkCardinality reads a foreign key field: many rows point at one target, or
// at none when the key may be missing.
func fkCardinality(field *Field) string {
	if field.Optional || field.Nullable {
		return "many : 0..1"
	}
	return "many : 1"
}

// foreignKeyBase returns foo for foo_id and fooId, otherwise "".
func foreignKeyBase(key string) string {
	if len(key) > 3 && strings.HasSuffix(key, "_id") {
		return strings.TrimSuffix(key, "_id")
	}
	if len(key) > 2 && strings.HasSuffix(key, "Id") {
		base := strings.TrimSuffix(key, "Id")
		last, _ := utf8.DecodeLastRuneInString(base)
		if unicode.IsLower(last) || unicode.IsDigit(last) {
			return base
		}
	}
	return ""
}

// entityIndex finds entities by type name for inferred foreign keys.
type entityIndex struct {
	byName map[string][]*Node
}

func newEntityIndex(nodes []*Node) *entityIndex {
	index := &entityIndex{byName: map[string][]*Node{}}
	for _, node := range nodes {
		parts := strings.Split(node.Name, "_")
		last := parts[len(parts)-1]
		index.byName[last] = append(index.byName[last], node)
		if node.Short != last {
			index.byName[node.Short] = append(index.byName[node.Short], node)
		}
	}
	return index
}

// match resolves the base of a foo_id key (organization, ticket_category,
// categories) to an entity named Organization, TicketCategory or Category,
// trying the whole base first and then without its leading words, so
// parent_ticket finds Ticket. An entity in group wins over one elsewhere.
func (x *entityIndex) match(base, group string) string {
	words := splitWords(base)
	for start := 0; start < len(words); start++ {
		rest := words[start:]
		singular := append(append([]string(nil), rest[:len(rest)-1]...), singularize(rest[len(rest)-1]))
		for _, candidate := range []string{camelCase(rest), camelCase(singular)} {
			nodes := x.byName[candidate]
			if len(nodes) == 0 {
				continue
			}
			for _, node := range nodes {
				if node.Group == group {
					return node.Name
				}
			}
			return nodes[0].Name
		}
	}
	return ""
}

// splitWords splits snake_case and camelCase into lower-case words.
func splitWords(s string) []string {
	var words []string
	var current []rune
	for _, r := range s {
		if r == '_' || r == '-' {
			if len(current) > 0 {
				words = append(words, string(current))
			}
			current = nil
			continue
		}
		if unicode.IsUpper(r) && len(current) > 0 {
			words = append(words, string(current))
			current = nil
		}
		current = append(current, unicode.ToLower(r))
	}
	if len(current) > 0 {
		words = append(words, string(current))
	}
	return words
}

func camelCase(words []string) string {
	var b strings.Builder
	for _, w := range words {
		first, size := utf8.DecodeRuneInString(w)
		b.WriteRune(unicode.ToUpper(first))
		b.WriteString(w[size:])
	}
	return b.String()
}

// singularize handles the English plurals that show up in field names:
// categories, addresses, boxes, branches, units. Words that already end in
// ss, us or is (address, status, analysis) are left alone.
func singularize(w string) string {
	switch {
	case strings.HasSuffix(w, "ies") && len(w) > 3:
		return strings.TrimSuffix(w, "ies") + "y"
	case strings.HasSuffix(w, "sses"), strings.HasSuffix(w, "xes"), strings.HasSuffix(w, "ches"), strings.HasSuffix(w, "shes"):
		return strings.TrimSuffix(w, "es")
	case strings.HasSuffix(w, "ss"), strings.HasSuffix(w, "us"), strings.HasSuffix(w, "is"):
		return w
	case strings.HasSuffix(w, "s") && len(w) > 1:
		return strings.TrimSuffix(w, "s")
	}
	return w
}
