package openapi

import (
	"encoding"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/vanclief/ez"

	"github.com/vanclief/compose/components/rest/api/internal/huma"
)

var (
	jsonUnmarshalerType = reflect.TypeFor[json.Unmarshaler]()
	jsonMarshalerType   = reflect.TypeFor[json.Marshaler]()
	textMarshalerType   = reflect.TypeFor[encoding.TextMarshaler]()
)

// jsonField is a struct field encoding/json decodes into and encodes from,
// possibly promoted from an embedded struct.
type jsonField struct {
	name   string       // JSON key
	tagged bool         // name came from the json tag
	index  []int        // field index path from the struct
	typ    reflect.Type // field type (unnamed pointer dereferenced)
	path   string       // Go path, e.g. base.Level
}

// jsonFields mirrors encoding/json's typeFields for t (a struct type). It
// returns the fields encoding/json binds (dominant fields, sorted by name)
// and every candidate before the dominance rules applied, grouped by name.
func jsonFields(t reflect.Type) (fields []jsonField, candidates map[string][]jsonField) {
	type level struct {
		typ   reflect.Type
		index []int
		path  string
	}

	var all []jsonField
	current := []level{}
	next := []level{{typ: t}}
	var count, nextCount map[reflect.Type]int
	visited := map[reflect.Type]bool{}

	for len(next) > 0 {
		current, next = next, current[:0]
		count, nextCount = nextCount, map[reflect.Type]int{}

		for _, f := range current {
			if visited[f.typ] {
				continue
			}
			visited[f.typ] = true

			for i := 0; i < f.typ.NumField(); i++ {
				sf := f.typ.Field(i)
				if sf.Anonymous {
					ft := sf.Type
					if ft.Kind() == reflect.Pointer {
						ft = ft.Elem()
					}
					if !sf.IsExported() && ft.Kind() != reflect.Struct {
						continue
					}
				} else if !sf.IsExported() {
					continue
				}

				tag := sf.Tag.Get("json")
				if tag == "-" {
					continue
				}
				name, _, _ := strings.Cut(tag, ",")
				name = jsonTagKey(name)

				index := make([]int, len(f.index)+1)
				copy(index, f.index)
				index[len(f.index)] = i
				path := sf.Name
				if f.path != "" {
					path = f.path + "." + sf.Name
				}

				ft := sf.Type
				if ft.Name() == "" && ft.Kind() == reflect.Pointer {
					ft = ft.Elem()
				}

				if name != "" || !sf.Anonymous || ft.Kind() != reflect.Struct {
					tagged := name != ""
					if name == "" {
						name = sf.Name
					}
					field := jsonField{name: name, tagged: tagged, index: index, typ: ft, path: path}
					all = append(all, field)
					if count[f.typ] > 1 {
						// The same embedded type at the same depth twice: add
						// a copy so the dominance rules annihilate it.
						all = append(all, field)
					}
					continue
				}

				nextCount[ft]++
				if nextCount[ft] == 1 {
					next = append(next, level{typ: ft, index: index, path: path})
				}
			}
		}
	}

	sort.SliceStable(all, func(i, j int) bool {
		x, y := all[i], all[j]
		if x.name != y.name {
			return x.name < y.name
		}
		if len(x.index) != len(y.index) {
			return len(x.index) < len(y.index)
		}
		if x.tagged != y.tagged {
			return x.tagged
		}
		return lessIndex(x.index, y.index)
	})

	candidates = map[string][]jsonField{}
	for _, f := range all {
		candidates[f.name] = append(candidates[f.name], f)
	}
	for _, name := range sortedKeys(candidates) {
		group := candidates[name]
		// Sorted by depth then tag: the first is dominant unless the second
		// ties on both.
		if len(group) > 1 && len(group[0].index) == len(group[1].index) && group[0].tagged == group[1].tagged {
			continue
		}
		fields = append(fields, group[0])
	}

	return fields, candidates
}

