package response

import (
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
	ErrorTypeRateLimit      = "rate_limit"
	ErrorTypeAPI            = "api"
)

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

// UnauthorizedWithMessage sends a 401 Unauthorized error with a message.
func UnauthorizedWithMessage(c *gin.Context, message string) {
	sendError(c, http.StatusUnauthorized, ErrorTypeAuthentication, "", message, "")
}

// ForbiddenWithMessage sends a 403 Forbidden error with a message.
func ForbiddenWithMessage(c *gin.Context, message string) {
	sendError(c, http.StatusForbidden, ErrorTypeForbidden, "", message, "")
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
