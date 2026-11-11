package handler

// ErrorResponse defines the structure of the error that the handler will return
type ErrorResponse struct {
	Error StandardError `json:"error"`
}

// StandardError defines a error in JSON format
type StandardError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
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
