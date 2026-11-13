package reference

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"math/rand"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/vanclief/compose/components/rest/api/validate"
)

// The property tests generate small schemas from fixed seeds. A generated
// schema is first a propGroup, the keywords that apply to it, and is then
// written out as JSON with each keyword placed at random on the schema
// itself or on one of its allOf members, beside a $ref or inside a member
// (nullable and const always on the schema itself, see render),
// in its 3.0 or 3.1 spelling, and each reference to a component directly,
// through the component's alias or, outside a component, as a single-member
// allOf around it with the component's type beside it. Two writings of one
// group are the same schema.

// propCases is how many cases each property runs.
const propCases = 500

// propAtom is one keyword of a generated schema and the value it sets.
type propAtom struct {
	key   string
	value any
}

// propGroup is a generated schema: whether it is a component, its type, the
// keywords that apply to it, the components it refers to through allOf
// members or a $ref, how many allOf members hold only keywords, and its
// items, properties and additional properties ("any", "closed" or "schema")
// when it has any.
type propGroup struct {
	comp       bool
	typ        string
	atoms      []propAtom
	refs       []string
	inline     int
	items      *propGroup
	props      []propProp
	additional string
	addSchema  *propGroup
}

type propProp struct {
	name  string
	group *propGroup
}

// propGen generates and writes out groups. comps are the components a new
// group can refer to and groups the components generated; numeric limits
// generation to numeric bounds.
type propGen struct {
	r       *rand.Rand
	comps   []string
	groups  map[string]*propGroup
	numeric bool
}

var (
	propLowerKeys  = []string{"minimum", "exclusiveMinimum"}
	propUpperKeys  = []string{"maximum", "exclusiveMaximum"}
	propMinCounts  = []string{"minLength", "minItems", "minProperties"}
	propMaxCounts  = []string{"maxLength", "maxItems", "maxProperties"}
	propSingleKeys = []string{"pattern", "multipleOf", "const"}
)

// combinableKey is a keyword whose values combine across allOf members:
// the tightest bound holds, and a flag holds when any member sets it.
func (g *propGen) combinableKey() string {
	keys := slices.Concat(propLowerKeys, propUpperKeys)
	if !g.numeric {
		keys = slices.Concat(keys, propMinCounts, propMaxCounts, []string{"uniqueItems", "nullable"})
	}
	return keys[g.r.Intn(len(keys))]
}

func (g *propGen) atom(key string) propAtom {
	switch key {
	case "minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum":
		return propAtom{key, float64(g.r.Intn(41)-10) / 2}
	case "minLength", "maxLength", "minItems", "maxItems", "minProperties", "maxProperties":
		return propAtom{key, g.r.Intn(8)}
	case "uniqueItems", "nullable":
		return propAtom{key, true}
	case "pattern":
		return propAtom{key, fmt.Sprintf("^[a-z]{%d}$", g.r.Intn(4))}
	case "multipleOf":
		return propAtom{key, float64(1 + g.r.Intn(4))}
	}
	return propAtom{key, fmt.Sprintf("c%d", g.r.Intn(3))}
}

func (g *propGen) ref() string {
	return g.comps[g.r.Intn(len(g.comps))]
}

// components generates three components, each referring only to those
// before it, so references never form a cycle. A component has no
// structure, a type of string or none, and no pattern, multipleOf or const,
// so the keywords that take the first value found are never set twice in
// one allOf.
func (g *propGen) components() map[string]*propGroup {
	out := map[string]*propGroup{}
	g.comps = nil
	g.groups = out
	for i := range 3 {
		name := fmt.Sprintf("C%d", i)
		out[name] = g.group(1, true)
		g.comps = append(g.comps, name)
	}
	return out
}

// group generates a schema. A component (comp set) is kept simple, see
// components; the others nest through items, properties and additional
// properties up to depth 3.
func (g *propGen) group(depth int, comp bool) *propGroup {
	pg := &propGroup{comp: comp}
	types := []string{"", "string", "integer", "number"}
	switch {
	case g.numeric && comp:
		types = []string{""}
	case g.numeric:
		types = []string{"integer", "number"}
	case comp:
		types = []string{"", "string"}
	case depth < 3:
		types = append(types, "array", "object")
	}
	pg.typ = types[g.r.Intn(len(types))]
	for range g.r.Intn(4) {
		pg.atoms = append(pg.atoms, g.atom(g.combinableKey()))
	}
	if !comp && !g.numeric {
		for _, key := range propSingleKeys {
			if g.r.Intn(5) == 0 {
				pg.atoms = append(pg.atoms, g.atom(key))
			}
		}
	}
	switch g.r.Intn(3) {
	case 1:
		if len(g.comps) > 0 {
			pg.refs = []string{g.ref()}
		}
	case 2:
		for range 2 + g.r.Intn(2) {
			if len(g.comps) > 0 && g.r.Intn(2) == 0 {
				pg.refs = append(pg.refs, g.ref())
			} else {
				pg.inline++
			}
		}
	}
	switch pg.typ {
	case "array":
		pg.items = g.group(depth+1, false)
	case "object":
		for i := range 1 + g.r.Intn(2) {
			pg.props = append(pg.props, propProp{name: fmt.Sprintf("p%d", i), group: g.group(depth+1, false)})
		}
		pg.additional = []string{"any", "closed", "schema"}[g.r.Intn(3)]
		if pg.additional == "schema" {
			pg.addSchema = g.group(depth+1, false)
		}
	}
	return pg
}

// render writes pg out as JSON, placing each keyword at random.
func (g *propGen) render(pg *propGroup) map[string]any {
	parent := map[string]any{}
	if pg.typ != "" {
		parent["type"] = pg.typ
	}
	if pg.items != nil {
		parent["items"] = g.render(pg.items)
	}
	if len(pg.props) > 0 {
		props := map[string]any{}
		for _, p := range pg.props {
			props[p.name] = g.render(p.group)
		}
		parent["properties"] = props
	}
	switch pg.additional {
	case "any":
		// None, true and {} all allow any additional property.
		switch g.r.Intn(3) {
		case 1:
			parent["additionalProperties"] = true
		case 2:
			parent["additionalProperties"] = map[string]any{}
		}
	case "closed":
		parent["additionalProperties"] = false
	case "schema":
		parent["additionalProperties"] = g.render(pg.addSchema)
	}

	var members []map[string]any
	for range pg.inline {
		members = append(members, map[string]any{})
	}
	for _, ref := range pg.refs {
		members = append(members, map[string]any{"$ref": refSchemas + ref})
	}
	g.r.Shuffle(len(members), func(i, j int) {
		members[i], members[j] = members[j], members[i]
	})
	// A schema with nothing but keywords and references can be a $ref
	// itself, with the other members in an allOf beside it.
	structural := pg.typ != "" || pg.items != nil || len(pg.props) > 0 || pg.additional != ""
	if !structural && len(pg.refs) > 0 && g.r.Intn(2) == 0 {
		var at []int
		for i, m := range members {
			_, ok := m["$ref"]
			if ok {
				at = append(at, i)
			}
		}
		i := at[g.r.Intn(len(at))]
		parent["$ref"] = members[i]["$ref"]
		members = slices.Delete(members, i, i+1)
	}

	// nullable and const go on the schema itself: a member of an allOf of
	// two or more makes it nullable only when every member admits null,
	// which a const on a member refuses.
	holders := append([]map[string]any{parent}, members...)
	placed := make([][]propAtom, len(holders))
	for _, a := range pg.atoms {
		i := g.r.Intn(len(holders))
		if a.key == "nullable" || a.key == "const" {
			i = 0
		}
		placed[i] = append(placed[i], a)
	}
	for i, h := range holders {
		g.write(h, placed[i], i == 0 && pg.typ != "")
	}
	// A member that is a $ref with keywords beside it can also be an allOf
	// of the bare $ref with the keywords beside the allOf.
	for _, m := range members {
		ref, ok := m["$ref"]
		if ok && len(m) > 1 && g.r.Intn(2) == 0 {
			delete(m, "$ref")
			m["allOf"] = []any{map[string]any{"$ref": ref}}
		}
	}
	nullable := nullableOf(pg, g.groups)
	for _, m := range members {
		g.respell(m, pg.comp, nullable)
	}
	if len(members) > 0 {
		list := make([]any, len(members))
		for i, m := range members {
			list[i] = m
		}
		parent["allOf"] = list
	}
	g.respell(parent, pg.comp, nullable)
	return parent
}

// respell writes the $ref of h at random to the component's alias, or, when
// h is a bare $ref outside a component, as a single-member allOf around it
// with the component's type beside it and nullable when the schema it is
// part of is nullable anyway, the 3.0 spelling of a nullable reference. In
// a component that would make a model of its own of what was another name
// for the model it refers to.
func (g *propGen) respell(h map[string]any, comp, nullable bool) {
	ref, ok := h["$ref"].(string)
	if !ok {
		return
	}
	switch g.r.Intn(3) {
	case 1:
		h["$ref"] = ref + propAlias
	case 2:
		if comp || len(h) > 1 {
			return
		}
		delete(h, "$ref")
		h["allOf"] = []any{map[string]any{"$ref": ref}}
		typ := g.groups[RefName(ref, refSchemas)].typ
		if typ != "" {
			h["type"] = typ
		}
		if nullable {
			h["nullable"] = true
		}
	}
}

