package reference

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Models are filed by family in the sidebar and on the Models page: each
// family is headed by an entity (see graphEntities) and holds the models
// that belong to it, such as its requests, responses and enums. A model
// joins, in this order:
//
//  1. the head whose display name is the longest prefix of its own at a
//     word boundary (Benchmark takes BenchmarkStatus but not Benchmarks);
//  2. the one head that refers to it, directly or through models that are
//     not heads (the enums and parts of an entity);
//  3. the one head it refers to the same way (wrappers such as
//     CreateAPIKeyResponse);
//
// and otherwise goes to the Shared group. A model only joins a head of its
// own module (see schemaGroup), so families stay inside module folders.

// familyHeads returns the component schemas of d that head a family.
func (d *Document) familyHeads() map[string]bool {
	declared := d.declaresTables()
	return d.graphEntities(d.graphObjects(declared), declared)
}

// isFamilyPrefix reports whether head starts name and ends at a word
// boundary of it: the next character is upper case, a digit or "_".
func isFamilyPrefix(head, name string) bool {
	if head == "" || len(name) <= len(head) || !strings.HasPrefix(name, head) {
		return false
	}
	r, _ := utf8.DecodeRuneInString(name[len(head):])
	return unicode.IsUpper(r) || unicode.IsDigit(r) || r == '_'
}

// familyRefs lists the component schemas a schema refers to, each once and
// in order, through properties, items, compositions and additional
// properties. It caches by component name.
type familyRefs struct {
	d     *Document
	cache map[string][]string
}

func newFamilyRefs(d *Document) *familyRefs {
	return &familyRefs{d: d, cache: map[string][]string{}}
}

func (r *familyRefs) of(name string) []string {
	out, ok := r.cache[name]
	if ok {
		return out
	}
	seen := map[string]bool{}
	var walk func(s *Schema)
	walk = func(s *Schema) {
		if s == nil {
			return
		}
		if s.Ref != "" {
			target := RefName(s.Ref, refSchemas)
			if target != "" && !seen[target] {
				seen[target] = true
				out = append(out, target)
			}
			return
		}
		for _, key := range s.Properties.Keys {
			walk(s.Properties.Map[key])
		}
		walk(s.Items)
		for _, list := range [][]*Schema{s.AllOf, s.OneOf, s.AnyOf} {
			for _, m := range list {
				walk(m)
			}
		}
		walk(s.Not)
		walk(s.AdditionalSchema())
	}
	walk(r.d.Components.Schemas.Map[name])
	r.cache[name] = out
	return out
}

// reach returns the heads found from name: it follows the refs of name
// through models that are not heads and stops at each head.
func (r *familyRefs) reach(name string, heads map[string]bool) map[string]bool {
	found := map[string]bool{}
	seen := map[string]bool{name: true}
	queue := []string{name}
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		for _, target := range r.of(next) {
			if seen[target] {
				continue
			}
			seen[target] = true
			if heads[target] {
				found[target] = true
				continue
			}
			queue = append(queue, target)
		}
	}
	return found
}

// users returns, for each model that is not a head, the heads that refer to
// it directly or through models that are not heads.
func (r *familyRefs) users(heads map[string]bool) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for head := range heads {
		seen := map[string]bool{head: true}
		queue := []string{head}
		for len(queue) > 0 {
			next := queue[0]
			queue = queue[1:]
			for _, target := range r.of(next) {
				if seen[target] || heads[target] {
					continue
				}
				seen[target] = true
				if out[target] == nil {
					out[target] = map[string]bool{}
				}
				out[target][head] = true
				queue = append(queue, target)
			}
		}
	}
	return out
}

// familyIndex files models by family, see the comment at the top.
type familyIndex struct {
	heads   map[string]bool
	display func(string) string
	refs    *familyRefs
	used    map[string]map[string]bool
}

func newFamilyIndex(d *Document, heads map[string]bool, display func(string) string) *familyIndex {
	refs := newFamilyRefs(d)
	return &familyIndex{heads: heads, display: display, refs: refs, used: refs.users(heads)}
}

