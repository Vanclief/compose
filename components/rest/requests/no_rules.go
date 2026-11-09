package requests

// NoRules is embedded by request types that have no cross-field validation
// rules. Shape rules (required, format, enum, ranges) live in struct tags and
// are enforced by the OpenAPI registry before the request reaches the app, so
// Validate only exists to satisfy Body.
//
// Do not embed it next to another embedded type that already has a Validate
// method (for example pagination.CursorRequest): the two promoted methods are
// ambiguous and the request type stops implementing Body, which the compiler
// reports at the registration call.
type NoRules struct{}

// Validate always succeeds.
func (NoRules) Validate() error {
	return nil
}
