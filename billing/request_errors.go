package openrails

// Transport refusal codes: the middleware answers before any handler exists,
// in the same envelope every handler uses.
const (
	// CodeRequestBodyTooLarge: the request body exceeds the deployment's cap
	// (HTTP 413, type invalid_request_error). The same cap applies in every
	// deployment, including the embedded in-process Client.
	CodeRequestBodyTooLarge = "request_body_too_large"
	// CodeInvalidRequestBody: the request body could not be read (HTTP 400).
	CodeInvalidRequestBody = "invalid_request_body"
)

var ErrRequestBodyTooLarge error = newCodedError(CodeRequestBodyTooLarge, ErrInvalid)