func lessIndex(a, b []int) bool {
	for i := range a {
		if i >= len(b) {
			return false
		}
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return len(a) < len(b)
}

// checkKeys verifies, for every struct reachable from the root t, that the
// keys encoding/json binds (request) or sends (response) are exactly the
// properties of the schema huma produced. A request field may not carry
// hidden:"true"; a response field may, its key sent but deliberately
// undocumented. Any other difference would let a value the validator never
// saw reach the handler, or send a key the document does not list. A
// response field may not send a null its schema does not allow (see
// sentNulls). A struct encoding/json refuses for the json tags of its
// fields, whatever the value, is refused (see refusedTags), as every request
// or response holding it would fail. Types encoding/json hands to their own
// decoder (request) are not looked into; a struct that encoding/json hands
// to its own encoder (response) is an error unless a JSONSchema method
// states its schema or a text decoder makes it a string (isOpaque). A struct
// documented by its fields that gets that decoder or encoder from an
// embedded field is an error too (see promotedCoder). A type with a
// JSONSchema method is not looked into either; it is an error unless it
// decodes (request) or encodes (response) its own JSON as its schema says,
// and a struct that gets the method or its coder from an embedded field is
// one (see providerProblems). The types its JSONSchema method asked refs.For
// for are linted and walked as fields are (see lintAsked).
func (r *registry) checkKeys(t reflect.Type, request bool) error {
	schemas := map[reflect.Type]string{}
	for name, typ := range r.types {
		schemas[typ] = name
	}

	var problems []string
	seen := map[reflect.Type]bool{}
	linted := map[reflect.Type]bool{}
	var walk func(t reflect.Type)
	walk = func(t reflect.Type) {
		t = deref(t)
		if seen[t] {
			return
		}
		seen[t] = true

		// Optional decodes its value with encoding/json: check T's keys.
		elem, ok := optionalElem(t)
		if ok {
			walk(elem)
			return
		}

		if providesSchema(t) {
			problems = append(problems, r.providerProblems(t, request)...)
			if request {
				problems = append(problems, r.notStructProblems(t)...)
			}
			for _, asked := range r.asks[t] {
				lintAsked(t, asked, request, linted, &problems)
				walk(asked)
			}
			return
		}

		switch t.Kind() {
		case reflect.Slice, reflect.Array, reflect.Map:
			walk(t.Elem())
			return
		case reflect.Struct:
		default:
			return
		}
		_, aliased := r.aliases[t]
		_, isEnum := enumValues(t)
		if aliased || isEnum || isOpaque(t) {
			return
		}
		embedded, ok := promotedCoder(t, request)
		if ok {
			problems = append(problems, promotedCoderProblem(t, embedded, request))
			return
		}
		if codesOwnJSON(t, request) {
			if !request {
				method := ownEncoder(t)
				if method == "" {
					method = ownEncoder(reflect.PointerTo(t))
				}
				problems = append(problems, fmt.Sprintf("%s writes its own JSON with %s, which the schema of its fields would not describe; add a JSONSchema method (api.SchemaProvider) that returns the schema of what %s writes", t, method, method))
			}
			return
		}
		// A response type that decodes itself is not asked (see
		// refusedTags).
		if !codesOwnJSON(t, true) {
			refusal := refusedTags(t)
			if refusal != "" {
				problems = append(problems, fmt.Sprintf("%s: encoding/json refuses the json tags of this type, whatever the value: %s; change the tag it names", t, refusal))
			}
		}

		hidden := map[string]bool{}
		hiddenFields(t, "", map[reflect.Type]bool{}, func(path string, f reflect.StructField) {
			hidden[path] = true
			if !request {
				return
			}
			isParam := slices.ContainsFunc(paramLocations, func(loc string) bool {
				_, ok := f.Tag.Lookup(loc)
				return ok
			})
			if isParam {
				problems = append(problems, fmt.Sprintf("%s: field %s has hidden:\"true\", which is not supported on a parameter (the document would hide a parameter the server still reads)", t, path))
				return
			}
			problems = append(problems, fmt.Sprintf("%s: field %s has hidden:\"true\", which is not allowed on request types (encoding/json still binds it)", t, path))
		})

		fields, candidates := jsonFields(t)
		name, ok := schemas[t]
		if ok && r.schemas[name].Type == "object" {
			problems = append(problems, compareKeys(t, fields, candidates, r.schemas[name], hidden, request)...)
			if !request {
				problems = append(problems, r.sentNulls(t, fields, r.schemas[name])...)
			}
		}
		if request {
			problems = append(problems, r.providedFieldProblems(t, fields)...)
		}

		for _, f := range fields {
			walk(f.typ)
		}
	}
	walk(t)

	if len(problems) == 0 {
		return nil
	}
	return ez.New(ez.EINVALID, "openapi: "+strings.Join(problems, "; "), nil)
}

// providedFieldProblems reports the fields of the request struct t that
// hold a type with a JSONSchema method, by value, behind a pointer or in a
// types.Optional, and cannot do what its text says: encoding/json calls no
// decoder for an absent key, so a default at the root of the text never
// reaches the handler, and none for null into a pointer, which it sets to
// nil, so a required:"true" pointer would be nil unless the text refuses
// null at its root (see refusesNull). The root is followed through its
// $refs (see rootChain).
func (r *registry) providedFieldProblems(t reflect.Type, fields []jsonField) []string {
	var problems []string
	for _, f := range fields {
		pt := deref(f.typ)
		elem, ok := optionalElem(pt)
		if ok {
			pt = deref(elem)
		}
		if !providesSchema(pt) {
			continue
		}
		chain := r.rootChain(r.schemas[r.names[pt]])
		defaulted := slices.ContainsFunc(chain, func(s *huma.Schema) bool {
			return s.Default != nil
		})
		if defaulted {
			problems = append(problems, fmt.Sprintf("%s.%s: the default at the root of the JSONSchema text of %s is not supported on a request field, the registry does not apply it and encoding/json calls no decoder for an absent key; set the default in the handler and describe it in the text's description", t, f.path, pt))
		}
		sf := t.FieldByIndex(f.index)
		if sf.Type.Kind() == reflect.Pointer && sf.Tag.Get("required") == "true" && !slices.ContainsFunc(chain, refusesNull) {
			problems = append(problems, fmt.Sprintf("%s.%s: required:\"true\" on a pointer to %s is not supported in a request type unless its JSONSchema text states, at its root, a type without null or an enum or const without null: encoding/json sets the pointer to nil for null without calling the decoder; state such a type in the text, declare the field by value, whose decoder reads the null, or drop required:\"true\"", t, f.path, pt))
		}
	}
	return problems
}

// rootChain returns s and the components its root $refs lead to, one after
// the other: every schema of the chain applies to the value at the root.
func (r *registry) rootChain(s *huma.Schema) []*huma.Schema {
	var chain []*huma.Schema
	for s != nil && !slices.Contains(chain, s) {
		chain = append(chain, s)
		if s.Ref == "" {
			break
		}
		s = r.SchemaFromRef(s.Ref)
	}
	return chain
}

// refusesNull reports whether s refuses null by a keyword of its own: a
// type without null, or an enum or const without null. Any other schema
// may let null through, as {}, a not, or an anyOf with a nullable member
// does, and is not looked into.
func refusesNull(s *huma.Schema) bool {
	if s.Type != "" && s.Type != "null" && !s.Nullable {
		return true
	}
	if len(s.Enum) > 0 && !slices.Contains(s.Enum, nil) {
		return true
	}
	return s.Const != nil
}

// notStructProblems reports, for t, a type with a JSONSchema method in a
// request, each struct documented by its fields that its text names with a
// $ref under a not (see structRefs): encoding/json binds that struct's keys
// ignoring case, and bodyCollisions, which refuses a key that differs from a
// property only in case, does not look under a not.
func (r *registry) notStructProblems(t reflect.Type) []string {
	var problems []string
	walkSchema(r.schemas[r.names[t]], func(n *huma.Schema) {
		if n.Not == nil {
			return
		}
		for _, st := range r.structRefs(n.Not, map[string]bool{}) {
			problems = append(problems, fmt.Sprintf("%s: its JSONSchema text refers to %s under a not, which is not supported in a request: encoding/json binds the keys of %s ignoring case, and the registry does not look under a not for a key that differs from a property only in case; leave %s out of the not", t, st, st, st))
		}
	})
	return problems
}

// structRefs returns the structs documented by their fields whose
// components s refers to, following the $refs of the text of a type with a
// JSONSchema method that s refers to.
func (r *registry) structRefs(s *huma.Schema, seen map[string]bool) []reflect.Type {
	var out []reflect.Type
	for _, name := range refsOf(s) {
		if seen[name] {
			continue
		}
		seen[name] = true
		t := r.types[name]
		switch {
		case t == nil:
		case providesSchema(t):
			out = append(out, r.structRefs(r.schemas[name], seen)...)
		case t.Kind() == reflect.Struct && !isOpaque(t):
			out = append(out, t)
		}
	}
	return out
}

// codesOwnJSON reports whether encoding/json hands t, or a pointer to it, to
// the type's own decoder (request: decodesOwnJSON; isOpaque covers
// encoding.TextUnmarshaler) or encoder (response: encodesOwnJSON) instead of
// reading or writing its fields.
func codesOwnJSON(t reflect.Type, request bool) bool {
	coder := encodesOwnJSON
	if request {
		coder = decodesOwnJSON
	}
	return coder(t) || coder(reflect.PointerTo(t))
}

// promotedCoder returns the name of the first embedded field of the struct t
// whose type codes its own JSON (codesOwnJSON) when t does too: t then has
// that decoder or encoder by promotion, and encoding/json hands it the whole
// value. reflect does not tell a method t declares from a promoted one, so a
// t that declares its own while also embedding such a field is reported too.
func promotedCoder(t reflect.Type, request bool) (string, bool) {
	if !codesOwnJSON(t, request) {
		return "", false
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Anonymous && codesOwnJSON(f.Type, request) {
			return f.Name, true
		}
	}
	return "", false
}

// promotedCoderProblem explains the promotedCoder finding of embedded in t.
func promotedCoderProblem(t reflect.Type, embedded string, request bool) string {
	coder := "decoder"
	if !request {
		coder = "encoder"
	}
	return fmt.Sprintf("%s: the JSON %s of embedded field %s would take over the whole value, not the fields the schema lists; make it a named field", t, coder, embedded)
}

// promotedSchema returns the name of the first embedded field of the struct
// t that has a JSONSchema method (providesSchema): t then has that method by
// promotion, and the registry would publish the whole value by it. As with
// promotedCoder, a t that declares its own JSONSchema method while also
// embedding such a field is reported too.
func promotedSchema(t reflect.Type) (string, bool) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Anonymous && providesSchema(f.Type) {
			return f.Name, true
		}
	}
	return "", false
}

