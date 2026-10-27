package reference

import (
	"net/url"
	"sort"
	"strings"
)

// Ref targets are local JSON pointers into components, the only form the
// generators we care about emit. Remote and relative refs stay unresolved and
// render as their raw string.

const (
	refSchemas       = "#/components/schemas/"
	refResponses     = "#/components/responses/"
	refParameters    = "#/components/parameters/"
	refRequestBodies = "#/components/requestBodies/"
	refExamples      = "#/components/examples/"
	refHeaders       = "#/components/headers/"
)

// RefName returns the component name a local ref points at, or "" when ref is
// empty or not of the given kind.
func RefName(ref, prefix string) string {
	if !strings.HasPrefix(ref, prefix) {
		return ""
	}
	return unescapePointer(strings.TrimPrefix(ref, prefix))
}

// unescapePointer decodes a JSON pointer token taken from a URI fragment:
// percent-encoding first (Foo%20Bar), then the ~1 and ~0 escapes.
func unescapePointer(s string) string {
	if strings.Contains(s, "%") {
		decoded, err := url.PathUnescape(s)
		if err == nil {
			s = decoded
		}
	}
	s = strings.ReplaceAll(s, "~1", "/")
	return strings.ReplaceAll(s, "~0", "~")
}

// Schema follows s through any chain of $ref and returns the target together
// with the name of the last component visited ("" for inline schemas). A ref
// that cannot be resolved returns s itself with the name it pointed at, so the
// renderer can still show something.
func (d *Document) Schema(s *Schema) (*Schema, string) {
	name := ""
	for hops := 0; s != nil && s.Ref != "" && hops < 32; hops++ {
		target := RefName(s.Ref, refSchemas)
		if target == "" {
			return s, s.Ref
		}
		next, ok := d.Components.Schemas.Get(target)
		if !ok || next == nil {
			return s, target
		}
		name = target
		s = next
	}
	return s, name
}

// SchemaName returns the component name s stands for: the name from Schema,
// or, when s is an inline wrapper whose only composition is a single-member
// allOf (the 3.0 idiom for adding description or nullable to a $ref), the
// name of that member. Cycle guards must use this name rather than the one
// from Schema, otherwise a model that refers to itself through such a wrapper
// is never recognised as recursive.
func (d *Document) SchemaName(s *Schema) string {
	for hops := 0; s != nil && hops < 32; hops++ {
		resolved, name := d.Schema(s)
		if name != "" {
			return name
		}
		if resolved == nil || len(resolved.AllOf) != 1 || resolved.Properties.Len() > 0 {
			return ""
		}
		s = resolved.AllOf[0]
	}
	return ""
}

// Parameter resolves a parameter ref.
func (d *Document) Parameter(p *Parameter) *Parameter {
	for hops := 0; p != nil && p.Ref != "" && hops < 32; hops++ {
		next, ok := d.Components.Parameters.Get(RefName(p.Ref, refParameters))
		if !ok || next == nil {
			return p
		}
		p = next
	}
	return p
}

// Header resolves a header ref.
func (d *Document) Header(h *Header) *Header {
	for hops := 0; h != nil && h.Ref != "" && hops < 32; hops++ {
		next, ok := d.Components.Headers.Get(RefName(h.Ref, refHeaders))
		if !ok || next == nil {
			return h
		}
		h = next
	}
	return h
}

// RequestBody resolves a request body ref.
func (d *Document) RequestBody(b *RequestBody) *RequestBody {
	for hops := 0; b != nil && b.Ref != "" && hops < 32; hops++ {
		next, ok := d.Components.RequestBodies.Get(RefName(b.Ref, refRequestBodies))
		if !ok || next == nil {
			return b
		}
		b = next
	}
	return b
}

// Response resolves a response ref.
func (d *Document) Response(r *Response) *Response {
	for hops := 0; r != nil && r.Ref != "" && hops < 32; hops++ {
		next, ok := d.Components.Responses.Get(RefName(r.Ref, refResponses))
		if !ok || next == nil {
			return r
		}
		r = next
	}
	return r
}

// Example resolves an example ref.
func (d *Document) Example(e *Example) *Example {
	for hops := 0; e != nil && e.Ref != "" && hops < 32; hops++ {
		next, ok := d.Components.Examples.Get(RefName(e.Ref, refExamples))
		if !ok || next == nil {
			return e
		}
		e = next
	}
	return e
}

