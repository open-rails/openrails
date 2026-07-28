package api

import (
	"fmt"
	"net/http"
)

// Error types matching Stripe's error taxonomy
const (
	// ErrorTypeInvalidRequest is for errors when the request has invalid parameters
	ErrorTypeInvalidRequest = "invalid_request_error"
	// ErrorTypeAuthentication is for errors with authentication (missing/invalid token)
	ErrorTypeAuthentication = "authentication_error"
	// ErrorTypeAuthorization is for errors when authenticated but not authorized
	ErrorTypeAuthorization = "authorization_error"
	// ErrorTypeAPI is for internal server errors
	ErrorTypeAPI = "api_error"
	// ErrorTypeCard is for card-related errors (declined, expired, etc.)
	ErrorTypeCard = "card_error"
	// ErrorTypeRateLimit is for rate limiting errors
	ErrorTypeRateLimit = "rate_limit_error"
)

// Common error codes
const (
	// Request validation errors
	CodeInvalidParam     = "invalid_param"
	CodeResourceNotFound = "resource_not_found"
	CodeResourceConflict = "resource_conflict"

	// Authentication/authorization errors
	CodeAuthRequired         = "authentication_required"
	CodeResourceAccessDenied = "resource_access_denied"

	// Payment/card errors
	CodeInsufficientFunds = "insufficient_funds"
	CodePaymentFailed     = "payment_failed"

	// Rate limiting
	CodeRateLimitExceeded = "rate_limit_exceeded"

	// Internal errors
	CodeInternalError      = "internal_error"
	CodeServiceUnavailable = "service_unavailable"
)

// ErrorDetails contains the detailed error information (nested under "error" key)
type ErrorDetails struct {
	Type     string         `json:"type"`               // Error type category
	Code     string         `json:"code"`               // Machine-readable error code
	Message  string         `json:"message"`            // Human-readable message
	Param    *string        `json:"param,omitempty"`    // Parameter that caused the error (if applicable)
	Metadata map[string]any `json:"metadata,omitempty"` // Machine-readable context for actionable errors.
}

// ErrorResponse is the top-level error response wrapper
type ErrorResponse struct {
	Error ErrorDetails `json:"error"`
}

// APIError represents an error that can be returned to clients
type APIError struct {
	HTTPStatus int
	Type       string
	Code       string
	Message    string
	Param      *string
	Metadata   map[string]any
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
			Type:     e.Type,
			Code:     e.Code,
			Message:  e.Message,
			Param:    e.Param,
			Metadata: e.Metadata,
		},
	}
}

// SimpleErrorResponse creates a Stripe-style error response from an HTTP status code and message.
// The error type and code are inferred from the status code.
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

// inferErrorTypeAndCode determines the error type and code from an HTTP status code
func inferErrorTypeAndCode(httpStatus int) (errType string, code string) {
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
func NewAPIError(httpStatus int, errType, code, message string) *APIError {
	return &APIError{
		HTTPStatus: httpStatus,
		Type:       errType,
		Code:       code,
		Message:    message,
	}
}

// WithParam adds a parameter name to the error
func (e *APIError) WithParam(param string) *APIError {
	e.Param = &param
	return e
}

// WithMetadata adds machine-readable context to the error response.
func (e *APIError) WithMetadata(metadata map[string]any) *APIError {
	e.Metadata = metadata
	return e
}

// ConflictError creates an error for resource conflicts.
func ConflictError(message string) *APIError {
	return NewAPIError(http.StatusConflict, ErrorTypeInvalidRequest, CodeResourceConflict, message)
}
