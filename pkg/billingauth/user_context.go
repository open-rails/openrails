// Package billingauth is the framework-neutral identity & auth boundary for
// embedded OpenRails openrails. It depends only on the standard library plus
// github.com/google/uuid — no gin, no AuthKit — so a host application can
// import it without pulling in HTTP frameworks or auth libraries it does not
// use.
//
// A host brings its own auth by implementing Authenticator; OpenRails attaches
// the resulting UserContext to the request context. The gin adapter and the
// AuthKit-backed implementation live in app-side packages (pkg/authprovider and
// internal/auth respectively), not here.
package billingauth

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// UserContext is the identity OpenRails billing reads about the caller. It is
// the contract between the host's auth system and the library: the host
// populates whichever fields apply to its identity model and OpenRails treats
// everything beyond UserID as optional.
type UserContext struct {
	// UserID is the unique identifier for the principal/payer (required).
	UserID string

	// Email is the user's email address (optional).
	Email string

	// EmailVerified indicates whether the email has been verified.
	EmailVerified bool

	// Username is the user's display name (optional).
	Username string

	// DiscordUsername is the user's Discord handle (optional).
	DiscordUsername string

	// SessionID is the identifier for the current session (optional).
	SessionID string

	// Roles is a list of roles assigned to the user (e.g., "admin", "moderator").
	Roles []string

	// Entitlements is a list of entitlements/permissions the user has (e.g.,
	// "premium", "pro").
	Entitlements []string

	// Tenant is the slug of the user's active tenant context
	// (optional). Empty when the host has no tenant model or no tenant is active.
	Tenant string

	// TenantRoles is the list of roles the user holds within Tenant. Only meaningful
	// when Tenant is non-empty.
	TenantRoles []string
}

// ValidateSubject enforces the UUID-only payable-identity contract (#364) at
// the auth boundary: the verified principal's UserID must itself be a UUID,
// because OpenRails uses it directly as the payable customer_id. Every
// auth middleware (gin and net/http, Required and Optional) calls this after a
// successful Authenticate — required routes reject the request, optional
// routes treat it as anonymous. The returned message is client-safe.
func (uc UserContext) ValidateSubject() error {
	if _, err := uuid.Parse(strings.TrimSpace(uc.UserID)); err != nil {
		return fmt.Errorf("subject %q is not a UUID: OpenRails payable identities are UUID-only", uc.UserID)
	}
	return nil
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

// HasAnyTenantRole returns true if the user holds any of the listed roles within
// Tenant (case-insensitive). Always returns false when Tenant is empty or want is empty.
func (uc UserContext) HasAnyTenantRole(want ...string) bool {
	if uc.Tenant == "" || len(want) == 0 {
		return false
	}
	for _, r := range uc.TenantRoles {
		for _, w := range want {
			if strings.EqualFold(r, w) {
				return true
			}
		}
	}
	return false
}

// userContextCtxKey is the context key for storing user context.
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

// ErrUnauthenticated is returned when authentication is required but not present.
var ErrUnauthenticated = errors.New("unauthenticated")
