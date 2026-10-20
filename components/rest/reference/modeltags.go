package reference

import (
	"sort"
	"strings"
)

// Models are filed by operation tag in the sidebar and on the Models page,
// each in exactly one group. A model joins, in this order:
//
//  1. the first tag its x-tags extension names (a list or a single string);
//  2. the tag whose operations use it most, through their parameters,
//     request bodies and responses and the schemas those refer to;
//  3. the tag most of the other models of its module (see schemaGroup) were
//     filed under by the rules above, for models no operation uses, such as
//     the request types of operations that take parameters instead;
//
// and otherwise, used by several tags equally or alone in its module, goes
// to the Shared group. A document without tags keeps the module folders, see
// schemaGroup.

// sharedGroup names the group of the models no one tag claims.
const sharedGroup = "Shared"

// modelGroup is one folder of the Models sidebar: a tag (or, without tags, a
// module) and its models. Changes sums up their diff states, for when the
// folder is collapsed.
type modelGroup struct {
	Name    string
	Shared  bool
	Models  []*modelView
	Changes []groupChange
}

// groupChange counts the models of a group in one diff state.
type groupChange struct {
	Class  string
	Letter string
	Label  string
	Count  int
}

// groupChanges counts the models in each diff state, added first.
func groupChanges(models []*modelView) []groupChange {
	var out []groupChange
	for _, class := range []string{"diff-added", "diff-changed", "diff-removed"} {
		c := groupChange{Class: class}
		for _, m := range models {
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

// schemaTags returns the tags of the x-tags extension of s, which may hold
// a list of tag names or a single one.
func schemaTags(s *Schema) []string {
	if s == nil {
		return nil
	}
	var out []string
	switch v := s.Extensions["x-tags"].(type) {
	case string:
		out = append(out, v)
	case []any:
		for _, item := range v {
			name, ok := item.(string)
			if ok {
				out = append(out, name)
			}
		}
	case []string:
		out = append(out, v...)
	}
	var tags []string
	for _, name := range out {
		name = strings.TrimSpace(name)
		if name != "" {
			tags = append(tags, name)
		}
	}
	return tags
}

// hasModelTags reports whether d can file its models by tag: an operation
// has a tag or a schema names one in x-tags.
func (d *Document) hasModelTags() bool {
	for _, ref := range d.Operations() {
		if len(ref.Operation.Tags) > 0 {
			return true
		}
	}
	for _, name := range d.Components.Schemas.Keys {
		if len(schemaTags(d.Components.Schemas.Map[name])) > 0 {
			return true
		}
	}
	return false
}

// modelTags returns the tag each component schema of d is filed under, see
// the comment at the top; models of the Shared group are left out.
func (d *Document) modelTags() map[string]string {
	uses := map[string]map[string]int{}
	for _, ref := range d.Operations() {
		tags := ref.Operation.Tags
		if len(tags) == 0 {
			tags = []string{"Other"}
		}
		for name := range d.operationSchemas(ref) {
			if uses[name] == nil {
				uses[name] = map[string]int{}
			}
			for _, tag := range tags {
				uses[name][tag]++
			}
		}
	}

	out := map[string]string{}
	for _, name := range d.Components.Schemas.Keys {
		explicit := schemaTags(d.Components.Schemas.Map[name])
		if len(explicit) > 0 {
			out[name] = explicit[0]
			continue
		}
		best := plurality(uses[name])
		if best != "" {
			out[name] = best
		}
	}

	byModule := map[string]map[string]int{}
	for name, tag := range out {
		module := schemaGroup(name)
		if module == "" {
			continue
		}
		if byModule[module] == nil {
			byModule[module] = map[string]int{}
		}
		byModule[module][tag]++
	}
	for _, name := range d.Components.Schemas.Keys {
		if out[name] != "" || len(uses[name]) > 0 {
			continue
		}
		best := plurality(byModule[schemaGroup(name)])
		if best != "" {
			out[name] = best
		}
	}
	return out
}

// plurality returns the key with the highest count, or "" when counts is
// empty or two keys share the highest count.
func plurality(counts map[string]int) string {
	best, top, tie := "", 0, false
	for key, n := range counts {
		switch {
		case n > top:
			best, top, tie = key, n, false
		case n == top:
			tie = true
		}
	}
	if tie {
		return ""
	}
	return best
}

// operationSchemas returns the component schemas an operation uses: those
// its parameters, request body and responses refer to, and the ones those
// refer to in turn.
func (d *Document) operationSchemas(ref OperationRef) map[string]bool {
	found := map[string]bool{}
	var queue []string
	var walk func(s *Schema, depth int)
	walk = func(s *Schema, depth int) {
		if s == nil || depth > 32 {
			return
		}
		if s.Ref != "" {
			name := RefName(s.Ref, refSchemas)
			if name != "" && !found[name] {
				found[name] = true
				queue = append(queue, name)
			}
			return
		}
		for _, key := range s.Properties.Keys {
			walk(s.Properties.Map[key], depth+1)
		}
		walk(s.Items, depth+1)
		for _, list := range [][]*Schema{s.AllOf, s.OneOf, s.AnyOf} {
			for _, m := range list {
				walk(m, depth+1)
			}
		}
		walk(s.Not, depth+1)
		walk(s.AdditionalSchema(), depth+1)
	}
	content := func(c Ordered[*MediaType]) {
		for _, key := range c.Keys {
			mt := c.Map[key]
			if mt != nil {
				walk(mt.Schema, 0)
			}
		}
	}

	op := ref.Operation
	params := op.Parameters
	if ref.Item != nil {
		params = append(append([]*Parameter{}, ref.Item.Parameters...), params...)
	}
	for _, p := range params {
		p = d.Parameter(p)
		if p != nil {
			walk(p.Schema, 0)
			content(p.Content)
		}
	}
	body := d.RequestBody(op.RequestBody)
	if body != nil {
		content(body.Content)
	}
	for _, status := range op.Responses.Keys {
		res := d.Response(op.Responses.Map[status])
		if res != nil {
			content(res.Content)
		}
	}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		walk(d.Components.Schemas.Map[name], 0)
	}
	return found
}

// tagGroups files models by tag: the head document's tags in their order,
// then any tag only removed models or x-tags name, then the Shared group.
// Inside a group the models stored in a table come first, then by display
// name. Removed models (diff mode) are filed by the base document.
func (b *builder) tagGroups(models []*modelView) []modelGroup {
	tagOf := b.d.modelTags()
	removed := map[string]bool{}
	var baseTags map[string]string
	if b.diff != nil {
		baseTags = b.diff.base.modelTags()
		for _, name := range b.diff.removedModels(b.d) {
			removed[name] = true
		}
	}

	names := b.d.tagNames()
	index := map[string]int{}
	for i, name := range names {
		index[name] = i
	}
	var groups []modelGroup
	byTag := map[string]int{}
	var shared []*modelView
	for _, m := range models {
		tag := tagOf[m.Name]
		if removed[m.Name] {
			tag = baseTags[m.Name]
		}
		if tag == "" {
			shared = append(shared, m)
			continue
		}
		i, ok := byTag[tag]
		if !ok {
			i = len(groups)
			byTag[tag] = i
			groups = append(groups, modelGroup{Name: tag})
		}
		groups[i].Models = append(groups[i].Models, m)
	}
	rank := func(tag string) int {
		i, ok := index[tag]
		if !ok {
			return len(names)
		}
		return i
	}
	sort.SliceStable(groups, func(i, j int) bool {
		return rank(groups[i].Name) < rank(groups[j].Name)
	})
	if len(shared) > 0 {
		groups = append(groups, modelGroup{Name: sharedGroup, Shared: true, Models: shared})
	}
	for i := range groups {
		list := groups[i].Models
		sort.SliceStable(list, func(i, j int) bool {
			if (list[i].Table != "") != (list[j].Table != "") {
				return list[i].Table != ""
			}
			return strings.ToLower(list[i].Short) < strings.ToLower(list[j].Short)
		})
		groups[i].Changes = groupChanges(list)
	}
	return groups
}
