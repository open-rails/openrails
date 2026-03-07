package middleware

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/pkg/authprovider"
)

// RequestIDKey is the key for request ID in gin.Context
const RequestIDKey = "request_id"

// contextKey is a custom type for context keys to avoid collisions
type contextKey string

// ContextKeyRequestID is the context key for request ID
const ContextKeyRequestID contextKey = "request_id"

// Logger middleware with enhanced logging for billing service
func Logger() gin.HandlerFunc {
	return gin.LoggerWithFormatter(func(param gin.LogFormatterParams) string {
		// Extract request ID from context
		requestID := ""
		if param.Keys != nil {
			if id, exists := param.Keys[RequestIDKey]; exists {
				requestID = id.(string)
			}
		}

		// Log to structured logger
		fields := log.Fields{
			"method":     param.Method,
			"path":       param.Path,
			"status":     param.StatusCode,
			"latency":    param.Latency,
			"client_ip":  param.ClientIP,
			"user_agent": param.Request.UserAgent(),
			"request_id": requestID,
		}

		// Add error information if present
		if param.ErrorMessage != "" {
			fields["error"] = param.ErrorMessage
		}

		// Log at different levels based on status code
		switch {
		case param.StatusCode >= 500:
			log.WithFields(fields).Error("HTTP request completed with server error")
		case param.StatusCode >= 400:
			log.WithFields(fields).Warn("HTTP request completed with client error")
		case param.StatusCode >= 300:
			log.WithFields(fields).Info("HTTP request completed with redirect")
		default:
			log.WithFields(fields).Info("HTTP request completed successfully")
		}

		// Return empty string as we're using structured logging
		return ""
	})
}

// RequestID middleware adds a unique request ID to each request
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Check if request ID already exists (from load balancer or proxy)
		requestID := c.GetHeader("X-Request-ID")
		if requestID == "" {
			// Generate new UUID for request ID
			requestID = uuid.New().String()
		}

		// Set request ID in context and headers
		c.Set(RequestIDKey, requestID)
		c.Header("X-Request-ID", requestID)

		// Add to request context for downstream services
		ctx := c.Request.Context()
		ctx = context.WithValue(ctx, ContextKeyRequestID, requestID)
		c.Request = c.Request.WithContext(ctx)

		c.Next()
	}
}

// responseBodyWriter captures response body for logging
type responseBodyWriter struct {
	gin.ResponseWriter
	body *bytes.Buffer
}

func (r *responseBodyWriter) Write(b []byte) (int, error) {
	r.body.Write(b)
	return r.ResponseWriter.Write(b)
}

// BillingAuditLog middleware logs detailed information for billing operations
func BillingAuditLog() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()

		// Capture request body for sensitive operations
		var requestBody []byte
		if shouldLogRequestBody(c) {
			requestBody, _ = io.ReadAll(c.Request.Body)
			c.Request.Body = io.NopCloser(bytes.NewBuffer(requestBody))
		}

		// Capture response body for audit purposes
		var responseWriter *responseBodyWriter
		if shouldLogResponseBody(c) {
			responseWriter = &responseBodyWriter{
				ResponseWriter: c.Writer,
				body:           bytes.NewBufferString(""),
			}
			c.Writer = responseWriter
		}

		c.Next()

		// Log detailed audit information for billing operations
		if isBillingOperation(c) {
			fields := log.Fields{
				"operation":    getBillingOperation(c),
				"method":       c.Request.Method,
				"path":         c.Request.URL.Path,
				"status_code":  c.Writer.Status(),
				"duration_ms":  time.Since(start).Milliseconds(),
				"client_ip":    getClientIP(c),
				"user_agent":   c.Request.UserAgent(),
				"content_type": c.GetHeader("Content-Type"),
			}

			// Add request ID
			if requestID, exists := c.Get(RequestIDKey); exists {
				fields["request_id"] = requestID
			}

			// Add user information if available
			if uc, ok := authprovider.UserContextFromGin(c); ok && uc.UserID != "" {
				fields["user_id"] = uc.UserID
			}

			// Add processor information if present in path
			if processor := extractProcessor(c.Request.URL.Path); processor != "" {
				fields["processor"] = processor
			}

			// Log request body for POST/PUT operations (excluding sensitive fields)
			if len(requestBody) > 0 {
				if sanitizedBody := sanitizeRequestBody(requestBody); sanitizedBody != "" {
					fields["request_body"] = sanitizedBody
				}
			}

			// Log response body for audit (excluding sensitive information)
			if responseWriter != nil && responseWriter.body.Len() > 0 {
				if sanitizedResponse := sanitizeResponseBody(responseWriter.body.Bytes()); sanitizedResponse != "" {
					fields["response_body"] = sanitizedResponse
				}
			}

			// Add error information if present
			if len(c.Errors) > 0 {
				fields["errors"] = c.Errors.String()
			}

			log.WithFields(fields).Info("Billing operation audit log")
		}
	}
}

