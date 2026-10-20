package openapi

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"unicode"

	"github.com/danielgtaylor/huma/v2"
	"github.com/vanclief/ez"

	"github.com/vanclief/compose/primitives/enums"
)

var jsonUnmarshalerType = reflect.TypeFor[json.Unmarshaler]()

// jsonField is a struct field encoding/json decodes into, possibly promoted
// from an embedded struct.
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

// checkRequestKeys verifies, for every struct reachable from the request
// root t, that the keys encoding/json binds are exactly the properties of
// the schema huma produced, and that no field carries hidden:"true". Any
// difference would let a value the validator never saw reach the handler.
func (r *registry) checkRequestKeys(t reflect.Type) error {
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

		switch t.Kind() {
		case reflect.Slice, reflect.Array, reflect.Map:
			walk(t.Elem())
			return
		case reflect.Struct:
		default:
			return
		}
		_, aliased := r.aliases[t]
		_, isEnum := enums.Values(t)
		if aliased || isEnum || isOpaque(t) || t.Implements(jsonUnmarshalerType) || reflect.PointerTo(t).Implements(jsonUnmarshalerType) {
			return
		}

		hidden := map[string]bool{}
		hiddenFields(t, "", map[reflect.Type]bool{}, func(path string) {
			hidden[path] = true
			problems = append(problems, fmt.Sprintf("%s: field %s has hidden:\"true\", which is not allowed on request types (encoding/json still binds it)", t, path))
		})

		fields, candidates := jsonFields(t)
		name, ok := schemas[t]
		if ok && r.schemas[name].Type == "object" {
			problems = append(problems, compareKeys(t, fields, candidates, r.schemas[name], hidden)...)
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

// compareKeys reports keys bound by encoding/json but missing from schema,
// schema properties encoding/json would not bind, and JSON names declared by
// several fields where huma and encoding/json may pick different ones.
func compareKeys(t reflect.Type, fields []jsonField, candidates map[string][]jsonField, schema *huma.Schema, hidden map[string]bool) []string {
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
			problems = append(problems, fmt.Sprintf("%s: field %s (json %q) is bound by encoding/json but missing from the schema", t, f.path, f.name))
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
			problems = append(problems, fmt.Sprintf("%s: schema property %q is not bound by encoding/json (conflicting fields at the same depth)", t, name))
		}
	}
	return problems
}

func goName(path string) string {
	i := strings.LastIndex(path, ".")
	return path[i+1:]
}

// hiddenFields calls fn with the Go path of every field of t (including
// fields of embedded structs, exported or not) tagged hidden:"true".
func hiddenFields(t reflect.Type, prefix string, seen map[reflect.Type]bool, fn func(path string)) {
	if seen[t] {
		return
	}
	seen[t] = true
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		path := prefix + f.Name
		if f.Tag.Get("hidden") == "true" {
			fn(path)
		}
		ft := deref(f.Type)
		if f.Anonymous && ft.Kind() == reflect.Struct {
			hiddenFields(ft, path+".", seen, fn)
		}
	}
}