// promotedSchemaProblem explains the promotedSchema finding of embedded in t.
func promotedSchemaProblem(t reflect.Type, embedded string) string {
	return fmt.Sprintf("%s: the JSONSchema method of embedded field %s would state the schema of the whole value; make it a named field", t, embedded)
}

// providerProblems reports why the registry cannot publish t, a type with a
// JSONSchema method, by the schema the method states: t is a struct that
// gets its JSON coder (see promotedCoder) or its JSONSchema method from an
// embedded field; encoding/json reads (request) or writes (response) t by
// its Go type, which the schema does not describe, as t has no JSON decoder
// or encoder of its own; or it codes t as text, always a JSON string, and
// the schema is not of type string or lets null through.
func (r *registry) providerProblems(t reflect.Type, request bool) []string {
	if t.Kind() == reflect.Struct {
		embedded, ok := promotedCoder(t, request)
		if ok {
			coder := "decoder"
			if !request {
				coder = "encoder"
			}
			return []string{fmt.Sprintf("%s: the JSON %s of embedded field %s would code the whole value as that field's type, not as the JSONSchema text of %s says; make it a named field", t, coder, embedded, t)}
		}
		embedded, ok = promotedSchema(t)
		if ok {
			return []string{promotedSchemaProblem(t, embedded)}
		}
	}

	ptr := reflect.PointerTo(t)
	method := ""
	switch {
	case request && !decodesOwnText(t):
		return []string{fmt.Sprintf("%s has a JSONSchema method but no JSON decoder of its own: encoding/json reads it by its Go type, which the JSONSchema method does not change; add an UnmarshalJSON that reads what the schema describes, or remove the JSONSchema method", t)}
	case !request && !codesOwnJSON(t, false):
		return []string{fmt.Sprintf("%s has a JSONSchema method but no JSON encoder of its own: encoding/json writes it by its Go type, which the JSONSchema method does not change; add a MarshalJSON that writes what the schema describes, or remove the JSONSchema method", t)}
	case request && !decodesOwnJSON(ptr):
		method = "UnmarshalText"
	case !request && (ownEncoder(ptr) == "MarshalText" || ownEncoder(ptr) == "AppendText"):
		method = ownEncoder(ptr)
	default:
		return nil
	}
	s := r.schemas[r.names[t]]
	if s.Type != huma.TypeString {
		return []string{fmt.Sprintf("%s codes its JSON as text with %s, which is always a JSON string, and its JSONSchema text does not say so; give it \"type\": \"string\"", t, method)}
	}
	if s.Nullable {
		return []string{fmt.Sprintf("%s codes its JSON as text with %s, which is always a JSON string, and its JSONSchema text lets null through too; give it \"type\": \"string\", without null", t, method)}
	}
	return nil
}

