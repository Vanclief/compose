// Package openapi turns Go request and response types into OpenAPI 3.1
// (JSON Schema draft 2020-12) component schemas and parameters.
//
// It uses the huma/v2 schema generator as a library (no huma router, API or
// validator) behind its own huma.Registry implementation, which adds compose
// specifics: package-qualified component names, uuid and compose types
// formats, enums.Set types as named enum components, request parameters from
// path/query/header tags, presence-required request variants, a struct tag
// lint and the removal of additionalProperties:false from published schemas.
//
// Usage: create a Reflector, call AddRequest/AddResponse for every operation
// type, then Finalize once to obtain the Result.
package openapi
