package apidocs

import "strings"

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

func unescapePointer(s string) string {
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
// way Redoc presents composed objects. The result is a fresh copy; the
// document is never mutated. Members are resolved and flattened recursively;
// a member seen earlier in the same chain is skipped so cyclic allOf
// definitions terminate.
//
// Merge rules: properties are unioned in order (later members override a
// property of the same name), required is unioned, scalar keywords take the
// first non-empty value walking parent first then members in order, and
// oneOf/anyOf of members are kept only when the parent has none of its own.
func (d *Document) Flatten(s *Schema) *Schema {
	return d.flatten(s, map[*Schema]bool{})
}

func (d *Document) flatten(s *Schema, visiting map[*Schema]bool) *Schema {
	resolved, _ := d.Schema(s)
	if resolved == nil {
		return nil
	}
	if len(resolved.AllOf) == 0 {
		return resolved
	}
	if visiting[resolved] {
		return resolved
	}
	visiting[resolved] = true
	defer delete(visiting, resolved)

	out := *resolved
	out.AllOf = nil
	out.Properties = Ordered[*Schema]{}
	out.Required = nil
	out.Ref = ""

	for _, key := range resolved.Properties.Keys {
		out.Properties.Set(key, resolved.Properties.Map[key])
	}
	out.Required = append(out.Required, resolved.Required...)

	for _, member := range resolved.AllOf {
		m := d.flatten(member, visiting)
		if m == nil {
			continue
		}
		mergeInto(&out, m)
	}

	return &out
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