// compareKeys reports keys bound (request) or sent (response) by
// encoding/json but missing from schema, a json tag name encoding/json does
// not take as written, which huma lists as written, schema properties
// encoding/json would not bind or send, and JSON names declared by several
// fields where huma and encoding/json may pick different ones.
func compareKeys(t reflect.Type, fields []jsonField, candidates map[string][]jsonField, schema *huma.Schema, hidden map[string]bool, request bool) []string {
	missing := "is bound by encoding/json but missing from the schema"
	unused := "is not bound by encoding/json"
	if !request {
		missing = "is sent by encoding/json but missing from the schema; export the embedded struct, or declare the field on the type"
		unused = "is not sent by encoding/json"
	}

	var problems []string
	props := map[string]bool{}
	for _, name := range propertyNames(schema) {
		props[name] = true
	}

	bound := map[string]bool{}
	for _, f := range fields {
		bound[f.name] = true
		if hidden[f.path] {
			continue
		}
		// The schema names a property by its json tag's name as written;
		// encoding/json names the key by its own rules (jsonTagKey).
		written, _, _ := strings.Cut(t.FieldByIndex(f.index).Tag.Get("json"), ",")
		if written != "" && jsonTagKey(written) != written {
			bound[written] = true
			problems = append(problems, fmt.Sprintf("%s.%s: encoding/json does not take the json tag name %q as it is written and uses the key %q, which the schema would not list; name the key with letters, digits, spaces and ASCII punctuation other than a backslash, a quote, a backtick or a comma", t, f.path, written, f.name))
			continue
		}
		if !props[f.name] {
			problems = append(problems, fmt.Sprintf("%s: field %s (json %q) %s", t, f.path, f.name, missing))
			continue
		}

		// huma lists direct fields first and keeps the first field per Go
		// name, but the last per JSON name. Only a direct field shadowing
		// embedded fields of the same Go name resolves identically.
		group := candidates[f.name]
		if len(group) < 2 {
			continue
		}
		direct := len(f.index) == 1
		var others []string
		for _, c := range group[1:] {
			if !direct || goName(c.path) != goName(f.path) {
				others = append(others, c.path)
			}
		}
		if len(others) > 0 {
			problems = append(problems, fmt.Sprintf("%s: json %q is declared by %s and %s; rename one (only a direct field may shadow embedded fields of the same Go name)", t, f.name, f.path, strings.Join(others, ", ")))
		}
	}

	for _, name := range sortedKeys(props) {
		if !bound[name] {
			problems = append(problems, fmt.Sprintf("%s: schema property %q %s (conflicting fields at the same depth)", t, name, unused))
		}
	}
	return problems
}

