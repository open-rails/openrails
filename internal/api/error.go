package api

import (
	"fmt"
	"net/http"

	"github.com/open-rails/openrails/billing"
)

// Error envelope types (billing.ErrorType*).
const (
	ErrorTypeInvalidRequest = billing.ErrorTypeInvalidRequest
	ErrorTypeAuthentication = billing.ErrorTypeAuthentication
	ErrorTypeAuthorization  = billing.ErrorTypeAuthorization
	ErrorTypeAPI            = billing.ErrorTypeAPI
	ErrorTypeCard           = billing.ErrorTypeCard
	ErrorTypeRateLimit      = billing.ErrorTypeRateLimit
)

// Generic error codes (billing.Code*).
const (
	CodeInvalidParam         = billing.CodeInvalidParam
	CodeResourceNotFound     = billing.CodeResourceNotFound
	CodeResourceConflict     = billing.CodeResourceConflict
	CodeIdempotencyKeyReused = billing.CodeIdempotencyKeyReused
	CodeAuthRequired         = billing.CodeAuthenticationRequired
	CodeResourceAccessDenied = billing.CodeResourceAccessDenied
	CodeInsufficientFunds    = billing.CodeInsufficientFunds
	CodeInsufficientCredits  = billing.CodeInsufficientCredits
	CodePaymentFailed        = billing.CodePaymentFailed
	CodeRateLimitExceeded    = billing.CodeRateLimitExceeded
	CodeInternalError        = billing.CodeInternalError
	CodeServiceUnavailable   = billing.CodeServiceUnavailable
)

// ErrorDetails contains the detailed error information (nested under "error" key)
type ErrorDetails = billing.ErrorDetails

// ErrorResponse is the top-level error response wrapper
type ErrorResponse struct {
	Error ErrorDetails `json:"error"`
}

// APIError represents an error that can be returned to clients
type APIError struct {
	HTTPStatus int
	Type       billing.ErrorType
	Code       billing.ErrorCode
	Message    string
	RequestID  string
	Param      *string
	Metadata   map[string]any
	// Invoice is the invoice a failed payment left open.
	Invoice *billing.Invoice
	cause   error
	// unregistered is the code Coded refused to answer.
	unregistered billing.ErrorCode
}

// Error implements the error interface
func (e *APIError) Error() string {
	if e.Param != nil {
		return fmt.Sprintf("%s: %s (param: %s)", e.Code, e.Message, *e.Param)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// ToResponse converts an APIError to an ErrorResponse for JSON serialization
func (e *APIError) ToResponse() ErrorResponse {
	return ErrorResponse{
		Error: ErrorDetails{
			Type:      e.Type,
			Code:      e.Code,
			Message:   e.Message,
			RequestID: e.RequestID,
			Param:     e.Param,
			Metadata:  e.Metadata,
			Invoice:   e.Invoice,
		},
	}
}

// Coded is the refusal for a registered code: the registry fixes its status
// and type. An empty message answers the code's meaning. New refusals are
// built here; an unregistered code is a programming error and answers 500.
func Coded(code billing.ErrorCode, message string) *APIError {
	info, ok := billing.LookupErrorCode(code)
	if !ok {
		e := NewAPIError(http.StatusInternalServerError, ErrorTypeAPI, CodeInternalError, "internal error")
		e.unregistered = code
		return e
	}
	if message == "" {
		message = info.Meaning
	}
	return &APIError{HTTPStatus: info.Status, Type: info.Type, Code: code, Message: message}
}

// SimpleErrorResponse answers a status with its generic code. Handlers that
// know why they refuse use Coded instead.
func SimpleErrorResponse(httpStatus int, message string) ErrorResponse {
	errType, code := inferErrorTypeAndCode(httpStatus)
	return ErrorResponse{
		Error: ErrorDetails{
			Type:    errType,
			Code:    code,
			Message: message,
		},
	}
}

// ErrorTypeForStatus is the envelope category an HTTP status maps to.
func ErrorTypeForStatus(httpStatus int) billing.ErrorType {
	errType, _ := inferErrorTypeAndCode(httpStatus)
	return errType
}

// TypeForCode is a code's registered envelope type, or its status's category
// for a code a host hook supplied.
func TypeForCode(httpStatus int, code billing.ErrorCode) billing.ErrorType {
	if info, ok := billing.LookupErrorCode(code); ok {
		return info.Type
	}
	return ErrorTypeForStatus(httpStatus)
}

// inferErrorTypeAndCode is the generic type and code of an HTTP status.
func inferErrorTypeAndCode(httpStatus int) (billing.ErrorType, billing.ErrorCode) {
	switch httpStatus {
	case http.StatusBadRequest:
		return ErrorTypeInvalidRequest, CodeInvalidParam
	case http.StatusUnauthorized:
		return ErrorTypeAuthentication, CodeAuthRequired
	case http.StatusForbidden:
		return ErrorTypeAuthorization, CodeResourceAccessDenied
	case http.StatusNotFound:
		return ErrorTypeInvalidRequest, CodeResourceNotFound
	case http.StatusConflict:
		return ErrorTypeInvalidRequest, CodeResourceConflict
	case http.StatusTooManyRequests:
		return ErrorTypeRateLimit, CodeRateLimitExceeded
	case http.StatusPaymentRequired:
		return ErrorTypeCard, CodePaymentFailed
	case http.StatusServiceUnavailable:
		return ErrorTypeAPI, CodeServiceUnavailable
	default:
		if httpStatus >= 500 {
			return ErrorTypeAPI, CodeInternalError
		}
		return ErrorTypeInvalidRequest, CodeInvalidParam
	}
}

// NewAPIError creates a new APIError
func NewAPIError(httpStatus int, errType billing.ErrorType, code billing.ErrorCode, message string) *APIError {
	return &APIError{
		HTTPStatus: httpStatus,
		Type:       errType,
		Code:       code,
		Message:    message,
	}
}

// WithCause attaches server-only diagnostic context.
func (e *APIError) WithCause(cause error) *APIError {
	e.cause = cause
	return e
}

// Cause returns the server-only diagnostic cause.
func (e *APIError) Cause() error {
	return e.cause
}

// WithParam adds a parameter name to the error
func (e *APIError) WithParam(param string) *APIError {
	e.Param = &param
	return e
}

// WithRequestID adds the request correlation identifier to the error response.
func (e *APIError) WithRequestID(requestID string) *APIError {
	e.RequestID = requestID
	return e
}

// WithMetadata adds machine-readable context to the error response.
func (e *APIError) WithMetadata(metadata map[string]any) *APIError {
	e.Metadata = metadata
	return e
}

// Violation is how this answer departs from billing.ErrorCodes: a code it
// does not register, or another status than it registers; empty when none.
// It is a programming error, for the writer of the answer to log.
func (e *APIError) Violation() string {
	if e.unregistered != "" {
		return "error code " + string(e.unregistered) + " is not registered in billing.ErrorCodes"
	}
	if e.Code == "" {
		return ""
	}
	switch info, ok := billing.LookupErrorCode(e.Code); {
	case !ok:
		return "error code " + string(e.Code) + " is not registered in billing.ErrorCodes"
	case e.HTTPStatus != 0 && info.Status != e.HTTPStatus:
		return fmt.Sprintf("error code %s answered %d; billing.ErrorCodes registers %d", e.Code, e.HTTPStatus, info.Status)
	}
	return ""
}