// Flatten resolves s and merges its allOf members into a single schema, the
// way Redoc presents composed objects. The document is never mutated: a schema
// without allOf is returned as resolved, otherwise the result is a fresh
// shallow copy. Treat the result as read-only either way. Members are resolved
// and flattened recursively; a member seen earlier in the same chain is not
// expanded again, so cyclic allOf definitions terminate.
//
// Merge rules: properties are unioned in order, parent first then members in
// order, and a later definition of the same property replaces the earlier one
// while keeping its position. Required is unioned. Scalar keywords take the
// first non-empty value walking parent first then members in order; boolean
// flags (nullable, deprecated, readOnly, writeOnly, uniqueItems) are true when
// any of them sets it. oneOf, anyOf, discriminator and additionalProperties of
// members are kept only when the parent has none of its own.
//
// A member's oneOf, anyOf and discriminator are dropped when they point back
// at the schema being flattened or at any schema in the current allOf chain.
// That is the springdoc and NSwag polymorphism shape, Pet: {oneOf: [Cat, Dog],
// discriminator} with Cat: {allOf: [Pet, {...}]}: Cat is one concrete variant,
// not another "one of Cat or Dog".
func (d *Document) Flatten(s *Schema) *Schema {
	resolved, _ := d.Schema(s)
	if resolved == nil || len(resolved.AllOf) == 0 {
		return resolved
	}
	f := &flattener{doc: d, visiting: map[*Schema]bool{}, done: map[*Schema]flattened{}}
	out, _ := f.flatten(resolved)
	return out
}

// flattener flattens one schema. visiting holds the allOf chain being
// flattened. done keeps each schema with allOf flattened so far, so a member
// that several members share, as in a diamond (A: allOf [B, C] with B and C
// both allOf [D]), is flattened once rather than once per path.
//
// A result depends on the chain only through the schemas flattening looked
// up in it: the allOf members, for cycles, and the oneOf, anyOf and
// discriminator targets of members, see polymorphicTargets. Those are the result's
// deps, and a result is reused only while none of them is in the chain,
// since flattening again would then find the same and give the same result.
type flattener struct {
	doc      *Document
	visiting map[*Schema]bool
	done     map[*Schema]flattened
}

type flattened struct {
	out  *Schema
	deps map[*Schema]bool
}

// flatten returns the flattened s and its deps, nil for a schema without
// allOf, whose result never depends on the chain.
func (f *flattener) flatten(s *Schema) (*Schema, map[*Schema]bool) {
	resolved, _ := f.doc.Schema(s)
	if resolved == nil {
		return nil, nil
	}
	if len(resolved.AllOf) == 0 {
		return resolved, nil
	}
	if f.visiting[resolved] {
		return resolved, map[*Schema]bool{resolved: true}
	}
	memo, ok := f.done[resolved]
	if ok && !f.inChain(memo.deps) {
		return memo.out, memo.deps
	}
	f.visiting[resolved] = true

	out := *resolved
	out.AllOf = nil
	out.Properties = Ordered[*Schema]{}
	out.Required = nil
	out.Ref = ""

	for _, key := range resolved.Properties.Keys {
		out.Properties.Set(key, resolved.Properties.Map[key])
	}
	out.Required = append(out.Required, resolved.Required...)

	// The result depends on whether resolved itself is in the chain.
	deps := map[*Schema]bool{resolved: true}
	for _, member := range resolved.AllOf {
		m, memberDeps := f.flatten(member)
		for dep := range memberDeps {
			deps[dep] = true
		}
		if m == nil {
			continue
		}
		targets := f.doc.polymorphicTargets(m)
		for _, target := range targets {
			deps[target] = true
		}
		if f.anyInChain(targets) {
			concrete := *m
			concrete.OneOf = nil
			concrete.AnyOf = nil
			concrete.Discriminator = nil
			m = &concrete
		}
		mergeInto(&out, m)
	}

	delete(f.visiting, resolved)
	if !f.inChain(deps) {
		f.done[resolved] = flattened{out: &out, deps: deps}
	}
	return &out, deps
}

func (f *flattener) inChain(deps map[*Schema]bool) bool {
	for dep := range deps {
		if f.visiting[dep] {
			return true
		}
	}
	return false
}

func (f *flattener) anyInChain(schemas []*Schema) bool {
	for _, s := range schemas {
		if f.visiting[s] {
			return true
		}
	}
	return false
}

// polymorphicTargets returns the oneOf and anyOf members of m and its
// discriminator mapping targets, resolved. Flatten drops m's polymorphism
// when one of them is in the allOf chain.
func (d *Document) polymorphicTargets(m *Schema) []*Schema {
	var out []*Schema
	for _, list := range [][]*Schema{m.OneOf, m.AnyOf} {
		for _, member := range list {
			resolved, _ := d.Schema(member)
			if resolved != nil {
				out = append(out, resolved)
			}
		}
	}
	if m.Discriminator == nil {
		return out
	}
	for _, target := range m.Discriminator.Mapping {
		name := RefName(target, refSchemas)
		if name == "" {
			name = target
		}
		resolved, ok := d.Components.Schemas.Get(name)
		if ok && resolved != nil {
			out = append(out, resolved)
		}
	}
	return out
}