// sentNulls reports the pointer fields of the response struct t without
// omitempty or omitzero, which encoding/json sends as null when nil, whose
// property schema does not allow null: a strict client would fail to parse
// the response.
func (r *registry) sentNulls(t reflect.Type, fields []jsonField, schema *huma.Schema) []string {
	var problems []string
	for _, f := range fields {
		sf := t.FieldByIndex(f.index)
		if sf.Type.Kind() != reflect.Pointer {
			continue
		}
		_, options, _ := strings.Cut(sf.Tag.Get("json"), ",")
		opts := strings.Split(options, ",")
		if slices.Contains(opts, "omitempty") || slices.Contains(opts, "omitzero") {
			continue
		}
		prop, ok := schema.Properties[f.name]
		if !ok || r.allowsNull(prop) {
			continue
		}
		problems = append(problems, fmt.Sprintf("%s: field %s (json %q) is sent as null when nil, which its schema does not allow; add omitempty to its json tag (the key is then absent rather than null), or declare the field by value instead of as a pointer", t, f.path, f.name))
	}
	return problems
}

// allowsNull reports whether s accepts null: it is nullable, refers to a
// schema that is, offers a branch that is, or has no type at all
// (json.RawMessage, an interface).
func (r *registry) allowsNull(s *huma.Schema) bool {
	if s.Nullable || s.Type == "null" {
		return true
	}
	if s.Ref != "" {
		target := r.SchemaFromRef(s.Ref)
		return target != nil && r.allowsNull(target)
	}
	if len(s.AnyOf) > 0 || len(s.OneOf) > 0 {
		return slices.ContainsFunc(s.AnyOf, r.allowsNull) || slices.ContainsFunc(s.OneOf, r.allowsNull)
	}
	return s.Type == "" && len(s.AllOf) == 0
}

func goName(path string) string {
	i := strings.LastIndex(path, ".")
	return path[i+1:]
}

// hiddenFields calls fn with the Go path and the field of every field of t
// (including fields of embedded structs, exported or not) tagged
// hidden:"true".
func hiddenFields(t reflect.Type, prefix string, seen map[reflect.Type]bool, fn func(path string, f reflect.StructField)) {
	if seen[t] {
		return
	}
	seen[t] = true
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		path := prefix + f.Name
		if f.Tag.Get("hidden") == "true" {
			fn(path, f)
		}
		ft := deref(f.Type)
		if f.Anonymous && ft.Kind() == reflect.Struct {
			hiddenFields(ft, path+".", seen, fn)
		}
	}
}
