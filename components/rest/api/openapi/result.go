package openapi

import (
	"reflect"

	"github.com/danielgtaylor/huma/v2"
)

// Result holds the finalized components. It is shared and must not be
// mutated: accessor methods return copies, and Components must be treated as
// read-only (copy before editing).
type Result struct {
	// Components keyed by name, ready to place under #/components/schemas.
	Components map[string]*huma.Schema

	bodies    map[reflect.Type]body
	responses map[reflect.Type]*huma.Schema
	strict    []string
}

type body struct {
	ref      string
	required bool
}

// BodyRef returns the $ref and whether the body is required (the schema has
// at least one required property) for a request root type. ok is false when
// the type has no body properties (all fields are params or hidden).
func (res *Result) BodyRef(t reflect.Type) (ref string, required bool, ok bool) {
	b, ok := res.bodies[deref(t)]
	return b.ref, b.required, ok
}

// ResponseSchema returns a copy of the schema for a response root type: a
// $ref for structs, an inline schema for slices and maps. nil when t was not
// registered.
func (res *Result) ResponseSchema(t reflect.Type) *huma.Schema {
	return copySchema(res.responses[deref(t)])
}

// StrictComponents returns a deep copy of Components with
// additionalProperties:false re-injected on every object component reachable
// from a response root (for contract tests).
func (res *Result) StrictComponents() map[string]*huma.Schema {
	out := make(map[string]*huma.Schema, len(res.Components))
	for name, s := range res.Components {
		out[name] = copySchema(s)
	}
	for _, name := range res.strict {
		out[name].AdditionalProperties = false
	}
	return out
}
