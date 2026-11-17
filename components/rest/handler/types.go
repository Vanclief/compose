package handler

// ErrorResponse defines the structure of the error that the handler will return
type ErrorResponse struct {
	Error StandardError `json:"error"`
}

// StandardError defines a error in JSON format
type StandardError struct {
	// Code is the ez code of the error. Its enum tag lists the codes of ez,
	// in the order components/rest/generator lists them, so a document the
	// api registry builds names them as a generated one does. An app that
	// invents a code of its own answers one the documents do not list.
	Code      string `json:"code" enum:"conflict,internal,invalid,not_found,not_authorized,not_authenticated,resource_exhausted,not_implemented,unavailable,timeout,canceled" doc:"Machine-readable error code, the ez code of the error."`
	Message   string `json:"message" doc:"Human-readable message, translated when the request carries a locale."`
	RequestID string `json:"request_id" doc:"Correlates this response with the backend logs."`
	// Fields lists the request fields that failed validation, at most
	// validate.MaxFields of them. Only rest.ErrorHandler sets it, when the
	// error it answers is an ez EINVALID built by validate.NewFieldsError:
	// the api registry's for a request that failed its validation, or an
	// app's own. The key is absent otherwise.
	Fields []FieldError `json:"fields,omitempty" nullable:"false" maxItems:"100" doc:"The request fields that failed validation, at most 100. Present only on the 400 of a request that failed request validation."`
}

// FieldError is one request field that failed validation, as an error
// answer lists it under fields. It is validate.FieldError on the wire.
type FieldError struct {
	In      string `json:"in" doc:"Where the field is in the request: path, query, header or body."`
	Name    string `json:"name" doc:"The parameter name, or the dotted path inside the parameter or the body (tags.1, config.name), with * in place of a map key. Empty when the entry is about the location itself, such as a missing body."`
	Message string `json:"message" doc:"Why the field failed. It never contains the submitted value."`
}
