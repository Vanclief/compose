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
}