// propAlias is the suffix of the name of a component's alias, a component
// that is only a reference to it.
const propAlias = "Alias"

// schemas writes out comps, each with its alias.
func (g *propGen) schemas(comps map[string]*propGroup) map[string]any {
	out := map[string]any{}
	for _, name := range slices.Sorted(maps.Keys(comps)) {
		out[name] = g.render(comps[name])
		out[name+propAlias] = map[string]any{"$ref": refSchemas + name}
	}
	return out
}

// write sets atoms on the schema h. A keyword h gets twice keeps the
// tighter value, which is the one that holds. An exclusive bound is written
// in the 3.1 form or, when h has no inclusive one on that side, the 3.0
// form; nullable on a typed schema as nullable: true or as a 3.1 type list.
func (g *propGen) write(h map[string]any, atoms []propAtom, typed bool) {
	vals := map[string]propAtom{}
	for _, a := range atoms {
		old, ok := vals[a.key]
		if !ok || atomTighter(a, old) {
			vals[a.key] = a
		}
	}
	for _, key := range slices.Sorted(maps.Keys(vals)) {
		a := vals[key]
		switch key {
		case "exclusiveMinimum", "exclusiveMaximum":
			inclusive := "minimum"
			if key == "exclusiveMaximum" {
				inclusive = "maximum"
			}
			_, ok := vals[inclusive]
			if !ok && g.r.Intn(2) == 0 {
				h[inclusive] = a.value
				h[key] = true
				continue
			}
		case "nullable":
			if typed && g.r.Intn(2) == 0 {
				h["type"] = []any{h["type"], "null"}
				continue
			}
		}
		h[key] = a.value
	}
}

// atomTighter reports whether a is a tighter bound than b, of the same
// keyword.
func atomTighter(a, b propAtom) bool {
	switch av := a.value.(type) {
	case float64:
		bv := b.value.(float64)
		if slices.Contains(propLowerKeys, a.key) {
			return av > bv
		}
		return av < bv
	case int:
		bv := b.value.(int)
		if slices.Contains(propMinCounts, a.key) {
			return av > bv
		}
		return av < bv
	}
	return false
}

// propSpec writes a document whose POST /things takes s as the query
// parameter q and as the request body property name, and returns it as the
// response property name and as the response header X-S, with comps and
// their aliases as its component schemas.
func (g *propGen) propSpec(s *propGroup, comps map[string]*propGroup) []byte {
	schemas := g.schemas(comps)
	rs := g.render(s)
	doc := map[string]any{
		"openapi": "3.1.0",
		"info":    map[string]any{"title": "t", "version": "1"},
		"paths": map[string]any{"/things": map[string]any{"post": map[string]any{
			"parameters":  []any{map[string]any{"name": "q", "in": "query", "schema": rs}},
			"requestBody": map[string]any{"content": map[string]any{"application/json": map[string]any{"schema": objectOf(rs)}}},
			"responses": map[string]any{"200": map[string]any{
				"description": "ok",
				"headers":     map[string]any{"X-S": map[string]any{"schema": rs}},
				"content":     map[string]any{"application/json": map[string]any{"schema": objectOf(rs)}},
			}},
		}}},
		"components": map[string]any{"schemas": schemas},
	}
	out, err := json.Marshal(doc)
	if err != nil {
		panic(err)
	}
	return out
}

func objectOf(s map[string]any) map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"name": s}}
}

// walkSchemas calls visit once for every schema of doc, in document order.
func walkSchemas(doc *Document, visit func(*Schema)) {
	seen := map[*Schema]bool{}
	var walk func(*Schema)
	walk = func(s *Schema) {
		if s == nil || seen[s] {
			return
		}
		seen[s] = true
		visit(s)
		for _, key := range s.Properties.Keys {
			walk(s.Properties.Map[key])
		}
		walk(s.Items)
		walk(s.AdditionalSchema())
		walk(s.Not)
		for _, list := range [][]*Schema{s.AllOf, s.OneOf, s.AnyOf} {
			for _, m := range list {
				walk(m)
			}
		}
	}
	content := func(c Ordered[*MediaType]) {
		for _, ct := range c.Keys {
			walk(c.Map[ct].Schema)
		}
	}
	for _, name := range doc.Components.Schemas.Keys {
		walk(doc.Components.Schemas.Map[name])
	}
	for _, path := range doc.Paths.Keys {
		for _, m := range doc.Paths.Map[path].Methods() {
			for _, p := range m.Operation.Parameters {
				walk(p.Schema)
			}
			if m.Operation.RequestBody != nil {
				content(m.Operation.RequestBody.Content)
			}
			for _, code := range m.Operation.Responses.Keys {
				res := m.Operation.Responses.Map[code]
				content(res.Content)
				for _, name := range res.Headers.Keys {
					walk(res.Headers.Map[name].Schema)
				}
			}
		}
	}
}

// boolBounds puts some exclusive bounds of doc back in the 3.0 boolean form
// Parse turns into numbers, as a document built without Parse can hold.
func boolBounds(r *rand.Rand, doc *Document) {
	walkSchemas(doc, func(s *Schema) {
		if s.Minimum == nil && s.ExclusiveMinimum != nil && r.Intn(2) == 0 {
			s.Minimum = s.ExclusiveMinimum.Value
			s.ExclusiveMinimum = &Bound{Flag: true}
		}
		if s.Maximum == nil && s.ExclusiveMaximum != nil && r.Intn(2) == 0 {
			s.Maximum = s.ExclusiveMaximum.Value
			s.ExclusiveMaximum = &Bound{Flag: true}
		}
	})
}

func mustParse(t *testing.T, spec []byte) *Document {
	t.Helper()
	doc, err := Parse(spec)
	require.NoError(t, err, string(spec))
	return doc
}

// TestDiffPropertyEquivalentSchemas checks that two writings of the same
// schema, with the keywords placed differently, allOf members reordered,
// keywords beside a $ref, on an allOf wrapper of it or inside the member,
// exclusive bounds and nullable in their 3.0 and 3.1 spellings, and none,
// true and {} for additional properties, compare as equal in a request
// body, a query parameter, a response body and a response header.
func TestDiffPropertyEquivalentSchemas(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for i := range propCases {
		g := &propGen{r: r}
		comps := g.components()
		s := g.group(0, false)
		base := g.propSpec(s, comps)
		head := g.propSpec(s, comps)
		headDoc := mustParse(t, head)
		boolBounds(r, headDoc)
		for _, rep := range []*Report{Diff(mustParse(t, base), headDoc), Diff(headDoc, mustParse(t, base))} {
			require.True(t, rep.Empty(), "case %d\nbase: %s\nhead: %s\n%s", i, base, head, rep.Markdown())
		}
	}
}

func (pg *propGroup) clone() *propGroup {
	if pg == nil {
		return nil
	}
	out := *pg
	out.atoms = slices.Clone(pg.atoms)
	out.refs = slices.Clone(pg.refs)
	out.items = pg.items.clone()
	out.addSchema = pg.addSchema.clone()
	out.props = nil
	for _, p := range pg.props {
		out.props = append(out.props, propProp{name: p.name, group: p.group.clone()})
	}
	return &out
}

// groups lists pg and the groups nested in it, depth first.
func (pg *propGroup) groups() []*propGroup {
	out := []*propGroup{pg}
	if pg.items != nil {
		out = append(out, pg.items.groups()...)
	}
	for _, p := range pg.props {
		out = append(out, p.group.groups()...)
	}
	if pg.addSchema != nil {
		out = append(out, pg.addSchema.groups()...)
	}
	return out
}

// applied is every keyword that applies to pg: its own and those of the
// components it refers to.
func applied(pg *propGroup, comps map[string]*propGroup) []propAtom {
	out := slices.Clone(pg.atoms)
	for _, ref := range pg.refs {
		out = append(out, applied(comps[ref], comps)...)
	}
	return out
}

// nullableOf reports whether pg is nullable as the diff flattens it: by its
// own nullable, which render writes on the schema itself, by that of the
// component it refers to when that is its only allOf member, or, with two or
// more members, by that of a member when every member admits null.
func nullableOf(pg *propGroup, comps map[string]*propGroup) bool {
	if hasAtom(pg.atoms, "nullable") {
		return true
	}
	if len(pg.refs) == 1 && pg.inline == 0 {
		return nullableOf(comps[pg.refs[0]], comps)
	}
	if len(pg.refs)+pg.inline < 2 {
		return false
	}
	// An inline member admits null, having no type and no const, and is
	// never nullable itself.
	some := false
	for _, ref := range pg.refs {
		if !admitsNullOf(comps[ref], comps) {
			return false
		}
		some = some || nullableOf(comps[ref], comps)
	}
	return some
}

// admitsNullOf reports whether pg lets null through as the diff reads it:
// it is nullable, or neither it nor a component it refers to states a type
// and no const applies.
func admitsNullOf(pg *propGroup, comps map[string]*propGroup) bool {
	return nullableOf(pg, comps) || !typedOf(pg, comps) && !hasAtom(applied(pg, comps), "const")
}

func typedOf(pg *propGroup, comps map[string]*propGroup) bool {
	if pg.typ != "" {
		return true
	}
	return slices.ContainsFunc(pg.refs, func(ref string) bool {
		return typedOf(comps[ref], comps)
	})
}

