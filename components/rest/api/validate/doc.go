// Package validate compiles OpenAPI 3.1 schemas with santhosh-tekuri/jsonschema
// and validates decoded requests against them.
//
// A Compiler holds the full OpenAPI document as one resource. Each operation
// compiles a synthetic schema of the form
//
//	{"type":"object","properties":{"path":{...},"query":{...},"header":{...},
//	 "body":{"$ref":"spec.json#/components/schemas/X"}},"required":[...]}
//
// whose $refs point into the document with the "spec.json#/..." prefix.
//
// The Compiler is not safe for concurrent use: compile every operation from
// a single goroutine, as the api registry does in Ready. A compiled Schema is immutable and safe
// to use from any number of goroutines.
//
// Validation errors become a single ez.EINVALID error whose message lists the
// failing fields, the first MaxFields of them. Messages never contain the
// submitted value. Fields returns the same list in structured form.
package validate
