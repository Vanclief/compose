package apidocs

// ExampleMode selects which side of the wire an example is for: request
// examples drop readOnly properties, response examples drop writeOnly ones.
type ExampleMode int

const (
	ForRequest ExampleMode = iota
	ForResponse
)

// ExampleValue builds a sample value for s.
//
// STUB: implemented by the example generation task.
func (d *Document) ExampleValue(s *Schema, mode ExampleMode) any {
	return nil
}

// ExampleJSON renders ExampleValue as indented JSON.
//
// STUB: implemented by the example generation task.
func (d *Document) ExampleJSON(s *Schema, mode ExampleMode) string {
	return ""
}

// MediaExample picks the example for a media type: an explicit example, the
// first named example, or a value generated from the schema.
//
// STUB: implemented by the example generation task.
func (d *Document) MediaExample(mt *MediaType, mode ExampleMode) (any, bool) {
	return nil, false
}

// Curl builds a curl command for the operation.
//
// STUB: implemented by the example generation task.
func (d *Document) Curl(method, path string, item *PathItem, op *Operation, server string) string {
	return ""
}