// expectNullable says, as expectChange does for the other keywords, whether
// nullable differs from before, with the components bc, to after, with the
// components ac, and whether null is refused or let through where it was
// not.
func expectNullable(before *propGroup, bc map[string]*propGroup, after *propGroup, ac map[string]*propGroup) (bool, bool, bool) {
	b := admitsNullOf(before, bc)
	a := admitsNullOf(after, ac)
	return nullableOf(before, bc) != nullableOf(after, ac), b && !a, a && !b
}

// testBound is a numeric bound: its value and whether it is excluded.
type testBound struct {
	v    float64
	excl bool
}

// narrower reports whether bound a, lower when lower is set and upper
// otherwise, admits fewer values than b. No bound admits every value.
func narrower(a, b *testBound, lower bool) bool {
	if a == nil {
		return false
	}
	if b == nil {
		return true
	}
	if a.v != b.v {
		return (a.v > b.v) == lower
	}
	return a.excl && !b.excl
}

// boundOf is the bound that holds among the atoms: the narrowest of the
// inclusive and exclusive keywords of one side.
func boundOf(atoms []propAtom, lower bool) *testBound {
	inclusive, exclusive := "maximum", "exclusiveMaximum"
	if lower {
		inclusive, exclusive = "minimum", "exclusiveMinimum"
	}
	var out *testBound
	for _, a := range atoms {
		if a.key != inclusive && a.key != exclusive {
			continue
		}
		b := &testBound{v: a.value.(float64), excl: a.key == exclusive}
		if narrower(b, out, lower) {
			out = b
		}
	}
	return out
}

// countOf is the count that holds among the atoms of key, the largest
// minimum or the smallest maximum, nil when none sets it.
func countOf(atoms []propAtom, key string) *int {
	var out *int
	for _, a := range atoms {
		if a.key != key {
			continue
		}
		v := a.value.(int)
		if out == nil || slices.Contains(propMinCounts, key) && v > *out || slices.Contains(propMaxCounts, key) && v < *out {
			out = &v
		}
	}
	return out
}

func hasAtom(atoms []propAtom, key string) bool {
	return slices.ContainsFunc(atoms, func(a propAtom) bool {
		return a.key == key
	})
}

func singleOf(atoms []propAtom, key string) string {
	for _, a := range atoms {
		if a.key == key {
			return fmt.Sprint(a.value)
		}
	}
	return ""
}

// expectChange says whether the keywords that apply, before and after, of
// the class of key differ, and whether the change narrows or widens the
// values accepted.
func expectChange(key string, before, after []propAtom) (bool, bool, bool) {
	switch key {
	case "minimum", "exclusiveMinimum", "maximum", "exclusiveMaximum":
		lower := slices.Contains(propLowerKeys, key)
		b := boundOf(before, lower)
		a := boundOf(after, lower)
		return !reflect.DeepEqual(a, b), narrower(a, b, lower), narrower(b, a, lower)
	case "minLength", "minItems", "minProperties":
		b := countOf(before, key)
		a := countOf(after, key)
		value := func(v *int) int {
			if v == nil {
				return 0
			}
			return *v
		}
		return !reflect.DeepEqual(a, b), value(a) > value(b), value(a) < value(b)
	case "maxLength", "maxItems", "maxProperties":
		b := countOf(before, key)
		a := countOf(after, key)
		return !reflect.DeepEqual(a, b), a != nil && (b == nil || *a < *b), b != nil && (a == nil || *a > *b)
	case "uniqueItems":
		b := hasAtom(before, key)
		a := hasAtom(after, key)
		return a != b, a && !b, b && !a
	}
	b := singleOf(before, key)
	a := singleOf(after, key)
	return a != b, a != "", b != ""
}

// mutate changes one keyword of pg, adding, removing or replacing it, or
// opens or closes pg when it is an object, and returns the keyword.
func (g *propGen) mutate(pg *propGroup, comps map[string]*propGroup) string {
	switch g.r.Intn(5) {
	case 1:
		if len(pg.atoms) > 0 {
			i := g.r.Intn(len(pg.atoms))
			key := pg.atoms[i].key
			pg.atoms = slices.Delete(pg.atoms, i, i+1)
			return key
		}
	case 2:
		if len(pg.atoms) > 0 {
			i := g.r.Intn(len(pg.atoms))
			key := pg.atoms[i].key
			if key == "minimum" || key == "exclusiveMinimum" {
				// Also turn an inclusive bound into an exclusive one.
				key = propLowerKeys[g.r.Intn(2)]
			}
			pg.atoms[i] = g.atom(key)
			return pg.atoms[i].key
		}
	case 3:
		key := propSingleKeys[g.r.Intn(len(propSingleKeys))]
		i := slices.IndexFunc(pg.atoms, func(a propAtom) bool {
			return a.key == key
		})
		switch {
		case i >= 0 && g.r.Intn(2) == 0:
			pg.atoms = slices.Delete(pg.atoms, i, i+1)
		case i >= 0:
			pg.atoms[i] = g.atom(key)
		case !hasAtom(applied(pg, comps), key):
			pg.atoms = append(pg.atoms, g.atom(key))
		}
		return key
	case 4:
		if pg.typ == "object" {
			if pg.additional == "closed" {
				pg.additional = "any"
			} else {
				pg.additional = "closed"
				pg.addSchema = nil
			}
			return "additionalProperties"
		}
	}
	a := g.atom(g.combinableKey())
	pg.atoms = append(pg.atoms, a)
	return a.key
}

// TestDiffPropertyDirection checks that one keyword narrowed or widened, at
// any depth of a schema, is reported once in each of a request body, a query
// parameter, a response body and a response header, breaking the request
// side when it narrows and the response side when it widens, and not at all
// when another keyword that applies, such as a tighter bound in another
// allOf member, holds the same values.
func TestDiffPropertyDirection(t *testing.T) {
	contexts := []struct {
		prefix  string
		request bool
	}{
		{"POST /things › query › q", true},
		{"POST /things › request body › name", true},
		{"POST /things › response 200 › name", false},
		{"POST /things › response 200 › header X-S", false},
	}
	r := rand.New(rand.NewSource(2))
	reported := 0
	for i := range propCases {
		g := &propGen{r: r}
		comps := g.components()
		s := g.group(0, false)
		head := s.clone()
		groups := s.groups()
		at := r.Intn(len(groups))
		before := groups[at]
		after := head.groups()[at]
		key := g.mutate(after, comps)

		var changed, narrows, widens bool
		switch key {
		case "additionalProperties":
			changed = true
			narrows = after.additional == "closed"
		case "nullable":
			changed, narrows, widens = expectNullable(before, comps, after, comps)
		default:
			changed, narrows, widens = expectChange(key, applied(before, comps), applied(after, comps))
		}

		baseSpec := g.propSpec(s, comps)
		headSpec := g.propSpec(head, comps)
		headDoc := mustParse(t, headSpec)
		boolBounds(r, headDoc)
		rep := Diff(mustParse(t, baseSpec), headDoc)
		msg := fmt.Sprintf("case %d: %s at group %d\nbase: %s\nhead: %s\n%s", i, key, at, baseSpec, headSpec, rep.Markdown())
		require.Empty(t, rep.Models, msg)
		if !changed {
			require.True(t, rep.Empty(), msg)
			continue
		}
		reported++
		od := rep.Operation("POST", "/things")
		require.NotNil(t, od, msg)
		require.Len(t, od.Changes, len(contexts), msg)
		word := detailWord(key)
		for _, ctx := range contexts {
			var found []Change
			for _, c := range od.Changes {
				if strings.HasPrefix(c.Path, ctx.prefix) {
					found = append(found, c)
				}
			}
			require.Len(t, found, 1, ctx.prefix+"\n"+msg)
			require.Contains(t, found[0].Detail, word, msg)
			breaking := widens
			if ctx.request {
				breaking = narrows
			}
			require.Equal(t, breaking, found[0].Breaking, ctx.prefix+"\n"+msg)
		}
	}
	t.Logf("%d of %d mutations changed what applies", reported, propCases)
	require.Greater(t, reported, propCases/3, "most mutations change what applies")
	require.Less(t, reported, propCases, "some mutations are hidden by a tighter keyword")
}

// detailWord is the word a change of key has in its detail.
func detailWord(key string) string {
	switch key {
	case "exclusiveMinimum":
		return "minimum"
	case "exclusiveMaximum":
		return "maximum"
	case "additionalProperties":
		return "additional properties"
	}
	return key
}

// sameModelUse writes a reference to the component name, directly or
// through its alias, in a shape Diff does not expand: a $ref or a
// single-member allOf around one, with atoms beside it, and the type typ
// beside it when always is set, or else at random when typ, the
// component's own type, is not empty.
func (g *propGen) sameModelUse(name string, atoms []propAtom, typ string, always bool) map[string]any {
	ref := refSchemas + name
	if g.r.Intn(2) == 0 {
		ref += propAlias
	}
	h := map[string]any{}
	if always || typ != "" && g.r.Intn(2) == 0 {
		h["type"] = typ
	}
	g.write(h, atoms, h["type"] != nil)
	if g.r.Intn(2) == 0 {
		h["$ref"] = ref
	} else {
		h["allOf"] = []any{map[string]any{"$ref": ref}}
	}
	return h
}

