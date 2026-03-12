package middleware

import (
	"strings"

	"github.com/doujins-org/ginapi/response"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// APIKeyRequired enforces the presence of a valid X-API-KEY header.
// This is used for server-to-server authentication on the private/service API.
func APIKeyRequired(expectedKey string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if strings.TrimSpace(expectedKey) == "" {
			log.Warn("api key middleware misconfigured: no expected key provided")
			response.InternalError(c, "service authentication not configured")
			c.Abort()
			return
		}

		providedKey := strings.TrimSpace(c.GetHeader("X-API-KEY"))
		if providedKey == "" {
			response.UnauthorizedWithMessage(c, "X-API-KEY header required")
			c.Abort()
			return
		}

		// Constant-time comparison to prevent timing attacks
		if !constantTimeCompare(providedKey, expectedKey) {
			log.Warn("invalid api key provided for service endpoint")
			response.UnauthorizedWithMessage(c, "invalid API key")
			c.Abort()
			return
		}

		c.Next()
	}
}

// constantTimeCompare performs a constant-time string comparison to prevent timing attacks.
func constantTimeCompare(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var result byte
	for i := 0; i < len(a); i++ {
		result |= a[i] ^ b[i]
	}
	return result == 0
}
