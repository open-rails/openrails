package api

import (
	"fmt"
	"net/http"
	"sort"
	"sync"

	log "github.com/sirupsen/logrus"

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
	Type       string
	Code       string
	Message    string
	RequestID  string
	Param      *string
	Metadata   map[string]any
	cause      error
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
	observe(e.HTTPStatus, e.Code)
	return ErrorResponse{
		Error: ErrorDetails{
			Type:      e.Type,
			Code:      e.Code,
			Message:   e.Message,
			RequestID: e.RequestID,
			Param:     e.Param,
			Metadata:  e.Metadata,
		},
	}
}

// Coded is the refusal for a registered code: the registry fixes its status
// and type. An empty message answers the code's meaning. New refusals are
// built here; an unregistered code is a programming error and answers 500.
func Coded(code, message string) *APIError {
	info, ok := billing.LookupErrorCode(code)
	if !ok {
		observe(0, code)
		return NewAPIError(http.StatusInternalServerError, ErrorTypeAPI, CodeInternalError, "internal error")
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
func ErrorTypeForStatus(httpStatus int) string {
	errType, _ := inferErrorTypeAndCode(httpStatus)
	return errType
}

// TypeForCode is a code's registered envelope type, or its status's category
// for a code a host hook supplied.
func TypeForCode(httpStatus int, code string) string {
	if info, ok := billing.LookupErrorCode(code); ok {
		return info.Type
	}
	return ErrorTypeForStatus(httpStatus)
}

// inferErrorTypeAndCode is the generic type and code of an HTTP status.
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

// ConflictError creates an error for resource conflicts.
func ConflictError(message string) *APIError {
	return NewAPIError(http.StatusConflict, ErrorTypeInvalidRequest, CodeResourceConflict, message)
}

// violations are the answered codes billing's registry does not hold, by code.
var violations sync.Map

// observe records a code answered outside the registry: unregistered, or
// under another status than the registry's. It is logged once; the e2e suites
// fail on any (CodeViolations).
func observe(status int, code string) {
	if code == "" {
		return
	}
	problem := ""
	switch info, ok := billing.LookupErrorCode(code); {
	case !ok:
		problem = "error code " + code + " is not registered in billing.ErrorCodes"
	case status != 0 && info.Status != status:
		problem = fmt.Sprintf("error code %s answered %d; billing.ErrorCodes registers %d", code, status, info.Status)
	default:
		return
	}
	if _, seen := violations.LoadOrStore(problem, struct{}{}); !seen {
		log.Error(problem)
	}
}

// CodeViolations lists every code this process answered outside the registry.
func CodeViolations() []string {
	var out []string
	violations.Range(func(key, _ any) bool {
		out = append(out, key.(string))
		return true
	})
	sort.Strings(out)
	return out
}