// sameModelSpec writes a document whose POST /things takes a schema use
// writes as the request body property name when request is set, and
// returns one as the response property name when response is set, with
// comps and their aliases as its component schemas.
func (g *propGen) sameModelSpec(comps map[string]*propGroup, use func() map[string]any, request, response bool) []byte {
	op := map[string]any{}
	if request {
		op["requestBody"] = map[string]any{"content": map[string]any{"application/json": map[string]any{"schema": objectOf(use())}}}
	}
	ok := map[string]any{"description": "ok"}
	if response {
		ok["content"] = map[string]any{"application/json": map[string]any{"schema": objectOf(use())}}
	}
	op["responses"] = map[string]any{"200": ok}
	doc := map[string]any{
		"openapi":    "3.1.0",
		"info":       map[string]any{"title": "t", "version": "1"},
		"paths":      map[string]any{"/things": map[string]any{"post": op}},
		"components": map[string]any{"schemas": g.schemas(comps)},
	}
	out, err := json.Marshal(doc)
	if err != nil {
		panic(err)
	}
	return out
}

// TestDiffPropertySameModel checks that one keyword narrowed or widened in
// a component that a request, a response or both refer to without
// expanding it, through a $ref or a single-member allOf around one, with
// keywords, nullable or a type beside it, directly or through an alias, is
// reported once, in the model, breaking when it narrows and a request uses
// the model or when it widens and a response does; and that each use
// carries one "model changed" entry with the flag of its context and
// nothing expanded.
func TestDiffPropertySameModel(t *testing.T) {
	const (
		requestPath  = "POST /things › request body › name"
		responsePath = "POST /things › response 200 › name"
	)
	r := rand.New(rand.NewSource(5))
	reported := 0
	for i := range propCases {
		g := &propGen{r: r}
		comps := g.components()
		// The last component is the one no other component refers to. One
		// without a type that refers to a single component is another name
		// for it as long as it has no keywords, which a mutation can add or
		// remove; an empty allOf member keeps it a model of its own.
		target := g.comps[len(g.comps)-1]
		tg := comps[target]
		if tg.typ == "" && len(tg.refs) == 1 && tg.inline == 0 {
			tg.inline = 1
		}
		head := maps.Clone(comps)
		head[target] = comps[target].clone()
		key := g.mutate(head[target], comps)
		changed, narrows, widens := expectChange(key, applied(comps[target], comps), applied(head[target], head))
		if key == "nullable" {
			changed, narrows, widens = expectNullable(comps[target], comps, head[target], head)
		}

		var atoms []propAtom
		if r.Intn(2) == 0 {
			atoms = append(atoms, propAtom{"nullable", true})
		}
		for range r.Intn(3) {
			atoms = append(atoms, g.atom(g.combinableKey()))
		}
		typ := comps[target].typ
		always := false
		if typ == "" && r.Intn(2) == 0 {
			typ = "string"
			always = true
		}
		use := func() map[string]any {
			return g.sameModelUse(target, atoms, typ, always)
		}
		request := r.Intn(3) != 1
		response := !request || r.Intn(2) == 0

		baseSpec := g.sameModelSpec(comps, use, request, response)
		headSpec := g.sameModelSpec(head, use, request, response)
		rep := Diff(mustParse(t, baseSpec), mustParse(t, headSpec))
		msg := fmt.Sprintf("case %d: %s in %s\nbase: %s\nhead: %s\n%s", i, key, target, baseSpec, headSpec, rep.Markdown())
		if !changed {
			require.True(t, rep.Empty(), msg)
			continue
		}
		reported++

		md := rep.Model(target)
		require.NotNil(t, md, msg)
		require.Len(t, md.Changes, 1, msg)
		require.Equal(t, target, md.Changes[0].Path, msg)
		require.Contains(t, md.Changes[0].Detail, detailWord(key), msg)
		require.Equal(t, request && narrows || response && widens, md.Changes[0].Breaking, msg)
		for _, other := range rep.Models {
			if other.Name == target {
				continue
			}
			// The alias only says the model it names changed.
			require.Equal(t, target+propAlias, other.Name, msg)
			require.Len(t, other.Changes, 1, msg)
			require.Equal(t, "model "+target+" changed", other.Changes[0].Detail, msg)
		}

		od := rep.Operation("POST", "/things")
		require.NotNil(t, od, msg)
		var want, got []string
		if request {
			want = append(want, fmt.Sprintf("%s | model %s changed | %t", requestPath, target, narrows))
		}
		if response {
			want = append(want, fmt.Sprintf("%s | model %s changed | %t", responsePath, target, widens))
		}
		for _, c := range od.Changes {
			got = append(got, fmt.Sprintf("%s | %s | %t", c.Path, c.Detail, c.Breaking))
		}
		require.Equal(t, want, got, msg)
	}
	t.Logf("%d of %d mutations changed what applies", reported, propCases)
	require.Greater(t, reported, propCases/3, "most mutations change what applies")
}

// TestDiffPropertyAliasKeyword checks a component A that only refers to a
// model, directly, through the model's alias or in a single-member allOf,
// and gets one keyword of its own, or loses the one it had, which makes it
// a model of its own on one side only: the keyword is reported once, in A,
// breaking when it narrows and a request uses A or when it widens and a
// response does; and each use, which refers to A or to A's alias in the
// same way on both sides with the same keywords beside it, carries one
// "model changed" entry with the flag of its context and nothing expanded.
func TestDiffPropertyAliasKeyword(t *testing.T) {
	const (
		requestPath  = "POST /things › request body › name"
		responsePath = "POST /things › response 200 › name"
	)
	r := rand.New(rand.NewSource(7))
	reported := 0
	for i := range propCases {
		g := &propGen{r: r}
		comps := g.components()
		bare := &propGroup{comp: true, refs: []string{g.ref()}}
		keyed := bare.clone()
		key := g.combinableKey()
		if r.Intn(4) == 0 {
			key = propSingleKeys[r.Intn(len(propSingleKeys))]
		}
		keyed.atoms = []propAtom{g.atom(key)}
		head := maps.Clone(comps)
		comps["A"] = bare
		head["A"] = keyed
		if r.Intn(2) == 0 {
			comps["A"] = keyed
			head["A"] = bare
		}
		changed, narrows, widens := expectChange(key, applied(comps["A"], comps), applied(head["A"], head))
		if key == "nullable" {
			changed, narrows, widens = expectNullable(comps["A"], comps, head["A"], head)
		}

		var atoms []propAtom
		if r.Intn(2) == 0 {
			atoms = append(atoms, propAtom{"nullable", true})
		}
		for range r.Intn(3) {
			atoms = append(atoms, g.atom(g.combinableKey()))
		}
		typ := ""
		if r.Intn(2) == 0 {
			typ = "string"
		}
		// The use names the same component on both sides, as an edit to A
		// leaves the schemas that use it as they were.
		name := "A"
		if r.Intn(2) == 0 {
			name += propAlias
		}
		use := func() map[string]any {
			h := g.sameModelUse("A", atoms, typ, typ != "")
			_, ok := h["$ref"]
			if ok {
				h["$ref"] = refSchemas + name
			} else {
				h["allOf"] = []any{map[string]any{"$ref": refSchemas + name}}
			}
			return h
		}
		request := r.Intn(3) != 1
		response := !request || r.Intn(2) == 0

		baseSpec := g.sameModelSpec(comps, use, request, response)
		headSpec := g.sameModelSpec(head, use, request, response)
		rep := Diff(mustParse(t, baseSpec), mustParse(t, headSpec))
		msg := fmt.Sprintf("case %d: %s in A\nbase: %s\nhead: %s\n%s", i, key, baseSpec, headSpec, rep.Markdown())
		if !changed {
			require.True(t, rep.Empty(), msg)
			continue
		}
		reported++

		md := rep.Model("A")
		require.NotNil(t, md, msg)
		require.Len(t, md.Changes, 1, msg)
		require.Equal(t, "A", md.Changes[0].Path, msg)
		require.Contains(t, md.Changes[0].Detail, detailWord(key), msg)
		require.Equal(t, request && narrows || response && widens, md.Changes[0].Breaking, msg)
		for _, other := range rep.Models {
			if other.Name == "A" {
				continue
			}
			// A's alias only says A changed.
			require.Equal(t, "A"+propAlias, other.Name, msg)
			require.Len(t, other.Changes, 1, msg)
			require.Equal(t, "model A changed", other.Changes[0].Detail, msg)
		}

		// A use through A's alias names the alias, which changed through A.
		detail := "model A changed"
		if name != "A" {
			detail = "model " + name + " changed (through A)"
		}
		od := rep.Operation("POST", "/things")
		require.NotNil(t, od, msg)
		var want, got []string
		if request {
			want = append(want, fmt.Sprintf("%s | %s | %t", requestPath, detail, narrows))
		}
		if response {
			want = append(want, fmt.Sprintf("%s | %s | %t", responsePath, detail, widens))
		}
		for _, c := range od.Changes {
			got = append(got, fmt.Sprintf("%s | %s | %t", c.Path, c.Detail, c.Breaking))
		}
		require.Equal(t, want, got, msg)
	}
	t.Logf("%d of %d mutations changed what applies", reported, propCases)
	require.Greater(t, reported, propCases/3, "most mutations change what applies")
}