// head returns the family head of name: name itself for a head, another
// head, or "" for the Shared group.
func (x *familyIndex) head(name string) string {
	if x.heads[name] {
		return name
	}
	module := schemaGroup(name)
	own := lastSegment(x.display(name))
	best := ""
	for h := range x.heads {
		if schemaGroup(h) != module {
			continue
		}
		short := lastSegment(x.display(h))
		if !isFamilyPrefix(short, own) {
			continue
		}
		if best == "" || len(short) > len(lastSegment(x.display(best))) || len(short) == len(lastSegment(x.display(best))) && h < best {
			best = h
		}
	}
	if best != "" {
		return best
	}
	only := x.single(x.used[name], module)
	if only != "" {
		return only
	}
	return x.single(x.refs.reach(name, x.heads), module)
}

// single returns the one head of module in set, or "".
func (x *familyIndex) single(set map[string]bool, module string) string {
	only := ""
	for h := range set {
		if schemaGroup(h) != module {
			continue
		}
		if only != "" {
			return ""
		}
		only = h
	}
	return only
}

// modelFamily is one entry of a Models sidebar list: a model with the
// models filed under it (none for a plain row), or, with Head nil, the
// Shared group. Changes sums up the diff states of the members, for a
// collapsed family.
type modelFamily struct {
	Head    *modelView
	Members []*modelView
	Changes []familyChange
}

// familyChange counts the members of a family in one diff state.
type familyChange struct {
	Class  string
	Letter string
	Label  string
	Count  int
}

// families groups models, all of one module, by family, with headOf giving
// the family head of each. Without a head among them each model is a plain
// row.
func families(models []*modelView, headOf func(string) string) []modelFamily {
	byShort := func(list []*modelView) {
		sort.SliceStable(list, func(i, j int) bool {
			return strings.ToLower(list[i].Short) < strings.ToLower(list[j].Short)
		})
	}
	byShort(models)

	heads := map[string]int{}
	var out []modelFamily
	for _, m := range models {
		if headOf(m.Name) == m.Name {
			heads[m.Name] = len(out)
			out = append(out, modelFamily{Head: m})
		}
	}
	if len(out) == 0 {
		for _, m := range models {
			out = append(out, modelFamily{Head: m})
		}
		return out
	}

	var shared []*modelView
	for _, m := range models {
		head := headOf(m.Name)
		if head == m.Name {
			continue
		}
		i, ok := heads[head]
		if !ok {
			shared = append(shared, m)
			continue
		}
		out[i].Members = append(out[i].Members, m)
	}
	if len(shared) > 0 {
		out = append(out, modelFamily{Members: shared})
	}
	for i := range out {
		out[i].Changes = familyChanges(out[i].Members)
	}
	return out
}

// familyChanges counts the members in each diff state, added first.
func familyChanges(members []*modelView) []familyChange {
	var out []familyChange
	for _, class := range []string{"diff-added", "diff-changed", "diff-removed"} {
		c := familyChange{Class: class}
		for _, m := range members {
			if m.Diff == nil || m.Diff.Class != class {
				continue
			}
			c.Count++
			c.Letter = m.Diff.Letter
		}
		if c.Count == 0 {
			continue
		}
		c.Label = strings.TrimPrefix(class, "diff-")
		out = append(out, c)
	}
	return out
}

// familyHeadOf returns the function that gives the family head of each
// model, see the comment at the top. The heads are the head document's
// entities plus, in diff mode, the removed models that were entities of the
// base document; removed models are filed by the base document's refs.
func (b *builder) familyHeadOf() func(string) string {
	heads := b.d.familyHeads()
	removed := map[string]bool{}
	var base *familyIndex
	if b.diff != nil {
		baseHeads := b.diff.base.familyHeads()
		for _, name := range b.diff.removedModels(b.d) {
			removed[name] = true
			if baseHeads[name] {
				heads[name] = true
			}
		}
		base = newFamilyIndex(b.diff.base, heads, b.display)
	}
	head := newFamilyIndex(b.d, heads, b.display)
	memo := map[string]string{}
	return func(name string) string {
		out, ok := memo[name]
		if ok {
			return out
		}
		if removed[name] {
			out = base.head(name)
		} else {
			out = head.head(name)
		}
		memo[name] = out
		return out
	}
}
