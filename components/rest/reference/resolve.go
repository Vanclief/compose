package reference

import (
	"net/url"
	"reflect"
	"sort"
	"strings"
	"sync"
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
// without allOf is returned as resolved, unless it holds its lower or upper
// bound in two keywords, see below, and otherwise the result is a fresh
// shallow copy. Treat the result as read-only either way. Members are resolved
// and flattened recursively; a member seen earlier in the same chain is not
// expanded again, so cyclic allOf definitions terminate. A $ref is followed
// and the keywords beside it are dropped.
//
// Merge rules: properties are unioned in order, parent first then members in
// order. Required is unioned. Every member applies, so of the numeric bounds
// the tightest is kept: of minLength, maxLength, minItems, maxItems,
// minProperties and maxProperties each, and of the lower bound minimum and
// exclusiveMinimum (and the upper bound maximum and exclusiveMaximum)
// together, kept in the one keyword that holds it, the exclusive one at
// equal values, in its numeric form. Of the types, those every member that
// states one allows are kept, an integer counting as a number, so number
// and integer give integer; where they share none, the first stated is
// kept, as no value is accepted either way. A property defined more than
// once, items given more than once and two schemas of additional
// properties are each merged into an allOf of the definitions, which
// flattens by these same rules: the later definition of a property first,
// so where a keyword takes one value it is the later one's, and the first
// items or additionalProperties schema first. additionalProperties false,
// or a schema that allows no value, wins over any other, and true or none
// gives way to a schema. Other scalar keywords take the first non-empty
// value walking parent first then members in order; boolean flags
// (nullable, deprecated, readOnly, writeOnly, uniqueItems) are true when any
// of them sets it. oneOf, anyOf and discriminator of members are kept only
// when the parent has none of its own.
//
// A member's oneOf, anyOf and discriminator are dropped when they point back
// at the schema being flattened or at any schema in the current allOf chain.
// That is the springdoc and NSwag polymorphism shape, Pet: {oneOf: [Cat, Dog],
// discriminator} with Cat: {allOf: [Pet, {...}]}: Cat is one concrete variant,
// not another "one of Cat or Dog".
func (d *Document) Flatten(s *Schema) *Schema {
	return flattenSchema(d, s, false)
}

// flattenSchema is Flatten. With siblings set, a $ref with other keywords
// beside it, at the top, as an allOf member at any depth or as a link in a
// chain of references, flattens as an allOf of the bare reference with those
// keywords on the parent, so they apply together with the model it names, as
// OpenAPI 3.1 says; Flatten follows the reference and drops them. Also with
// siblings set, an allOf of two or more members is nullable from its members
// only when every member admits null, see admitsNull, since null must pass
// each of them; its own nullable holds as written, and the only member of a
// single-member allOf passes its nullable on, as in the 3.0 spelling of a
// nullable reference. A not is kept too, the first one found, parent first
// then members in order, where Flatten drops the not of a member.
func flattenSchema(d *Document, s *Schema, siblings bool) *Schema {
	f := &flattener{doc: d, siblings: siblings}
	_, src := f.resolve(s)
	if src == nil {
		return nil
	}
	if len(src.AllOf) == 0 {
		return oneBoundForm(src)
	}
	f.visiting = map[*Schema]bool{}
	f.done = map[*Schema]flattened{}
	out, _ := f.flatten(s)
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
	siblings bool
}

type flattened struct {
	out  *Schema
	deps map[*Schema]bool
}

// flatten returns the flattened s and its deps, nil for a schema without
// allOf, whose result never depends on the chain.
func (f *flattener) flatten(s *Schema) (*Schema, map[*Schema]bool) {
	key, src := f.resolve(s)
	if src == nil {
		return nil, nil
	}
	if len(src.AllOf) == 0 {
		return src, nil
	}
	if f.visiting[key] {
		return key, map[*Schema]bool{key: true}
	}
	memo, ok := f.done[key]
	if ok && !f.inChain(memo.deps) {
		return memo.out, memo.deps
	}
	f.visiting[key] = true

	out := *src
	out.AllOf = nil
	out.Properties = Ordered[*Schema]{}
	out.Required = nil
	out.Ref = ""

	for _, name := range src.Properties.Keys {
		out.Properties.Set(name, src.Properties.Map[name])
	}
	out.Required = append(out.Required, src.Required...)

	// The result depends on whether key itself is in the chain.
	deps := map[*Schema]bool{key: true}
	allNull := true
	for _, member := range src.AllOf {
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
		allNull = allNull && admitsNull(m)
		f.doc.mergeInto(&out, m)
		if f.siblings && out.Not == nil {
			out.Not = m.Not
		}
	}
	if f.siblings && len(src.AllOf) > 1 && !allNull {
		out.Nullable = src.Nullable
		out.NullMember = src.NullMember
	}

	delete(f.visiting, key)
	if !f.inChain(deps) {
		f.done[key] = flattened{out: &out, deps: deps}
	}
	return &out, deps
}

// resolve follows s through its $ref chain and returns the schema reached,
// which the chain and the results are kept by, and the schema to flatten.
// They are the same schema except with siblings set, where the chain stops
// at the first schema with keywords beside its $ref and the schema to
// flatten is that one as an allOf of its bare reference with the keywords
// on the parent. The bare reference resolves to the next schema in the
// chain, so every keyword along it applies, and a chain that comes back to
// a schema it passed stops there as any allOf cycle does.
func (f *flattener) resolve(s *Schema) (*Schema, *Schema) {
	if !f.siblings {
		resolved, _ := f.doc.Schema(s)
		return resolved, resolved
	}
	for hops := 0; s != nil && s.Ref != "" && hops < 32; hops++ {
		if hasSiblings(s) {
			return s, refAllOf(s)
		}
		target := RefName(s.Ref, refSchemas)
		next, ok := f.doc.Components.Schemas.Get(target)
		if target == "" || !ok || next == nil {
			return s, s
		}
		s = next
	}
	return s, s
}

// hasSiblings reports whether s has keywords beside its $ref. Extensions
// are not keywords and are left out of the test.
func hasSiblings(s *Schema) bool {
	return len(s.AllOf) > 0 || hasKeywords(s)
}

// keywordFields holds the index of each field of Schema that is a keyword
// other than $ref and allOf, so every field but Ref, AllOf and Extensions.
var keywordFields = func() []int {
	var out []int
	t := reflect.TypeOf(Schema{})
	for i := range t.NumField() {
		switch t.Field(i).Name {
		case "Ref", "AllOf", "Extensions":
			continue
		}
		out = append(out, i)
	}
	return out
}()

// hasKeywords reports whether s sets a keyword other than $ref and allOf.
func hasKeywords(s *Schema) bool {
	v := reflect.ValueOf(s).Elem()
	for _, i := range keywordFields {
		if !v.Field(i).IsZero() {
			return true
		}
	}
	return false
}

// refAllOf is s, a $ref with keywords beside it, as an allOf of the bare
// reference, first, and the members s already has, with the keywords on the
// parent.
func refAllOf(s *Schema) *Schema {
	out := *s
	out.Ref = ""
	out.AllOf = append([]*Schema{{Ref: s.Ref}}, s.AllOf...)
	return &out
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

// mergeInto merges the flattened member m into out by the rules Flatten
// describes.
func (d *Document) mergeInto(out, m *Schema) {
	for _, key := range m.Properties.Keys {
		earlier, ok := out.Properties.Get(key)
		if !ok {
			out.Properties.Set(key, m.Properties.Map[key])
			continue
		}
		out.Properties.Set(key, d.mergedSchema(m.Properties.Map[key], earlier))
	}
	for _, r := range m.Required {
		if !out.IsRequired(r) {
			out.Required = append(out.Required, r)
		}
	}

	out.Type = commonTypes(out.Type, m.Type)
	if out.Format == "" {
		out.Format = m.Format
	}
	if out.Title == "" {
		out.Title = m.Title
	}
	if out.Description == "" {
		out.Description = m.Description
	}
	out.Items = d.mergedSchema(out.Items, m.Items)
	if len(out.Enum) == 0 {
		out.Enum = m.Enum
	}
	if out.Const == nil && !out.ConstNull {
		out.Const = m.Const
		out.ConstNull = m.ConstNull
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
	out.AdditionalProperties = d.mergedAdditional(out.AdditionalProperties, m.AdditionalProperties)
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

	// Every member applies, so the bound that holds is the tightest one,
	// whichever keyword sets it. It is kept in one keyword, so a reader that
	// looks at one of the two first cannot see a looser one.
	out.Minimum, out.ExclusiveMinimum = boundKeywords(tighterBound(lowerBound(out), lowerBound(m), true))
	out.Maximum, out.ExclusiveMaximum = boundKeywords(tighterBound(upperBound(out), upperBound(m), false))
	out.MinLength = tightest(out.MinLength, m.MinLength, true)
	out.MaxLength = tightest(out.MaxLength, m.MaxLength, false)
	out.MinItems = tightest(out.MinItems, m.MinItems, true)
	out.MaxItems = tightest(out.MaxItems, m.MaxItems, false)
	out.MinProperties = tightest(out.MinProperties, m.MinProperties, true)
	out.MaxProperties = tightest(out.MaxProperties, m.MaxProperties, false)
	if out.MultipleOf == nil {
		out.MultipleOf = m.MultipleOf
	}
	if out.Pattern == "" {
		out.Pattern = m.Pattern
	}
	if m.UniqueItems {
		out.UniqueItems = true
	}
}

// commonTypes returns the types both a and b allow, in the order of a, an
// integer counting as a number: either list where the other is empty, and a
// where they share none, as no value is then accepted whichever is kept.
func commonTypes(a, b TypeList) TypeList {
	if len(a) == 0 {
		return b
	}
	if len(b) == 0 {
		return a
	}
	var out TypeList
	for _, t := range a {
		both := t
		switch {
		case b.Has(t):
		case t == "number" && b.Has("integer"):
			both = "integer"
		case t == "integer" && b.Has("number"):
		default:
			continue
		}
		if !out.Has(both) {
			out = append(out, both)
		}
	}
	if len(out) == 0 {
		return a
	}
	return out
}

// mergedMu guards the merged field of every document, see mergedSchema.
var mergedMu sync.Mutex

// mergedSchema returns a schema that applies both a and b, either of which
// may be nil: one of them where the other adds nothing to it, as the empty
// schema, a bare $ref to the model the other refers to, or a schema written
// the same does, or where one allows no value, otherwise an allOf of a and
// then b, which
// flattens by the rules of Flatten, so where a keyword takes one value it
// is a's. The allOf is made once per pair and document, so flattening a
// schema again gives the same schemas, and a cycle guard keyed by schema
// sees a schema met again through a merged definition as the cycle it is.
func (d *Document) mergedSchema(a, b *Schema) *Schema {
	switch {
	case b == nil || a == b || emptySchema(b) || refusesAll(a) || bareRefTo(b, a) || reflect.DeepEqual(a, b):
		return a
	case a == nil || emptySchema(a) || refusesAll(b) || bareRefTo(a, b):
		return b
	}
	key := [2]*Schema{a, b}
	mergedMu.Lock()
	defer mergedMu.Unlock()
	out, ok := d.merged[key]
	if ok {
		return out
	}
	if d.merged == nil {
		d.merged = map[[2]*Schema]*Schema{}
	}
	out = &Schema{AllOf: []*Schema{a, b}}
	d.merged[key] = out
	return out
}

// emptySchema reports whether s is {}, which allows any value and says
// nothing about it.
func emptySchema(s *Schema) bool {
	return s.Ref == "" && len(s.AllOf) == 0 && len(s.Extensions) == 0 && !hasKeywords(s)
}

// bareRefTo reports whether s is a $ref with nothing beside it to the model
// other refers to in the shape modelRef accepts, so other already applies
// all of s.
func bareRefTo(s, other *Schema) bool {
	return s.Ref != "" && !hasSiblings(s) && modelRef(other) == s.Ref
}

// mergedAdditional returns the additionalProperties that apply where both a
// and b do, either of which may be nil, standing for the keyword absent:
// one that allows no other property wins, true or none gives way to the
// other, and two schemas merge as mergedSchema says.
func (d *Document) mergedAdditional(a, b *AdditionalProperties) *AdditionalProperties {
	switch {
	case b == nil || refusesExtra(a):
		return a
	case a == nil || refusesExtra(b):
		return b
	case b.Schema == nil:
		return a
	case a.Schema == nil:
		return b
	}
	merged := d.mergedSchema(a.Schema, b.Schema)
	if merged == a.Schema {
		return a
	}
	if merged == b.Schema {
		return b
	}
	return &AdditionalProperties{Allowed: true, Schema: merged}
}

// refusesExtra reports whether a, an additionalProperties keyword or nil,
// refuses every property the object does not list: false, or a schema that
// allows no value.
func refusesExtra(a *AdditionalProperties) bool {
	return a != nil && (!a.Allowed || refusesAll(a.Schema))
}

// tightest returns the tighter of two limits, either of which may be unset:
// the larger of two lower limits (lower set) or the smaller of two upper
// limits.
func tightest[T int | float64](a, b *T, lower bool) *T {
	if a == nil {
		return b
	}
	if b == nil || *a == *b || (*b > *a) != lower {
		return a
	}
	return b
}

// boundKeywords writes an effective bound as the keywords that hold it: an
// inclusive bound as minimum or maximum, an exclusive one as the numeric
// exclusiveMinimum or exclusiveMaximum, the other nil.
func boundKeywords(nb *numBound) (*float64, *Bound) {
	if nb == nil {
		return nil, nil
	}
	v := nb.value
	if nb.exclusive {
		return nil, &Bound{Value: &v}
	}
	return &v, nil
}

// oneBoundForm returns s, or, when s holds its lower or upper bound in two
// keywords, such as minimum beside exclusiveMinimum, or in the 3.0 boolean
// form a document built without Parse can hold, a copy of s with each bound
// in the one keyword mergeInto would leave it in.
func oneBoundForm(s *Schema) *Schema {
	twoForms := func(limit *float64, excl *Bound) bool {
		return excl != nil && (limit != nil || excl.Value == nil)
	}
	if !twoForms(s.Minimum, s.ExclusiveMinimum) && !twoForms(s.Maximum, s.ExclusiveMaximum) {
		return s
	}
	out := *s
	out.Minimum, out.ExclusiveMinimum = boundKeywords(lowerBound(s))
	out.Maximum, out.ExclusiveMaximum = boundKeywords(upperBound(s))
	return &out
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