// jsonBound is the bound, lower when lower is set, that holds for the JSON
// schema s with the component schemas comps: the narrowest of its own
// keywords, its allOf members' and, through a $ref, the component's. A
// $ref's siblings and allOf apply only with siblings set, as in the diff;
// Flatten drops them.
func jsonBound(s map[string]any, comps map[string]any, siblings, lower bool) *testBound {
	ref, ok := s["$ref"].(string)
	if ok {
		target := jsonBound(comps[RefName(ref, refSchemas)].(map[string]any), comps, siblings, lower)
		if !siblings {
			return target
		}
		rest := maps.Clone(s)
		delete(rest, "$ref")
		own := jsonBound(rest, comps, siblings, lower)
		if narrower(target, own, lower) {
			return target
		}
		return own
	}
	inclusive, exclusive := "maximum", "exclusiveMaximum"
	if lower {
		inclusive, exclusive = "minimum", "exclusiveMinimum"
	}
	var out *testBound
	v, ok := s[inclusive].(float64)
	if ok {
		flag, _ := s[exclusive].(bool)
		out = &testBound{v: v, excl: flag}
	}
	v, ok = s[exclusive].(float64)
	if ok && narrower(&testBound{v: v, excl: true}, out, lower) {
		out = &testBound{v: v, excl: true}
	}
	members, _ := s["allOf"].([]any)
	for _, m := range members {
		b := jsonBound(m.(map[string]any), comps, siblings, lower)
		if narrower(b, out, lower) {
			out = b
		}
	}
	return out
}

// schemaBound reads the bound of a flattened schema, which must hold it in
// one keyword in the numeric form.
func schemaBound(t *testing.T, f *Schema, lower bool, msg string) *testBound {
	limit, excl := f.Maximum, f.ExclusiveMaximum
	if lower {
		limit, excl = f.Minimum, f.ExclusiveMinimum
	}
	require.False(t, limit != nil && excl != nil, "one keyword per bound\n"+msg)
	switch {
	case limit != nil:
		return &testBound{v: *limit}
	case excl != nil:
		require.NotNil(t, excl.Value, "numeric form\n"+msg)
		return &testBound{v: *excl.Value, excl: true}
	}
	return nil
}

// boundText is how the page writes the bounds lo and hi.
func boundText(lo, hi *testBound) []string {
	switch {
	case lo != nil && hi != nil:
		left, right := "[", "]"
		if lo.excl {
			left = "("
		}
		if hi.excl {
			right = ")"
		}
		return []string{left + formatNumber(lo.v) + " .. " + formatNumber(hi.v) + right}
	case lo != nil && lo.excl:
		return []string{"> " + formatNumber(lo.v)}
	case lo != nil:
		return []string{">= " + formatNumber(lo.v)}
	case hi != nil && hi.excl:
		return []string{"< " + formatNumber(hi.v)}
	case hi != nil:
		return []string{"<= " + formatNumber(hi.v)}
	}
	return nil
}

func within(v float64, lo, hi *testBound) bool {
	if lo != nil && (v < lo.v || v == lo.v && lo.excl) {
		return false
	}
	return hi == nil || v < hi.v || v == hi.v && !hi.excl
}

// satisfiable reports whether a value of type typ lies between lo and hi.
func satisfiable(typ string, lo, hi *testBound) bool {
	if lo == nil || hi == nil {
		return true
	}
	if typ == "number" {
		return lo.v < hi.v || lo.v == hi.v && !lo.excl && !hi.excl
	}
	v := math.Ceil(lo.v)
	if lo.excl && v == lo.v {
		v++
	}
	return within(v, lo, hi)
}

