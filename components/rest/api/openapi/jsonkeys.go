package openapi

import (
	"encoding"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"unicode"

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
				if !validJSONTag(name) {
					name = ""
				}

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

// validJSONTag mirrors encoding/json's isValidTag.
func validJSONTag(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		switch {
		case strings.ContainsRune("!#$%&()*+-./:;<=>?@[]^_{|}~ ", c):
		case !unicode.IsLetter(c) && !unicode.IsDigit(c):
			return false
		}
	}
	return true
}

// checkKeys verifies, for every struct reachable from the root t, that the
// keys encoding/json binds (request) or sends (response) are exactly the
// properties of the schema huma produced. A request field may not carry
// hidden:"true"; a response field may, its key sent but deliberately
// undocumented. Any other difference would let a value the validator never
// saw reach the handler, or send a key the document does not list. A
// response field may not send a null its schema does not allow (see
// sentNulls). Types encoding/json hands to their own decoder (request) or
// encoder (response) are not looked into, unless a struct documented by its
// fields gets that decoder or encoder from an embedded field (see
// promotedCoder), which is an error.
func (r *registry) checkKeys(t reflect.Type, request bool) error {
	schemas := map[reflect.Type]string{}
	for name, typ := range r.types {
		schemas[typ] = name
	}

	var problems []string
	seen := map[reflect.Type]bool{}
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
			coder := "decoder"
			if !request {
				coder = "encoder"
			}
			problems = append(problems, fmt.Sprintf("%s: the JSON %s of embedded field %s would take over the whole value, not the fields the schema lists; make it a named field", t, coder, embedded))
			return
		}
		if codesOwnJSON(t, request) {
			return
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

// codesOwnJSON reports whether encoding/json hands t, or a pointer to it, to
// the type's own decoder (request: json.Unmarshaler; isOpaque covers
// encoding.TextUnmarshaler) or encoder (response: json.Marshaler or
// encoding.TextMarshaler) instead of reading or writing its fields.
func codesOwnJSON(t reflect.Type, request bool) bool {
	coders := []reflect.Type{jsonMarshalerType, textMarshalerType}
	if request {
		coders = []reflect.Type{jsonUnmarshalerType}
	}
	ptr := reflect.PointerTo(t)
	return slices.ContainsFunc(coders, func(coder reflect.Type) bool {
		return t.Implements(coder) || ptr.Implements(coder)
	})
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

// compareKeys reports keys bound (request) or sent (response) by
// encoding/json but missing from schema, schema properties encoding/json
// would not bind or send, and JSON names declared by several fields where
// huma and encoding/json may pick different ones.
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