// shouldLogRequestBody determines if request body should be captured for audit
func shouldLogRequestBody(c *gin.Context) bool {
	path := c.Request.URL.Path
	method := c.Request.Method

	// Log request body for sensitive operations
	return (method == http.MethodPost || method == http.MethodPut) &&
		(contains(path, "/subscriptions/") ||
			contains(path, "/payment-methods/") ||
			contains(path, "/webhook/") ||
			contains(path, "/solana/"))
}

// shouldLogResponseBody determines if response body should be captured for audit
func shouldLogResponseBody(c *gin.Context) bool {
	path := c.Request.URL.Path

	// Log response body for critical operations
	return contains(path, "/subscriptions/") ||
		contains(path, "/payment-methods/") ||
		contains(path, "/solana/generate") ||
		contains(path, "/solana/submit")
}

// isBillingOperation checks if the request is a billing-related operation
func isBillingOperation(c *gin.Context) bool {
	path := c.Request.URL.Path

	return contains(path, "/subscriptions/") ||
		contains(path, "/payment-methods/") ||
		contains(path, "/webhook/") ||
		contains(path, "/solana/") ||
		contains(path, "/billing/")
}

// getBillingOperation extracts the billing operation type from the request
func getBillingOperation(c *gin.Context) string {
	path := c.Request.URL.Path
	method := c.Request.Method

	switch {
	case contains(path, "/subscriptions/") && method == http.MethodPost:
		return "subscription_create"
	case contains(path, "/subscriptions/") && contains(path, "/cancel"):
		return "subscription_cancel"
	case contains(path, "/subscriptions/") && method == http.MethodGet:
		return "subscription_query"
	case contains(path, "/payment-methods/") && method == http.MethodPost:
		return "payment_method_create"
	case contains(path, "/payment-methods/") && method == http.MethodPut:
		return "payment_method_update"
	case contains(path, "/payment-methods/") && method == http.MethodDelete:
		return "payment_method_delete"
	case contains(path, "/webhook/"):
		return "webhook_processing"
	case contains(path, "/solana/generate"):
		return "solana_generate_transaction"
	case contains(path, "/solana/submit"):
		return "solana_submit_transaction"
	case contains(path, "/solana/qr"):
		return "solana_generate_qr"
	default:
		return "billing_operation"
	}
}

// extractProcessor extracts processor name from URL path
func extractProcessor(path string) string {
	if contains(path, "/nmi") || contains(path, "processor=nmi") {
		return "nmi"
	}
	if contains(path, "/ccbill") || contains(path, "processor=ccbill") {
		return "ccbill"
	}
	if contains(path, "/solana") {
		return "solana"
	}
	return ""
}

// sanitizeRequestBody removes sensitive information from request body for logging
func sanitizeRequestBody(body []byte) string {
	// For now, return empty string for security - in production, implement proper sanitization
	// that removes payment tokens, card numbers, etc. but keeps other useful audit information
	return "[REQUEST_BODY_SANITIZED]"
}

// sanitizeResponseBody removes sensitive information from response body for logging
func sanitizeResponseBody(body []byte) string {
	// For now, return empty string for security - in production, implement proper sanitization
	// that removes tokens, keys, etc. but keeps transaction IDs and status information
	return "[RESPONSE_BODY_SANITIZED]"
}

// contains checks if a string contains a substring
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || (len(s) > len(substr) &&
		(s[:len(substr)] == substr || s[len(s)-len(substr):] == substr ||
			bytes.Contains([]byte(s), []byte(substr)))))
}