// as31 is s with 3.0 exclusive bounds in the 3.1 form JSON Schema reads,
// and references into the document the validator loads.
func as31(s any) any {
	switch v := s.(type) {
	case map[string]any:
		out := map[string]any{}
		for key, value := range v {
			out[key] = as31(value)
		}
		for _, side := range [][2]string{{"minimum", "exclusiveMinimum"}, {"maximum", "exclusiveMaximum"}} {
			flag, ok := v[side[1]].(bool)
			if !ok {
				continue
			}
			delete(out, side[1])
			if flag {
				out[side[1]] = v[side[0]]
				delete(out, side[0])
			}
		}
		ref, ok := v["$ref"].(string)
		if ok {
			out["$ref"] = "spec.json" + ref
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, m := range v {
			out[i] = as31(m)
		}
		return out
	}
	return s
}

// TestFlattenPropertyBounds checks that a flattened schema holds each of
// its bounds in one keyword, the narrowest that applies across its allOf
// members and references, which the page writes and the generated example
// meets, and that Flatten, the diff's flattening and Diff leave the
// document as it was.
func TestFlattenPropertyBounds(t *testing.T) {
	r := rand.New(rand.NewSource(3))
	validated := 0
	for i := range propCases {
		g := &propGen{r: r, numeric: true}
		comps := g.components()
		s := g.group(0, false)
		comps["S"] = s
		spec := g.propSpec(s, comps)
		var raw struct {
			Components struct {
				Schemas map[string]any `json:"schemas"`
			} `json:"components"`
		}
		require.NoError(t, json.Unmarshal(spec, &raw))
		js := raw.Components.Schemas["S"].(map[string]any)

		seed := r.Int63()
		doc := mustParse(t, spec)
		boolBounds(rand.New(rand.NewSource(seed)), doc)
		pristine := mustParse(t, spec)
		boolBounds(rand.New(rand.NewSource(seed)), pristine)
		ref := &Schema{Ref: refSchemas + "S"}
		msg := fmt.Sprintf("case %d\n%s", i, spec)

		f := doc.Flatten(ref)
		lo := jsonBound(js, raw.Components.Schemas, false, true)
		hi := jsonBound(js, raw.Components.Schemas, false, false)
		require.Equal(t, lo, schemaBound(t, f, true, msg), msg)
		require.Equal(t, hi, schemaBound(t, f, false, msg), msg)
		require.Equal(t, boundText(lo, hi), constraints(f), msg)

		applied := flattenSchema(doc, ref, true)
		require.Equal(t, jsonBound(js, raw.Components.Schemas, true, true), schemaBound(t, applied, true, msg), msg)
		require.Equal(t, jsonBound(js, raw.Components.Schemas, true, false), schemaBound(t, applied, false, msg), msg)

		if satisfiable(s.typ, lo, hi) {
			example := doc.ExampleValue(ref, ForRequest)
			var v float64
			switch n := example.(type) {
			case int64:
				v = float64(n)
			case float64:
				v = n
				if s.typ == "integer" {
					require.Equal(t, math.Trunc(v), v, msg)
				}
			default:
				require.Failf(t, "example is not a number", "%#v\n%s", example, msg)
			}
			require.True(t, within(v, lo, hi), "example %v\n%s", example, msg)

			// Where the keywords beside a $ref change nothing, the
			// example also meets the schema as JSON Schema reads it.
			same := reflect.DeepEqual(lo, jsonBound(js, raw.Components.Schemas, true, true)) &&
				reflect.DeepEqual(hi, jsonBound(js, raw.Components.Schemas, true, false))
			if same {
				validated++
				compiled := compileProperty(t, raw.Components.Schemas)
				require.NoError(t, compiled.Validate(example), "example %v\n%s", example, msg)
			}
		}

		other := mustParse(t, g.propSpec(s, comps))
		Diff(doc, other)
		Diff(other, doc)
		require.True(t, reflect.DeepEqual(pristine, doc), "the document is not changed\n"+msg)
	}
	t.Logf("%d of %d examples also validated as JSON Schema", validated, propCases)
	require.Greater(t, validated, propCases/4)
}

func compileProperty(t *testing.T, schemas map[string]any) *validate.Schema {
	t.Helper()
	doc, err := json.Marshal(map[string]any{"components": map[string]any{"schemas": as31(schemas)}})
	require.NoError(t, err)
	c, err := validate.NewCompiler(doc)
	require.NoError(t, err)
	compiled, err := c.Compile("property", map[string]any{"$ref": "spec.json#/components/schemas/S"})
	require.NoError(t, err)
	return compiled
}

// propText is spec text made of markdown and HTML metacharacters, line
// breaks, backticks and text that looks like a URL or an email address.
func propText(r *rand.Rand) string {
	pieces := []string{
		"<", ">", "&", "*", "_", "`", "``", "[", "]", "(", ")", "!", "#", "|", "~", `\`, "-", "+", "=", ":", "/", ".", "@", " ",
		"\n", "\r\n", "\t", "a", "Z", "9", "é",
		"http://e.co/x", "https://e.co", "ftp://e.co", "www.e.co", "a@b.co", "mailto:a@b.co", "<http://e.co>", "[x](http://e.co)", "![x](y)",
		"<img src=x onerror=alert(1)>", `<a href="x">`, "<script>", "</p>", "<b>", "&amp;", "&#60;",
		"```", "# ", "## ", "- ", "* ", "1. ", "> ", "    ", "**", "__", "~~", "---", "===", "|---|", "[ ] ",
	}
	var sb strings.Builder
	for range 1 + r.Intn(6) {
		sb.WriteString(pieces[r.Intn(len(pieces))])
	}
	return sb.String()
}

// TestDiffPropertyMarkdown checks that spec text in the markdown summary,
// tags, paths, property and header names, patterns, const and default
// values, renders as text: no headings or list items of its own, no tag
// and no link.
func TestDiffPropertyMarkdown(t *testing.T) {
	allowed := map[string]bool{"h1": true, "h2": true, "h3": true, "p": true, "ul": true, "li": true, "code": true}
	tagRe := regexp.MustCompile(`<(/?)([a-zA-Z][a-zA-Z0-9]*)`)
	r := rand.New(rand.NewSource(4))
	for i := range propCases {
		tag := propText(r)
		path := func(prefix string) string {
			return "/" + prefix + propText(r)
		}
		kept, removed, added := path("a"), path("b"), path("c")
		op := func(props, headers map[string]any) map[string]any {
			return map[string]any{"get": map[string]any{
				"tags": []any{tag},
				"responses": map[string]any{"200": map[string]any{
					"description": "ok",
					"headers":     headers,
					"content":     map[string]any{"application/json": map[string]any{"schema": map[string]any{"type": "object", "properties": props}}},
				}},
			}}
		}
		value := func() map[string]any {
			return map[string]any{"type": "string", "pattern": propText(r), "const": propText(r), "default": propText(r)}
		}
		prop, gone, extra := "p"+propText(r), "q"+propText(r), "r"+propText(r)
		header, oldHeader := "X"+propText(r), "Y"+propText(r)
		spec := func(paths map[string]any) []byte {
			out, err := json.Marshal(map[string]any{"openapi": "3.1.0", "info": map[string]any{"title": "t", "version": "1"}, "paths": paths})
			require.NoError(t, err)
			return out
		}
		base := spec(map[string]any{
			kept:    op(map[string]any{prop: value(), gone: value()}, map[string]any{oldHeader: map[string]any{"schema": value()}}),
			removed: op(map[string]any{}, map[string]any{}),
		})
		head := spec(map[string]any{
			kept:  op(map[string]any{prop: value(), extra: value()}, map[string]any{header: map[string]any{"schema": value()}}),
			added: op(map[string]any{}, map[string]any{}),
		})
		rep := Diff(mustParse(t, base), mustParse(t, head))
		md := rep.Markdown()
		html := string(markdown(md))
		msg := fmt.Sprintf("case %d\n%s\n%s", i, md, html)

		breaking := 0
		for _, c := range rep.Changes {
			if c.Breaking {
				breaking++
			}
		}
		headings := 1
		if breaking > 0 {
			headings += 2
		}
		if breaking < len(rep.Changes) {
			headings += 2
		}
		require.Equal(t, headings, len(regexp.MustCompile(`<h[1-6]`).FindAllString(html, -1)), msg)
		require.Equal(t, len(rep.Changes), strings.Count(html, "<li>"), msg)
		for _, m := range tagRe.FindAllStringSubmatch(html, -1) {
			require.True(t, allowed[m[2]], "tag <%s%s>\n%s", m[1], m[2], msg)
		}
	}
}

// TestDiffPropertySiteAndModel checks that where the keyword beside a
// reference to a model and the same keyword in the model change together,
// the use reports the bound that held there before against the one that
// holds after, breaking when that narrows in a request or widens in a
// response, and nothing when the bound that holds there is the same or a
// change to the model alone moved it; the model reports its own change once.
func TestDiffPropertySiteAndModel(t *testing.T) {
	paths := []struct {
		path    string
		request bool
	}{
		{"POST /things › query › q", true},
		{"POST /things › request body › name", true},
		{"POST /things › response 200 › name", false},
	}
	keys := []string{"minimum", "maximum", "minLength", "maxLength"}
	r := rand.New(rand.NewSource(6))
	value := func() *int {
		if r.Intn(3) == 0 {
			return nil
		}
		v := r.Intn(6)
		return &v
	}
	text := func(v *int) string {
		if v == nil {
			return "none"
		}
		return fmt.Sprint(*v)
	}
	combined := 0
	for i := range propCases {
		key := keys[r.Intn(len(keys))]
		lower := key == "minimum" || key == "minLength"
		typ := "number"
		if strings.HasSuffix(key, "Length") {
			typ = "string"
		}
		siteBase, modelBase, siteHead, modelHead := value(), value(), value(), value()
		tightest := func(a, b *int) *int {
			if a == nil || b != nil && (*b > *a) == lower {
				return b
			}
			return a
		}
		use := func(v *int) string {
			if v == nil {
				return `{"$ref": "#/components/schemas/M"}`
			}
			if r.Intn(2) == 0 {
				return fmt.Sprintf(`{"$ref": "#/components/schemas/M", "%s": %d}`, key, *v)
			}
			return fmt.Sprintf(`{"allOf": [{"$ref": "#/components/schemas/M"}], "%s": %d}`, key, *v)
		}
		model := func(v *int) string {
			if v == nil {
				return fmt.Sprintf(`"M": {"type": "%s"}`, typ)
			}
			return fmt.Sprintf(`"M": {"type": "%s", "%s": %d}`, typ, key, *v)
		}
		baseDoc := keywordSpecWith(t, "", use(siteBase), model(modelBase))
		headDoc := keywordSpecWith(t, "", use(siteHead), model(modelHead))
		rep := Diff(baseDoc, headDoc)
		msg := fmt.Sprintf("case %d: %s at the use %s to %s, in the model %s to %s\n%s", i, key, text(siteBase), text(siteHead), text(modelBase), text(modelHead), rep.Markdown())

		require.Equal(t, !reflect.DeepEqual(modelBase, modelHead), rep.Model("M") != nil, msg)

		before := tightest(siteBase, modelBase)
		after := tightest(siteHead, modelHead)
		moved := tightest(siteBase, modelHead)
		var detail string
		var narrows, widens bool
		if !reflect.DeepEqual(moved, after) && !reflect.DeepEqual(before, after) {
			switch {
			case before == nil:
				detail = fmt.Sprintf("%s added: %d", key, *after)
			case after == nil:
				detail = fmt.Sprintf("%s removed (was %d)", key, *before)
			case *after > *before:
				detail = fmt.Sprintf("%s raised from %d to %d", key, *before, *after)
			default:
				detail = fmt.Sprintf("%s lowered from %d to %d", key, *before, *after)
			}
			narrows = after != nil && (before == nil || (*after > *before) == lower)
			widens = before != nil && (after == nil || (*after > *before) != lower)
			if key == "minLength" {
				count := func(v *int) int {
					if v == nil {
						return 0
					}
					return *v
				}
				narrows = count(after) > count(before)
				widens = count(after) < count(before)
			}
			if !reflect.DeepEqual(modelBase, modelHead) {
				combined++
			}
		}

		od := rep.Operation("POST", "/things")
		for _, p := range paths {
			var want, got []string
			if detail != "" {
				breaking := widens
				if p.request {
					breaking = narrows
				}
				want = append(want, fmt.Sprintf("%s | %t", detail, breaking))
			}
			if od != nil {
				for _, c := range changesAt(od, p.path) {
					if c.model == "" {
						got = append(got, fmt.Sprintf("%s | %t", c.Detail, c.Breaking))
					}
				}
			}
			require.Equal(t, want, got, p.path+"\n"+msg)
		}
	}
	t.Logf("%d of %d cases changed the use and the model together", combined, propCases)
	require.Greater(t, combined, propCases/10, "many cases change both")
}

// TestDiffPropertyIntegerBounds checks that an integer schema and its twin,
// with each bound written in the other form, inclusive or exclusive, at the
// value that admits the same integers, in the 3.0 or the 3.1 spelling,
// compare as equal, and that the same two schemas of type number, which
// admit different numbers, do not.
func TestDiffPropertyIntegerBounds(t *testing.T) {
	r := rand.New(rand.NewSource(8))
	spell := func(version string, lower bool, v float64, excl bool) string {
		name, flag := "maximum", "exclusiveMaximum"
		if lower {
			name, flag = "minimum", "exclusiveMinimum"
		}
		switch {
		case !excl:
			return fmt.Sprintf(`"%s": %v`, name, v)
		case version == "3.0.3":
			return fmt.Sprintf(`"%s": %v, "%s": true`, name, v, flag)
		}
		return fmt.Sprintf(`"%s": %v`, flag, v)
	}
	for i := range propCases {
		version := "3.1.0"
		if r.Intn(2) == 0 {
			version = "3.0.3"
		}
		var base, twin []string
		for _, lower := range []bool{true, false} {
			if r.Intn(4) == 0 {
				continue
			}
			v := float64(r.Intn(41)-20) / 2
			excl := r.Intn(2) == 0
			// The bound in the other form that admits the same integers.
			var w float64
			switch {
			case lower && excl:
				w = math.Floor(v) + 1
			case lower:
				w = math.Ceil(v) - 1
			case excl:
				w = math.Ceil(v) - 1
			default:
				w = math.Floor(v) + 1
			}
			base = append(base, spell(version, lower, v, excl))
			twin = append(twin, spell(version, lower, w, !excl))
		}
		if len(base) == 0 {
			continue
		}
		for _, typ := range []string{"integer", "number"} {
			schema := func(bounds []string) string {
				return `{"type": "` + typ + `", ` + strings.Join(bounds, ", ") + `}`
			}
			rep := Diff(keywordSpec(t, version, schema(base)), keywordSpec(t, version, schema(twin)))
			msg := fmt.Sprintf("case %d: %s to %s\n%s", i, schema(base), schema(twin), rep.Markdown())
			require.Equal(t, typ == "integer", rep.Empty(), msg)
		}
	}
}

// TestDiffPropertyNarrowFromNone checks that a type, an enum or the schema
// of the items stated where there was none, on a schema or beside a
// reference to a model, narrows what it accepts: every change it reports
// breaks the request side and none the response side, and the reverse
// change, which widens, the other way round.
func TestDiffPropertyNarrowFromNone(t *testing.T) {
	r := rand.New(rand.NewSource(9))
	extras := []string{`"minLength": 2`, `"maxLength": 9`, `"description": "d"`, `"minItems": 1`, `"maximum": 7`}
	for i := range propCases {
		var shared []string
		for _, e := range extras {
			if r.Intn(3) == 0 {
				shared = append(shared, e)
			}
		}
		types := []string{"string", "integer", "number", "boolean", "object", "array"}
		var added string
		var base []string
		switch r.Intn(3) {
		case 0:
			added = `"type": "` + types[r.Intn(len(types))] + `"`
		case 1:
			base = []string{`"type": "` + types[r.Intn(3)] + `"`}
			values := []string{`"a"`, `1`, `"b"`, `2.5`}
			added = `"enum": [` + strings.Join(values[:1+r.Intn(len(values))], ", ") + `]`
		default:
			base = []string{`"type": "array"`}
			items := []string{`"type": "` + types[r.Intn(4)] + `"`}
			if r.Intn(2) == 0 {
				items = append(items, `"minimum": 1`)
			}
			added = `"items": {` + strings.Join(items, ", ") + `}`
		}
		base = append(base, shared...)
		head := append(slices.Clone(base), added)
		model := r.Intn(2) == 0 && !strings.HasPrefix(added, `"type"`)
		write := func(kw []string) (string, string) {
			if !model {
				return "{" + strings.Join(kw, ", ") + "}", `"M": {}`
			}
			// The keyword on the model, used through a reference.
			return `{"$ref": "#/components/schemas/M"}`, `"M": {` + strings.Join(kw, ", ") + `}`
		}
		if strings.HasPrefix(added, `"type"`) && r.Intn(2) == 0 {
			// The type beside a reference to a model that states none.
			m := `"M": {` + strings.Join(shared, ", ") + `}`
			use := `{"$ref": "#/components/schemas/M"`
			if r.Intn(2) == 0 {
				use = `{"allOf": [{"$ref": "#/components/schemas/M"}]`
			}
			write = func(kw []string) (string, string) {
				if len(kw) > len(shared) {
					return use + ", " + added + "}", m
				}
				return use + "}", m
			}
		}
		for n, pair := range [][2][]string{{base, head}, {head, base}} {
			narrows := n == 0
			bs, bm := write(pair[0])
			hs, hm := write(pair[1])
			rep := Diff(keywordSpecWith(t, "", bs, bm), keywordSpecWith(t, "", hs, hm))
			msg := fmt.Sprintf("case %d: %s %s to %s %s\n%s", i, bs, bm, hs, hm, rep.Markdown())
			require.False(t, rep.Empty(), msg)
			for _, c := range rep.Changes {
				request := strings.Contains(c.Path, "› query ›") || strings.Contains(c.Path, "› request body")
				response := strings.Contains(c.Path, "› response ")
				if !request && !response {
					// The model's own entry breaks the side its uses break.
					require.True(t, c.Breaking, msg)
					continue
				}
				require.Equal(t, request == narrows, c.Breaking, c.Path+"\n"+msg)
			}
		}
	}
}

// shapeCases is how many documents the reflexivity property runs, notCases
// how many documents of models that refer to themselves and each other
// under nots it runs too, and cycleCases how many pairs of documents whose
// models refer to each other in a cycle the operation order property runs,
// each with its operations in both orders.
const (
	shapeCases = 3000
	notCases   = 3000
	cycleCases = 1500
)

// shapeGen generates documents whose schemas mix the shapes that reach
// nots and folded unions: references among a few models, under nots too,
// so nots form cycles through them, unions nested in unions, unions of one
// schema and {type: null}, and the keywords whose changes narrow or widen.
type shapeGen struct {
	r      *rand.Rand
	models []string
}

// pick returns one of options at random.
func pick[T any](r *rand.Rand, options ...T) T {
	return options[r.Intn(len(options))]
}

// ref is a reference to one of the models.
func (g *shapeGen) ref() map[string]any {
	return map[string]any{"$ref": "#/components/schemas/" + pick(g.r, g.models...)}
}

// schema generates a schema at most depth levels deep.
func (g *shapeGen) schema(depth int) any {
	if depth <= 0 {
		return pick(g.r,
			map[string]any{"type": "string"},
			map[string]any{"type": "string", "maxLength": 1 + g.r.Intn(3)},
			map[string]any{"type": "string", "enum": []any{"a", "b"}[:1+g.r.Intn(2)]},
			map[string]any{"type": "string", "format": "date"},
			map[string]any{"type": "integer", "maximum": g.r.Intn(3)},
			map[string]any{"type": "object", "maxProperties": 1 + g.r.Intn(3)},
			map[string]any{"type": "null"},
			map[string]any{},
			g.ref(),
		)
	}
	switch g.r.Intn(13) {
	case 0, 1:
		return g.ref()
	case 10:
		// An object that refers to a model, often its own, under a not.
		return map[string]any{"type": "object", "maxProperties": 1 + g.r.Intn(3), "properties": map[string]any{"n": map[string]any{"not": g.ref()}}}
	case 11:
		return map[string]any{"type": "object", "maxProperties": 1 + g.r.Intn(3), "not": g.ref()}
	case 2:
		return map[string]any{"not": g.schema(depth - 1)}
	case 3:
		return map[string]any{"not": g.ref()}
	case 4:
		return map[string]any{pick(g.r, "anyOf", "oneOf"): g.list(depth)}
	case 5:
		// A union of one schema and null, which Parse folds, whose one
		// schema may be a union itself.
		return map[string]any{pick(g.r, "anyOf", "oneOf"): []any{g.schema(depth - 1), map[string]any{"type": "null"}}}
	case 6:
		inner := map[string]any{pick(g.r, "anyOf", "oneOf"): g.list(depth - 1)}
		return map[string]any{pick(g.r, "anyOf", "oneOf"): []any{inner, map[string]any{"type": "null"}}}
	case 7:
		return map[string]any{"allOf": []any{g.schema(depth - 1)}}
	case 8:
		return map[string]any{"type": "array", "items": g.schema(depth - 1)}
	default:
		props := map[string]any{}
		for i := range 1 + g.r.Intn(2) {
			props[fmt.Sprintf("p%d", i)] = g.schema(depth - 1)
		}
		s := map[string]any{"type": "object", "properties": props}
		switch g.r.Intn(4) {
		case 0:
			s["additionalProperties"] = false
		case 1:
			s["additionalProperties"] = g.schema(depth - 1)
		}
		if g.r.Intn(3) == 0 {
			s["maxProperties"] = 1 + g.r.Intn(3)
		}
		return s
	}
}

// list generates one to three variants.
func (g *shapeGen) list(depth int) []any {
	var out []any
	for range 1 + g.r.Intn(3) {
		out = append(out, g.schema(depth-1))
	}
	return out
}

// document generates a document of up to four models and three operations,
// each taking or returning one schema.
func (g *shapeGen) document() map[string]any {
	g.models = nil
	for i := range 1 + g.r.Intn(4) {
		g.models = append(g.models, fmt.Sprintf("M%d", i))
	}
	schemas := map[string]any{}
	for _, name := range g.models {
		schemas[name] = g.schema(2)
	}
	paths := map[string]any{}
	for i := range 1 + g.r.Intn(4) {
		s := g.schema(2)
		if g.r.Intn(2) == 0 {
			paths[fmt.Sprintf("/o%d", i)] = map[string]any{"post": map[string]any{
				"requestBody": map[string]any{"content": map[string]any{"application/json": map[string]any{"schema": s}}},
				"responses":   map[string]any{"200": map[string]any{"description": "ok"}},
			}}
			continue
		}
		paths[fmt.Sprintf("/o%d", i)] = map[string]any{"get": map[string]any{
			"responses": map[string]any{"200": map[string]any{"description": "ok", "content": map[string]any{"application/json": map[string]any{"schema": s}}}},
		}}
	}
	return map[string]any{
		"openapi":    pick(g.r, "3.0.3", "3.1.0"),
		"info":       map[string]any{"title": "t", "version": "1"},
		"paths":      paths,
		"components": map[string]any{"schemas": schemas},
	}
}

// mutate changes doc in place: it replaces a few schemas at random with new
// ones, and renames some models, references to them included.
func (g *shapeGen) mutate(doc map[string]any) {
	schemas := doc["components"].(map[string]any)["schemas"].(map[string]any)
	for name, s := range schemas {
		schemas[name] = g.change(s)
	}
	for _, item := range doc["paths"].(map[string]any) {
		for _, op := range item.(map[string]any) {
			op := op.(map[string]any)
			content := op["responses"].(map[string]any)["200"].(map[string]any)["content"]
			body, ok := op["requestBody"].(map[string]any)
			if ok {
				content = body["content"]
			}
			media := content.(map[string]any)["application/json"].(map[string]any)
			media["schema"] = g.change(media["schema"])
		}
	}

	renamed := map[string]string{}
	for _, name := range g.models {
		if g.r.Intn(2) == 0 {
			renamed[name] = "N" + name[1:]
		}
	}
	var rename func(v any)
	rename = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			ref, ok := v["$ref"].(string)
			if ok {
				to, ok := renamed[strings.TrimPrefix(ref, "#/components/schemas/")]
				if ok {
					v["$ref"] = "#/components/schemas/" + to
				}
			}
			for _, child := range v {
				rename(child)
			}
		case []any:
			for _, child := range v {
				rename(child)
			}
		}
	}
	rename(doc)
	for from, to := range renamed {
		schemas[to] = schemas[from]
		delete(schemas, from)
	}
}

