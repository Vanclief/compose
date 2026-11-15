// Package spectest checks an OpenAPI document made by components/rest/generator
// against the running application, in the app's own tests. The generator reads
// the app's source and predicts its routes and the JSON its types marshal to; a
// prediction it gets wrong makes a wrong document that nothing else notices.
// Apps on the route registry (components/rest/api) have apitest instead.
//
// Routes compares the routes Echo registered with the paths and methods of the
// document. It catches a route the generator never saw and an operation the
// document holds that Echo does not serve. It cannot tell whether a documented
// operation's parameters, bodies or responses are right, and it does not
// follow a path item that is a $ref, so the operations behind one count as
// missing from the document.
//
// Schema marshals a value with encoding/json, or takes JSON as recorded, and
// validates it against a component schema of the document as a response. It
// catches a field the document names otherwise than encoding/json writes it,
// a field it calls required that encoding/json leaves out (omitempty,
// omitzero), a nil slice, map or pointer written as null where the document
// says the value is never null, a key the document does not declare, as an
// object that declares its properties is checked as closed where it is
// reached from the component through properties, items, map values, allOf
// and $refs to whole components, and a value outside a type, enum, format or
// bound. Everything under oneOf, anyOf, not, if, then, else, contains,
// propertyNames, patternProperties or dependentSchemas, and behind a $ref
// that is not to a whole component, is checked as the document writes it. It
// checks the values it is given: a field a test leaves at its zero value is
// checked as the zero value only. It cannot tell whether the handler returns
// that component, which is the test's own choice. A component with $id or
// $anchor does not compile when the checked component reaches it both
// closed and as written, such as under a oneOf or anyOf. A YAML document is
// read by the values its scalars have in YAML, not as written: an integer
// key or value not in plain decimal, such as 01 or 0x10, an unquoted
// timestamp, and a number past the range of a uint64, which is rounded.
//
// Both take the document as bytes, JSON or YAML, OpenAPI 3.0 or 3.1, such as
// the 3.0.3 the generator writes (openapi.json, openapi.yaml, and their
// .internal forms).
// They report with t.Errorf, so a test sees every mismatch of a call, except
// that Schema lists at most validate.MaxFields (100) failures and then how
// many more there are.
package spectest
