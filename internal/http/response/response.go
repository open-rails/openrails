package response

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
)

// Error is the structured error envelope returned by Gin handlers.
type Error struct {
	Object string    `json:"object"`
	Error  ErrorInfo `json:"error"`
}

// ErrorInfo contains client-facing error details.
type ErrorInfo struct {
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
	Param   string `json:"param,omitempty"`
}

const (
	ErrorTypeInvalidRequest = "invalid_request"
	ErrorTypeAuthentication = "authentication"
	ErrorTypeForbidden      = "forbidden"
	ErrorTypeNotFound       = "not_found"
	ErrorTypeConflict       = "conflict"
	ErrorTypeRateLimit      = "rate_limit"
	ErrorTypeAPI            = "api"
)

const (
	ErrorCodeInvalidParam           = "invalid_param"
	ErrorCodeMissingParam           = "missing_param"
	ErrorCodeInvalidFormat          = "invalid_format"
	ErrorCodeResourceNotFound       = "resource_not_found"
	ErrorCodeAlreadyExists          = "already_exists"
	ErrorCodeAuthRequired           = "auth_required"
	ErrorCodeInvalidToken           = "invalid_token"
	ErrorCodeTokenExpired           = "token_expired"
	ErrorCodeInsufficientPermission = "insufficient_permission"
	ErrorCodeRateLimitExceeded      = "rate_limit_exceeded"
	ErrorCodeInternal               = "internal"
	ErrorCodeServiceUnavailable     = "service_unavailable"
)

// List is a Stripe-style list response with offset/limit pagination.
type List[T any] struct {
	Object  string `json:"object"`
	Data    []T    `json:"data"`
	Total   int64  `json:"total"`
	Limit   int    `json:"limit"`
	Offset  int    `json:"offset"`
	HasMore bool   `json:"has_more"`
}

// NewList creates a list response with has_more calculated automatically.
func NewList[T any](data []T, total int64, limit, offset int) List[T] {
	if data == nil {
		data = []T{}
	}

	return List[T]{
		Object:  "list",
		Data:    data,
		Total:   total,
		Limit:   limit,
		Offset:  offset,
		HasMore: int64(offset+len(data)) < total,
	}
}

// ListResponse sends a list response.
func ListResponse[T any](c *gin.Context, data []T, total int64, limit, offset int) {
	c.JSON(http.StatusOK, NewList(data, total, limit, offset))
}

func sendError(c *gin.Context, status int, errType, code, message, param string) {
	c.JSON(status, Error{
		Object: "error",
		Error: ErrorInfo{
			Type:    errType,
			Code:    code,
			Message: message,
			Param:   param,
		},
	})
}

// BadRequest sends a 400 Bad Request error.
func BadRequest(c *gin.Context, message string) {
	sendError(c, http.StatusBadRequest, ErrorTypeInvalidRequest, "", message, "")
}

// BadRequestWithCode sends a 400 Bad Request error with a code.
func BadRequestWithCode(c *gin.Context, code, message string) {
	sendError(c, http.StatusBadRequest, ErrorTypeInvalidRequest, code, message, "")
}

// BadRequestParam sends a 400 Bad Request error for a parameter.
func BadRequestParam(c *gin.Context, param, message string) {
	sendError(c, http.StatusBadRequest, ErrorTypeInvalidRequest, "", message, param)
}

// Unauthorized sends a 401 Unauthorized error.
func Unauthorized(c *gin.Context) {
	sendError(c, http.StatusUnauthorized, ErrorTypeAuthentication, "", "unauthorized", "")
}

// UnauthorizedWithMessage sends a 401 Unauthorized error with a message.
func UnauthorizedWithMessage(c *gin.Context, message string) {
	sendError(c, http.StatusUnauthorized, ErrorTypeAuthentication, "", message, "")
}

// Forbidden sends a 403 Forbidden error.
func Forbidden(c *gin.Context) {
	sendError(c, http.StatusForbidden, ErrorTypeForbidden, "", "forbidden", "")
}

// ForbiddenWithMessage sends a 403 Forbidden error with a message.
func ForbiddenWithMessage(c *gin.Context, message string) {
	sendError(c, http.StatusForbidden, ErrorTypeForbidden, "", message, "")
}

// NotFound sends a 404 Not Found error for an entity.
func NotFound(c *gin.Context, entity string) {
	sendError(c, http.StatusNotFound, ErrorTypeNotFound, "", fmt.Sprintf("%s not found", entity), "")
}

// NotFoundWithMessage sends a 404 Not Found error with a message.
func NotFoundWithMessage(c *gin.Context, message string) {
	sendError(c, http.StatusNotFound, ErrorTypeNotFound, "", message, "")
}

// Conflict sends a 409 Conflict error.
func Conflict(c *gin.Context, message string) {
	sendError(c, http.StatusConflict, ErrorTypeConflict, "", message, "")
}

// TooManyRequests sends a 429 Too Many Requests error.
func TooManyRequests(c *gin.Context, message string) {
	sendError(c, http.StatusTooManyRequests, ErrorTypeRateLimit, "", message, "")
}

// InternalError sends a 500 Internal Server Error.
func InternalError(c *gin.Context, message string) {
	sendError(c, http.StatusInternalServerError, ErrorTypeAPI, "", message, "")
}

// ServiceUnavailable sends a 503 Service Unavailable error.
func ServiceUnavailable(c *gin.Context, message string) {
	sendError(c, http.StatusServiceUnavailable, ErrorTypeAPI, "", message, "")
}

// NotImplemented sends a 501 Not Implemented error.
func NotImplemented(c *gin.Context, message string) {
	sendError(c, http.StatusNotImplemented, ErrorTypeAPI, "", message, "")
}

// UnprocessableEntity sends a 422 Unprocessable Entity error.
func UnprocessableEntity(c *gin.Context, message string) {
	sendError(c, http.StatusUnprocessableEntity, ErrorTypeInvalidRequest, "", message, "")
}

// UnsupportedMediaType sends a 415 Unsupported Media Type error.
func UnsupportedMediaType(c *gin.Context, message string) {
	sendError(c, http.StatusUnsupportedMediaType, ErrorTypeInvalidRequest, "", message, "")
}

// BadGateway sends a 502 Bad Gateway error.
func BadGateway(c *gin.Context, message string) {
	sendError(c, http.StatusBadGateway, ErrorTypeAPI, "", message, "")
}