// change returns s, or a new schema in its place, with some of the schemas
// inside it replaced too.
func (g *shapeGen) change(s any) any {
	if g.r.Intn(6) == 0 {
		return g.schema(1)
	}
	m, ok := s.(map[string]any)
	if !ok {
		return s
	}
	for _, key := range []string{"maxLength", "maxProperties", "maximum"} {
		_, ok := m[key]
		if ok && g.r.Intn(2) == 0 {
			m[key] = 1 + g.r.Intn(3)
		}
	}
	for _, key := range []string{"not", "items", "additionalProperties"} {
		child, ok := m[key].(map[string]any)
		if ok {
			m[key] = g.change(child)
		}
	}
	props, ok := m["properties"].(map[string]any)
	if ok {
		for name, child := range props {
			props[name] = g.change(child)
		}
	}
	for _, key := range []string{"anyOf", "oneOf", "allOf"} {
		list, ok := m[key].([]any)
		if ok {
			for i, child := range list {
				list[i] = g.change(child)
			}
		}
	}
	return m
}

// TestDiffPropertyOperationOrder checks that the changes of an operation do
// not depend on the operations compared before it, on pairs of documents
// whose models all refer to each other in a cycle, under nots too, and are
// all renamed, with the operations in both orders.
func TestDiffPropertyOperationOrder(t *testing.T) {
	r := rand.New(rand.NewSource(3))
	for i := range cycleCases {
		c := cycleCase(r)
		want := map[string][]Change{}
		for _, reverse := range []bool{false, true} {
			base := c.spec(c.base, false, reverse)
			head := c.spec(c.head, true, reverse)
			rep := Diff(mustParse(t, base), mustParse(t, head))
			for _, od := range rep.Operations {
				key := od.Method + " " + od.Path
				_, ok := want[key]
				if !ok {
					want[key] = od.Changes
					continue
				}
				require.Equal(t, want[key], od.Changes, "case %d, %s\nbase: %s\nhead: %s", i, key, base, head)
			}
		}
	}
}

