package response

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/open-rails/openrails/pkg/api"
)

// These are the gin-middleware layer's error writers. They emit the SAME
// Stripe-style envelope as the rest of the HTTP surface — pkg/api's
// {"error":{type,code,message}} — so a client decodes one error shape everywhere
// (httprequest handlers via Request.ErrorJSON, raw-gin handlers, and middleware
// alike). pkg/api is the single source of truth for the shape; each helper just
// pins the status and lets api.SimpleErrorResponse infer the type + machine code.

// BadRequest sends a 400 Bad Request error.
func BadRequest(c *gin.Context, message string) {
	c.JSON(http.StatusBadRequest, api.SimpleErrorResponse(http.StatusBadRequest, message))
}

// UnauthorizedWithMessage sends a 401 Unauthorized error with a message.
func UnauthorizedWithMessage(c *gin.Context, message string) {
	c.JSON(http.StatusUnauthorized, api.SimpleErrorResponse(http.StatusUnauthorized, message))
}

// ForbiddenWithMessage sends a 403 Forbidden error with a message.
func ForbiddenWithMessage(c *gin.Context, message string) {
	c.JSON(http.StatusForbidden, api.SimpleErrorResponse(http.StatusForbidden, message))
}

// TooManyRequests sends a 429 Too Many Requests error.
func TooManyRequests(c *gin.Context, message string) {
	c.JSON(http.StatusTooManyRequests, api.SimpleErrorResponse(http.StatusTooManyRequests, message))
}

// RequestEntityTooLarge sends a 413 Request Entity Too Large error.
func RequestEntityTooLarge(c *gin.Context, message string) {
	c.JSON(http.StatusRequestEntityTooLarge, api.SimpleErrorResponse(http.StatusRequestEntityTooLarge, message))
}

// InternalError sends a 500 Internal Server Error.
func InternalError(c *gin.Context, message string) {
	c.JSON(http.StatusInternalServerError, api.SimpleErrorResponse(http.StatusInternalServerError, message))
}

// ServiceUnavailable sends a 503 Service Unavailable error.
func ServiceUnavailable(c *gin.Context, message string) {
	c.JSON(http.StatusServiceUnavailable, api.SimpleErrorResponse(http.StatusServiceUnavailable, message))
}
