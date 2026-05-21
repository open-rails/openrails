package authprovider

import (
	"context"
	"errors"
	"strings"

	"github.com/gin-gonic/gin"
)

// UserContext represents authenticated user information.
// This is billing's own user context type, decoupled from external auth libraries.
//
// Custom auth providers should implement the Provider interface and populate this struct
// in their middleware, then set it in the Gin context.
type UserContext struct {
	// UserID is the unique identifier for the user (required)
	UserID string

	// Email is the user's email address (optional)
	Email string

	// EmailVerified indicates whether the email has been verified
	EmailVerified bool

	// Username is the user's display name (optional)
	Username string

	// DiscordUsername is the user's Discord handle (optional, for platforms with Discord integration)
	DiscordUsername string

	// SessionID is the identifier for the current session (optional)
	SessionID string

	// Roles is a list of roles assigned to the user (e.g., "admin", "moderator")
	Roles []string

	// Entitlements is a list of entitlements/permissions the user has (e.g., "premium", "pro")
	Entitlements []string

	// Org is the slug of the user's active organization context (optional).
	// Populated by host apps integrating with multi-org auth systems like AuthKit.
	// Empty when the host app does not use orgs, or when no org is active for the request.
	Org string

	// OrgRoles is the list of roles the user holds within Org (e.g., ["admin", "billing_admin"]).
	// Only meaningful when Org is non-empty.
	OrgRoles []string
}

// HasRole checks if the user has a specific role (case-insensitive).
func (uc UserContext) HasRole(role string) bool {
	for _, r := range uc.Roles {
		if strings.EqualFold(r, role) {
			return true
		}
	}
	return false
}

// HasEntitlement checks if the user has a specific entitlement (case-insensitive).
func (uc UserContext) HasEntitlement(ent string) bool {
	for _, e := range uc.Entitlements {
		if strings.EqualFold(e, ent) {
			return true
		}
	}
	return false
}

// HasAnyOrgRole returns true if the user holds any of the listed roles within Org (case-insensitive).
// Always returns false when Org is empty or want is empty.
func (uc UserContext) HasAnyOrgRole(want ...string) bool {
	if uc.Org == "" || len(want) == 0 {
		return false
	}
	for _, r := range uc.OrgRoles {
		for _, w := range want {
			if strings.EqualFold(r, w) {
				return true
			}
		}
	}
	return false
}

// userContextCtxKey is the context key for storing user context
type userContextCtxKey struct{}

// SetUserContext returns a child context with user context attached.
func SetUserContext(ctx context.Context, uc UserContext) context.Context {
	return context.WithValue(ctx, userContextCtxKey{}, uc)
}

// FromContext extracts user context from a standard context.
func FromContext(ctx context.Context) (UserContext, bool) {
	v := ctx.Value(userContextCtxKey{})
	if v == nil {
		return UserContext{}, false
	}
	uc, ok := v.(UserContext)
	return uc, ok
}

// UserContextFromGin extracts user context from a Gin context.
// Checks both the Gin context values and the request context.
func UserContextFromGin(c *gin.Context) (UserContext, bool) {
	// Check Gin context first
	if v, ok := c.Get("billing.user_context"); ok {
		if uc, ok := v.(UserContext); ok {
			return uc, true
		}
	}
	// Fallback to request context
	return FromContext(c.Request.Context())
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

// ErrUnauthenticated is returned when authentication is required but not present.
var ErrUnauthenticated = errors.New("unauthenticated")
