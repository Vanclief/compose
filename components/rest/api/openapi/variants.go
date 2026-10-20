package openapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/vanclief/ez"
)

// Finalize runs the request-variant pass, clears additionalProperties and
// returns the result. Call once after all Add* calls; later calls return the
// same Result. Errors on duplicate component names.
func (r *Reflector) Finalize() (res *Result, err error) {
	if r.result != nil {
		return r.result, nil
	}

	defer recoverPanic(&err)

	base := map[string]*huma.Schema{}
	strict := map[string]bool{}
	for name, s := range r.reg.schemas {
		c := copySchema(s)
		walkSchema(c, func(n *huma.Schema) {
			normalizeNull(n)
			closed, isBool := n.AdditionalProperties.(bool)
			if isBool && !closed {
				n.AdditionalProperties = nil
				if n == c {
					strict[name] = true
				}
			}
		})
		base[name] = c
	}

	var reqRoots, respRoots []*huma.Schema
	for _, t := range r.reqOrder {
		reqRoots = append(reqRoots, &huma.Schema{Ref: RefPrefix + r.requests[t].name})
	}
	for _, t := range r.respOrder {
		walkSchema(r.responses[t], normalizeNull)
		respRoots = append(respRoots, r.responses[t])
	}
	reqReach := reachable(reqRoots, base)
	respReach := reachable(respRoots, base)

	// Local rewrite: presence-required and nullable pointers.
	local := map[string]*huma.Schema{}
	variant := map[string]bool{}
	for name := range reqReach {
		t := r.reg.types[name]
		if t == nil || t.Kind() != reflect.Struct {
			continue
		}
		local[name] = requestShape(base[name], t)
		variant[name] = !sameJSON(local[name], base[name])
	}

	// Fixpoint: a component referencing a variant needs a variant itself.
	for changed := true; changed; {
		changed = false
		for name := range local {
			if variant[name] {
				continue
			}
			for _, ref := range refsOf(base[name]) {
				if variant[ref] {
					variant[name] = true
					changed = true
					break
				}
			}
		}
	}

	variantName := func(name string) string {
		if respReach[name] {
			return name + "Input"
		}
		return name
	}

	out := map[string]*huma.Schema{}
	for name, s := range base {
		out[name] = s
	}
	for _, name := range sortedKeys(variant) {
		if !variant[name] {
			continue
		}
		s := local[name]
		walkSchema(s, func(n *huma.Schema) {
			ref := strings.TrimPrefix(n.Ref, RefPrefix)
			if n.Ref != "" && variant[ref] {
				n.Ref = RefPrefix + variantName(ref)
			}
		})

		vname := variantName(name)
		_, exists := base[vname]
		if vname != name && exists {
			return nil, ez.New(ez.EINVALID, fmt.Sprintf("openapi: duplicate component name %s (request variant of %s)", vname, name), nil)
		}
		out[vname] = s
	}

	res = &Result{
		bodies:    map[reflect.Type]body{},
		responses: map[reflect.Type]*huma.Schema{},
	}

	published := append([]*huma.Schema{}, respRoots...)
	for _, t := range r.reqOrder {
		root := r.requests[t]
		for _, p := range root.params {
			published = append(published, p.Schema)
		}

		name := root.name
		if variant[name] {
			name = variantName(name)
		}
		if len(propertyNames(out[name])) == 0 {
			continue
		}
		res.bodies[t] = body{ref: RefPrefix + name, required: len(out[name].Required) > 0}
		published = append(published, &huma.Schema{Ref: RefPrefix + name})
	}
	for _, t := range r.respOrder {
		res.responses[t] = r.responses[t]
	}

	keep := reachable(published, out)
	res.Components = map[string]*huma.Schema{}
	for name := range keep {
		res.Components[name] = out[name]
	}
	for name := range reachable(respRoots, out) {
		if strict[name] {
			res.strict = append(res.strict, name)
		}
	}
	sort.Strings(res.strict)

	r.result = res
	return res, nil
}

// requestShape copies base and rewrites it for the request direction:
// required becomes exactly the fields tagged required:"true", and pointer
// fields accept null unless required (present and non-null).
func requestShape(base *huma.Schema, t reflect.Type) *huma.Schema {
	s := copySchema(base)
	var required []string
	for _, f := range bodyFields(t) {
		name := jsonName(f)
		prop, ok := s.Properties[name]
		if !ok {
			continue
		}
		isRequired := f.Tag.Get("required") == "true"
		if isRequired {
			required = append(required, name)
		}
		if f.Type.Kind() != reflect.Pointer {
			continue
		}
		if isRequired {
			nonNull(prop)
			continue
		}
		nullable(prop)
	}
	s.Required = required
	return s
}

// normalizeNull fixes huma's nullable output in place: a nullable $ref
// (pointer to an enum, or a nullable tag on one) becomes anyOf [ref, null],
// and a nullable schema with an enum lists null among its values.
func normalizeNull(s *huma.Schema) {
	if !s.Nullable {
		return
	}
	if s.Ref != "" {
		wrapNull(s)
		return
	}
	addNullEnum(s)
}

// nullable makes s accept null in place. Scalars, arrays and maps use the
// 3.1 type array (null added to any enum); refs are wrapped as
// anyOf [ref, {type: null}].
func nullable(s *huma.Schema) {
	if s.Ref != "" {
		wrapNull(s)
		return
	}
	if s.Type != "" {
		s.Nullable = true
		addNullEnum(s)
	}
}