func mergeInto(out, m *Schema) {
	for _, key := range m.Properties.Keys {
		out.Properties.Set(key, m.Properties.Map[key])
	}
	for _, r := range m.Required {
		if !out.IsRequired(r) {
			out.Required = append(out.Required, r)
		}
	}

	if len(out.Type) == 0 {
		out.Type = m.Type
	}
	if out.Format == "" {
		out.Format = m.Format
	}
	if out.Title == "" {
		out.Title = m.Title
	}
	if out.Description == "" {
		out.Description = m.Description
	}
	if out.Items == nil {
		out.Items = m.Items
	}
	if len(out.Enum) == 0 {
		out.Enum = m.Enum
	}
	if out.Const == nil {
		out.Const = m.Const
	}
	if len(out.Examples) == 0 {
		out.Examples = m.Examples
	}
	if out.Default == nil {
		out.Default = m.Default
	}
	if out.Example == nil {
		out.Example = m.Example
	}
	if len(out.OneOf) == 0 {
		out.OneOf = m.OneOf
	}
	if len(out.AnyOf) == 0 {
		out.AnyOf = m.AnyOf
	}
	if out.Discriminator == nil {
		out.Discriminator = m.Discriminator
	}
	if out.AdditionalProperties == nil {
		out.AdditionalProperties = m.AdditionalProperties
	}
	if m.Nullable {
		out.Nullable = true
	}
	if m.NullMember {
		out.NullMember = true
	}
	if m.Deprecated {
		out.Deprecated = true
	}
	if m.ReadOnly {
		out.ReadOnly = true
	}
	if m.WriteOnly {
		out.WriteOnly = true
	}

	if out.Minimum == nil {
		out.Minimum = m.Minimum
	}
	if out.Maximum == nil {
		out.Maximum = m.Maximum
	}
	if out.ExclusiveMinimum == nil {
		out.ExclusiveMinimum = m.ExclusiveMinimum
	}
	if out.ExclusiveMaximum == nil {
		out.ExclusiveMaximum = m.ExclusiveMaximum
	}
	if out.MultipleOf == nil {
		out.MultipleOf = m.MultipleOf
	}
	if out.MinLength == nil {
		out.MinLength = m.MinLength
	}
	if out.MaxLength == nil {
		out.MaxLength = m.MaxLength
	}
	if out.Pattern == "" {
		out.Pattern = m.Pattern
	}
	if out.MinItems == nil {
		out.MinItems = m.MinItems
	}
	if out.MaxItems == nil {
		out.MaxItems = m.MaxItems
	}
	if out.MinProperties == nil {
		out.MinProperties = m.MinProperties
	}
	if out.MaxProperties == nil {
		out.MaxProperties = m.MaxProperties
	}
	if m.UniqueItems {
		out.UniqueItems = true
	}
}

// OperationSecurity returns the security requirements that apply to op: its
// own when set, otherwise the document's.
func (d *Document) OperationSecurity(op *Operation) []map[string][]string {
	if op.Security != nil {
		return *op.Security
	}
	return d.Security
}

// OperationRef is one operation of the document with the path it lives on.
type OperationRef struct {
	Method    string
	Path      string
	Item      *PathItem
	Operation *Operation
}

// Operations lists every operation in document path order, then in the
// canonical method order of PathItem.Methods.
func (d *Document) Operations() []OperationRef {
	var out []OperationRef
	for _, path := range d.Paths.Keys {
		item := d.Paths.Map[path]
		if item == nil {
			continue
		}
		for _, m := range item.Methods() {
			out = append(out, OperationRef{Method: m.Method, Path: path, Item: item, Operation: m.Operation})
		}
	}
	return out
}

// parameterOrder ranks the "in" locations the way references list them.
var parameterOrder = map[string]int{"path": 0, "query": 1, "header": 2, "cookie": 3}

// Parameters returns the parameters that apply to op: the path item's merged
// with the operation's, where an operation parameter replaces a path parameter
// of the same name and location in place. Refs are resolved. The result is
// grouped by location (path, query, header, cookie, then anything else) and
// keeps document order within each group. Either argument may be nil.
func (d *Document) Parameters(item *PathItem, op *Operation) []*Parameter {
	var all []*Parameter
	if item != nil {
		all = append(all, item.Parameters...)
	}
	if op != nil {
		all = append(all, op.Parameters...)
	}

	var out []*Parameter
	index := map[string]int{}
	for _, raw := range all {
		p := d.Parameter(raw)
		if p == nil {
			continue
		}
		key := p.In + "\x00" + p.Name
		if p.Ref != "" {
			key = "$ref\x00" + p.Ref
		}
		i, exists := index[key]
		if exists {
			out[i] = p
			continue
		}
		index[key] = len(out)
		out = append(out, p)
	}

	sort.SliceStable(out, func(i, j int) bool {
		return parameterRank(out[i].In) < parameterRank(out[j].In)
	})
	return out
}

func parameterRank(in string) int {
	rank, ok := parameterOrder[in]
	if !ok {
		return len(parameterOrder)
	}
	return rank
}
