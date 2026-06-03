package authprovider

import (
	"github.com/gin-gonic/gin"

	"github.com/open-rails/openrails/pkg/billingauth"
)

// UserContext is the framework-neutral identity contract. The canonical
// definition now lives in the gin-free pkg/billingauth; this alias preserves the
// historical authprovider import path for the gin-side adapter code. New
// gin-free code should depend on pkg/billingauth directly.
type UserContext = billingauth.UserContext

// Re-exported gin-free helpers (canonical home: pkg/billingauth). Kept here so
// existing importers of authprovider keep compiling during the library split.
var (
	// SetUserContext returns a child context with user context attached.
	SetUserContext = billingauth.SetUserContext
	// FromContext extracts user context from a standard context.
	FromContext = billingauth.FromContext
	// ErrUnauthenticated is returned when authentication is required but absent.
	ErrUnauthenticated = billingauth.ErrUnauthenticated
)

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
	return UserContext{}, ErrUnauthenticated
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
