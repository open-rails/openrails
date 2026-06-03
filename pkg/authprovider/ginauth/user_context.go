package ginauth

import (
	"github.com/gin-gonic/gin"

	"github.com/open-rails/openrails/pkg/billingauth"
)

// UserContext is the framework-neutral identity contract (canonical home:
// pkg/billingauth). Aliased here for the gin-side helpers below.
type UserContext = billingauth.UserContext

// UserContextFromGin extracts user context from a Gin context.
// Checks both the Gin context values and the request context.
func UserContextFromGin(c *gin.Context) (UserContext, bool) {
	// Check Gin context first
	if v, ok := c.Get(ginUserContextKey); ok {
		if uc, ok := v.(UserContext); ok {
			return uc, true
		}
	}
	// Fallback to request context
	return billingauth.FromContext(c.Request.Context())
}

// GetUserContext returns user context or an error if not present/unauthenticated.
func GetUserContext(c *gin.Context) (UserContext, error) {
	if uc, ok := UserContextFromGin(c); ok {
		return uc, nil
	}
	return UserContext{}, billingauth.ErrUnauthenticated
}

// UserID extracts the user ID from a Gin context.
func UserID(c *gin.Context) (string, bool) {
	if uc, ok := UserContextFromGin(c); ok && uc.UserID != "" {
		return uc.UserID, true
	}
	return "", false
}

// Email extracts the email from a Gin context.
func Email(c *gin.Context) (string, bool) {
	if uc, ok := UserContextFromGin(c); ok && uc.Email != "" {
		return uc.Email, true
	}
	return "", false
}

// Roles extracts the roles from a Gin context.
func Roles(c *gin.Context) []string {
	if uc, ok := UserContextFromGin(c); ok {
		return uc.Roles
	}
	return nil
}

// Entitlements extracts the entitlements from a Gin context.
func Entitlements(c *gin.Context) []string {
	if uc, ok := UserContextFromGin(c); ok {
		return uc.Entitlements
	}
	return nil
}