// nonNull undoes nullable in place.
func nonNull(s *huma.Schema) {
	if len(s.AnyOf) == 2 && s.Ref == "" && s.AnyOf[1].Type == "null" && s.AnyOf[0].Ref != "" {
		inner := s.AnyOf[0]
		s.AnyOf = nil
		s.Ref = inner.Ref
		s.Enum = inner.Enum
		s.Const = inner.Const
		return
	}
	s.Nullable = false
	var enum []any
	for _, v := range s.Enum {
		if v != nil {
			enum = append(enum, v)
		}
	}
	s.Enum = enum
}

// wrapNull rewrites a $ref schema in place as anyOf [{$ref, enum, const},
// {type: null}]. Annotations (description, default, deprecated, hidden) stay
// on the wrapper; enum and const move into the ref branch so null passes.
func wrapNull(s *huma.Schema) {
	inner := &huma.Schema{Ref: s.Ref, Enum: s.Enum, Const: s.Const}
	s.Ref = ""
	s.Enum = nil
	s.Const = nil
	s.Nullable = false
	s.AnyOf = []*huma.Schema{inner, {Type: "null"}}
}

func addNullEnum(s *huma.Schema) {
	if len(s.Enum) == 0 {
		return
	}
	for _, v := range s.Enum {
		if v == nil {
			return
		}
	}
	s.Enum = append(s.Enum, nil)
}

// bodyFields returns the exported fields of t that become JSON properties,
// shallowest declaration first wins, in declaration order.
func bodyFields(t reflect.Type) []reflect.StructField {
	var fields []reflect.StructField
	index := map[string]int{}
	for _, f := range reflect.VisibleFields(t) {
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" || (f.Anonymous && name == "") {
			continue
		}

		key := jsonName(f)
		i, ok := index[key]
		if ok {
			if len(f.Index) < len(fields[i].Index) {
				fields[i] = f
			}
			continue
		}
		index[key] = len(fields)
		fields = append(fields, f)
	}
	return fields
}

// reachable returns the component names reachable through $ref from roots.
func reachable(roots []*huma.Schema, comps map[string]*huma.Schema) map[string]bool {
	seen := map[string]bool{}
	var queue []string
	for _, root := range roots {
		queue = append(queue, refsOf(root)...)
	}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		s, ok := comps[name]
		if seen[name] || !ok {
			continue
		}
		seen[name] = true
		queue = append(queue, refsOf(s)...)
	}
	return seen
}

// refsOf returns the component names s references directly.
func refsOf(s *huma.Schema) []string {
	var refs []string
	walkSchema(s, func(n *huma.Schema) {
		if n.Ref != "" {
			refs = append(refs, strings.TrimPrefix(n.Ref, RefPrefix))
		}
	})
	return refs
}

// walkSchema calls fn for s and every inline subschema (not following refs).
func walkSchema(s *huma.Schema, fn func(*huma.Schema)) {
	if s == nil {
		return
	}
	fn(s)
	walkSchema(s.Items, fn)
	walkSchema(s.Not, fn)
	sub, ok := s.AdditionalProperties.(*huma.Schema)
	if ok {
		walkSchema(sub, fn)
	}
	for _, p := range s.Properties {
		walkSchema(p, fn)
	}
	for _, list := range [][]*huma.Schema{s.OneOf, s.AnyOf, s.AllOf} {
		for _, c := range list {
			walkSchema(c, fn)
		}
	}
}

// copySchema deep-copies the schema tree (not following refs).
func copySchema(s *huma.Schema) *huma.Schema {
	if s == nil {
		return nil
	}
	c := *s
	c.Items = copySchema(s.Items)
	c.Not = copySchema(s.Not)
	sub, ok := s.AdditionalProperties.(*huma.Schema)
	if ok {
		c.AdditionalProperties = copySchema(sub)
	}
	if s.Properties != nil {
		c.Properties = make(map[string]*huma.Schema, len(s.Properties))
		for k, v := range s.Properties {
			c.Properties[k] = copySchema(v)
		}
	}
	c.OneOf = copyList(s.OneOf)
	c.AnyOf = copyList(s.AnyOf)
	c.AllOf = copyList(s.AllOf)
	c.Required = append([]string(nil), s.Required...)
	c.Enum = append([]any(nil), s.Enum...)
	c.Examples = append([]any(nil), s.Examples...)
	return &c
}

func copyList(list []*huma.Schema) []*huma.Schema {
	if list == nil {
		return nil
	}
	out := make([]*huma.Schema, len(list))
	for i, s := range list {
		out[i] = copySchema(s)
	}
	return out
}

func sameJSON(a, b *huma.Schema) bool {
	ja, err := json.Marshal(a)
	if err != nil {
		return false
	}
	jb, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return bytes.Equal(ja, jb)
}

// propertyNames returns the published (non-hidden) property names of s,
// sorted. huma only drops hidden properties when marshalling.
func propertyNames(s *huma.Schema) []string {
	if s == nil || len(s.Properties) == 0 {
		return nil
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return nil
	}
	var decoded struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	err = json.Unmarshal(raw, &decoded)
	if err != nil {
		return nil
	}
	return sortedKeys(decoded.Properties)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
