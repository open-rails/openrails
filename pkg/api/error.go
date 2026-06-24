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
	// ErrorTypeIdempotency is for idempotency key conflicts
	ErrorTypeIdempotency = "idempotency_error"
	// ErrorTypeRateLimit is for rate limiting errors
	ErrorTypeRateLimit = "rate_limit_error"
)

// Common error codes
const (
	// Request validation errors
	CodeMissingParam     = "missing_required_param"
	CodeInvalidParam     = "invalid_param"
	CodeParamOutOfRange  = "param_out_of_range"
	CodeInvalidIDFormat  = "invalid_id_format"
	CodeResourceNotFound = "resource_not_found"
	CodeResourceConflict = "resource_conflict"

	// Authentication/authorization errors
	CodeAuthRequired         = "authentication_required"
	CodeInvalidToken         = "invalid_token"
	CodeTokenExpired         = "token_expired"
	CodeInsufficientPerms    = "insufficient_permissions"
	CodeResourceAccessDenied = "resource_access_denied"

	// Payment/card errors
	CodeCardDeclined      = "card_declined"
	CodeCardExpired       = "expired_card"
	CodeInsufficientFunds = "insufficient_funds"
	CodePaymentFailed     = "payment_failed"
	CodeRailError         = "rail_error"

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
	DocURL   *string        `json:"doc_url,omitempty"`  // URL to documentation (optional)
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

// --- Helper constructors for common errors ---

// InvalidParamError creates an error for invalid parameter values
func InvalidParamError(param, message string) *APIError {
	return &APIError{
		HTTPStatus: http.StatusBadRequest,
		Type:       ErrorTypeInvalidRequest,
		Code:       CodeInvalidParam,
		Message:    message,
		Param:      &param,
	}
}

// MissingParamError creates an error for missing required parameters
func MissingParamError(param string) *APIError {
	return &APIError{
		HTTPStatus: http.StatusBadRequest,
		Type:       ErrorTypeInvalidRequest,
		Code:       CodeMissingParam,
		Message:    fmt.Sprintf("Missing required parameter: %s", param),
		Param:      &param,
	}
}

// InvalidIDError creates an error for invalid ID format
func InvalidIDError(resourceType string) *APIError {
	return &APIError{
		HTTPStatus: http.StatusBadRequest,
		Type:       ErrorTypeInvalidRequest,
		Code:       CodeInvalidIDFormat,
		Message:    fmt.Sprintf("Invalid %s ID format", resourceType),
	}
}

// NotFoundError creates an error for resources that don't exist
func NotFoundError(resourceType string) *APIError {
	return &APIError{
		HTTPStatus: http.StatusNotFound,
		Type:       ErrorTypeInvalidRequest,
		Code:       CodeResourceNotFound,
		Message:    fmt.Sprintf("%s not found", resourceType),
	}
}

// AuthRequiredError creates an error when authentication is required
func AuthRequiredError() *APIError {
	return &APIError{
		HTTPStatus: http.StatusUnauthorized,
		Type:       ErrorTypeAuthentication,
		Code:       CodeAuthRequired,
		Message:    "Authentication required",
	}
}

// InvalidTokenError creates an error for invalid authentication tokens
func InvalidTokenError() *APIError {
	return &APIError{
		HTTPStatus: http.StatusUnauthorized,
		Type:       ErrorTypeAuthentication,
		Code:       CodeInvalidToken,
		Message:    "Invalid authentication token",
	}
}

// AccessDeniedError creates an error when user doesn't have access to a resource
func AccessDeniedError(resourceType string) *APIError {
	return &APIError{
		HTTPStatus: http.StatusForbidden,
		Type:       ErrorTypeAuthorization,
		Code:       CodeResourceAccessDenied,
		Message:    fmt.Sprintf("You don't have access to this %s", resourceType),
	}
}

// InsufficientPermissionsError creates an error for missing permissions
func InsufficientPermissionsError(action string) *APIError {
	return &APIError{
		HTTPStatus: http.StatusForbidden,
		Type:       ErrorTypeAuthorization,
		Code:       CodeInsufficientPerms,
		Message:    fmt.Sprintf("Insufficient permissions to %s", action),
	}
}

// CardDeclinedError creates an error when a card is declined
func CardDeclinedError(reason string) *APIError {
	msg := "Card was declined"
	if reason != "" {
		msg = fmt.Sprintf("Card was declined: %s", reason)
	}
	return &APIError{
		HTTPStatus: http.StatusPaymentRequired,
		Type:       ErrorTypeCard,
		Code:       CodeCardDeclined,
		Message:    msg,
	}
}

// PaymentFailedError creates an error for payment processing failures
func PaymentFailedError(reason string) *APIError {
	msg := "Payment processing failed"
	if reason != "" {
		msg = fmt.Sprintf("Payment processing failed: %s", reason)
	}
	return &APIError{
		HTTPStatus: http.StatusBadRequest,
		Type:       ErrorTypeCard,
		Code:       CodePaymentFailed,
		Message:    msg,
	}
}

// RateLimitError creates an error when rate limit is exceeded
func RateLimitError() *APIError {
	return &APIError{
		HTTPStatus: http.StatusTooManyRequests,
		Type:       ErrorTypeRateLimit,
		Code:       CodeRateLimitExceeded,
		Message:    "Rate limit exceeded. Please try again later.",
	}
}

// InternalError creates an error for internal server errors
func InternalError(message string) *APIError {
	msg := "An internal error occurred"
	if message != "" {
		msg = message
	}
	return &APIError{
		HTTPStatus: http.StatusInternalServerError,
		Type:       ErrorTypeAPI,
		Code:       CodeInternalError,
		Message:    msg,
	}
}

// ServiceUnavailableError creates an error when a service is unavailable
func ServiceUnavailableError(service string) *APIError {
	msg := "Service temporarily unavailable"
	if service != "" {
		msg = fmt.Sprintf("%s service temporarily unavailable", service)
	}
	return &APIError{
		HTTPStatus: http.StatusServiceUnavailable,
		Type:       ErrorTypeAPI,
		Code:       CodeServiceUnavailable,
		Message:    msg,
	}
}

// ConflictError creates an error for resource conflicts
func ConflictError(message string) *APIError {
	return &APIError{
		HTTPStatus: http.StatusConflict,
		Type:       ErrorTypeInvalidRequest,
		Code:       CodeResourceConflict,
		Message:    message,
	}
}