// modelCycle is a generated pair of documents whose models refer to each
// other in a cycle under nots: the models of each side, renamed in the head,
// and the operations, each entering the cycle at one of them.
type modelCycle struct {
	base, head []string
	ops        []cycleOp
}

// cycleOp is an operation of a modelCycle: whether its schema is the
// request body, and its schema, written with %s for the model prefix.
type cycleOp struct {
	request bool
	schema  string
}

// cycleCase generates three to five models that refer to each other, each
// to the next and at random to others, through properties,
// additionalProperties and items, under no, one or two nots, with a keyword
// that the head may change, and two to four operations that refer to models
// under nots.
func cycleCase(r *rand.Rand) modelCycle {
	n := 3 + r.Intn(3)
	refTo := func(i int) string {
		s := fmt.Sprintf(`{"$ref": "#/components/schemas/%%s%d"}`, i)
		for range r.Intn(3) {
			s = `{"not": ` + s + `}`
		}
		return s
	}
	var c modelCycle
	for i := range n {
		members := []string{}
		targets := []int{(i + 1) % n}
		for range r.Intn(3) {
			targets = append(targets, r.Intn(n))
		}
		for j, target := range targets {
			switch r.Intn(3) {
			case 0:
				members = append(members, fmt.Sprintf(`"properties": {"p%d": %s}`, j, refTo(target)))
			case 1:
				members = append(members, `"additionalProperties": `+refTo(target))
			default:
				members = append(members, `"items": `+refTo(target))
			}
		}
		seen := map[string]bool{}
		var unique []string
		for _, m := range members {
			key := m[:strings.Index(m, ":")]
			if !seen[key] {
				seen[key] = true
				unique = append(unique, m)
			}
		}
		keyword := pick(r, "maxLength", "maxProperties", "minItems")
		base := fmt.Sprintf(`"%%s%d": {%s, "%s": %d}`, i, strings.Join(unique, ", "), keyword, 2)
		head := base
		switch r.Intn(3) {
		case 0:
			head = fmt.Sprintf(`"%%s%d": {%s, "%s": %d}`, i, strings.Join(unique, ", "), keyword, 1+r.Intn(3))
		case 1:
			head = fmt.Sprintf(`"%%s%d": {%s}`, i, strings.Join(unique, ", "))
		}
		c.base = append(c.base, base)
		c.head = append(c.head, head)
	}
	for range 2 + r.Intn(3) {
		c.ops = append(c.ops, cycleOp{request: r.Intn(2) == 0, schema: refTo(r.Intn(n))})
	}
	return c
}

// spec writes a side of c: models are named C0, C1, and so on in the base
// and R0, R1, and so on in the head, and the operations are /o0, /o1, and so
// on, in reverse order where reverse is set.
func (c modelCycle) spec(models []string, head, reverse bool) []byte {
	prefix := "C"
	if head {
		prefix = "R"
	}
	fill := func(s string) string {
		return strings.ReplaceAll(s, "%s", prefix)
	}
	var schemas []string
	for _, m := range models {
		schemas = append(schemas, fill(m))
	}
	var paths []string
	for i, op := range c.ops {
		route := fmt.Sprintf("/o%d", i)
		if op.request {
			paths = append(paths, `"`+route+`": {"post": {"requestBody": {"content": {"application/json": {"schema": `+fill(op.schema)+`}}}, "responses": {"200": {"description": "ok"}}}}`)
		} else {
			paths = append(paths, `"`+route+`": {"get": {"responses": {"200": {"description": "ok", "content": {"application/json": {"schema": `+fill(op.schema)+`}}}}}}`)
		}
	}
	if reverse {
		slices.Reverse(paths)
	}
	return []byte(`{"openapi": "3.1.0", "info": {"title": "t", "version": "1"}, "paths": {` + strings.Join(paths, ", ") + `}, "components": {"schemas": {` + strings.Join(schemas, ", ") + `}}}`)
}

// TestDiffPropertyReflexive checks that two parses of the same document
// compare as equal, also for documents whose models refer to themselves and
// to each other under nots, which compare in time.
func TestDiffPropertyReflexive(t *testing.T) {
	g := &shapeGen{r: rand.New(rand.NewSource(2))}
	for i := range shapeCases {
		spec, err := json.Marshal(g.document())
		require.NoError(t, err)
		r := Diff(mustParse(t, spec), mustParse(t, spec))
		require.True(t, r.Empty(), "case %d\n%s\n%s", i, spec, r.Markdown())
	}

	r := rand.New(rand.NewSource(4))
	var spent time.Duration
	for i := range notCases {
		spec := notDocument(r)
		base := mustParse(t, spec)
		head := mustParse(t, spec)
		start := time.Now()
		rep := Diff(base, head)
		spent += time.Since(start)
		require.True(t, rep.Empty(), "case %d\n%s\n%s", i, spec, rep.Markdown())
	}
	// The bound is far above the time these take, a few seconds under the
	// race detector on a slow machine, and far below what a comparison that
	// grows with the number of paths through a document takes.
	require.Less(t, spent, 30*time.Second)
}

// notDocument generates a document of two to seven models, each an object
// of one to twelve properties that refer to the models, itself included,
// under nots: a not of a model, of an allOf that joins a model to a bound,
// of a not of a model, or of an object whose property is a not of a model,
// some of them as the items of an array, and up to three operations that
// take or return a model or a not of one.
func notDocument(r *rand.Rand) []byte {
	n := 2 + r.Intn(6)
	ref := func() string {
		return fmt.Sprintf(`{"$ref": "#/components/schemas/M%d"}`, r.Intn(n))
	}
	var models []string
	for i := range n {
		var props []string
		for j := range 1 + r.Intn(12) {
			var p string
			switch r.Intn(5) {
			case 0:
				p = `{"not": ` + ref() + `}`
			case 1:
				p = fmt.Sprintf(`{"not": {"allOf": [%s, {"maxLength": %d}]}}`, ref(), 1+r.Intn(5))
			case 2:
				p = `{"not": {"not": ` + ref() + `}}`
			case 3:
				p = `{"not": {"type": "object", "properties": {"x": {"not": ` + ref() + `}}}}`
			default:
				p = ref()
			}
			if r.Intn(4) == 0 {
				p = `{"type": "array", "items": ` + p + `}`
			}
			props = append(props, fmt.Sprintf(`"p%d": %s`, j, p))
		}
		models = append(models, fmt.Sprintf(`"M%d": {"type": "object", "properties": {%s}}`, i, strings.Join(props, ", ")))
	}
	var paths []string
	for i := range 1 + r.Intn(3) {
		s := ref()
		if r.Intn(2) == 0 {
			s = `{"not": ` + s + `}`
		}
		paths = append(paths, fmt.Sprintf(`"/o%d": {"post": {"requestBody": {"content": {"application/json": {"schema": %s}}}, "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": %s}}}}}}`, i, s, ref()))
	}
	return []byte(`{"openapi": "3.1.0", "info": {"title": "t", "version": "1"}, "paths": {` + strings.Join(paths, ", ") + `}, "components": {"schemas": {` + strings.Join(models, ", ") + `}}}`)
}
