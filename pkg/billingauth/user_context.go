// Package billingauth is the framework-neutral identity & auth boundary for
// embedded OpenRails openrails. It depends only on the standard library plus
// github.com/google/uuid — no gin, no AuthKit — so a host application can
// import it without pulling in HTTP frameworks or auth libraries it does not
// use.
//
// A host brings its own auth by implementing Authenticator; OpenRails attaches
// the resulting UserContext to the request context. The AuthKit-backed
// implementation lives app-side (internal/auth), not here.
package billingauth

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// MerchantSelectorHeader names the explicit merchant context requested by a
// human user session. The value is an untrusted routing hint: merchant route
// gates must still verify the user's live permission for the selected merchant
// and resolve it to an active merchant before authorizing the request.
const MerchantSelectorHeader = "X-OpenRails-Merchant"

// UserContext is the identity OpenRails billing reads about the caller. It is
// the contract between the host's auth system and the library: the host
// populates whichever fields apply to its identity model and OpenRails treats
// everything beyond UserID as optional.
type UserContext struct {
	// UserID is the unique identifier for the principal/payer (required). It
	// MUST be a UUID (#364/#766): OpenRails uses it directly as the payable
	// customer_id, and ValidateSubject enforces this at every auth middleware.
	// A host whose native subject ids are NOT UUIDs (e.g. sequential integers,
	// external-provider opaque strings) must map them to a stable UUID before
	// returning UserContext from its Authenticator — otherwise EVERY request
	// from that host fails this gate: required routes 401 ("subject ... is not
	// a UUID"), optional routes silently downgrade to anonymous (no error
	// surfaced), a correctness footgun that looks like "auth isn't working" if
	// undocumented.
	UserID string

	// Email is the user's email address (optional).
	Email string

	// EmailVerified indicates whether the email has been verified.
	EmailVerified bool

	// Username is the user's display name (optional).
	Username string

	// DiscordUsername is the user's Discord handle (optional).
	DiscordUsername string

	// SessionID is the identifier for the current session (optional). AuthKit's
	// `sid` claim remains session identity for logout/reauth/freshness flows; it
	// is not profile or authorization data.
	SessionID string

	// Roles is a list of roles assigned to the user (e.g., "admin", "moderator").
	Roles []string

	// Entitlements is the verified AuthKit token's entitlement snapshot. OpenRails
	// treats these claims as authoritative for short-lived user-token access
	// decisions by default. Embedded deployments that need grant/revoke changes
	// before token expiry can opt into EntitlementFreshnessResolver at the gate.
	Entitlements []string

	// Merchant is the slug of the merchant context the user is acting in
	// (optional). Empty when no merchant context is active.
	Merchant string

	// MerchantRoles is the list of roles the user holds within Merchant. Only
	// meaningful when Merchant is non-empty.
	MerchantRoles []string
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

// HasEntitlement checks the verified token entitlement snapshot for a specific
// entitlement (case-insensitive).
func (uc UserContext) HasEntitlement(ent string) bool {
	for _, e := range uc.Entitlements {
		if strings.EqualFold(e, ent) {
			return true
		}
	}
	return false
}

// EntitlementFreshnessResolver is an optional embedded hook for deployments that
// need entitlement grant/revoke changes reflected before the short-lived access
// token expires. It is a freshness/revocation check, not a replacement authority
// model: nil means trust the verified token snapshot.
type EntitlementFreshnessResolver interface {
	HasFreshEntitlement(ctx context.Context, user UserContext, entitlement string) (bool, error)
}

// EntitlementFreshnessResolverFunc adapts a function to EntitlementFreshnessResolver.
type EntitlementFreshnessResolverFunc func(ctx context.Context, user UserContext, entitlement string) (bool, error)

// HasFreshEntitlement implements EntitlementFreshnessResolver.
func (f EntitlementFreshnessResolverFunc) HasFreshEntitlement(ctx context.Context, user UserContext, entitlement string) (bool, error) {
	return f(ctx, user, entitlement)
}

// HasEntitlementFresh checks an entitlement using an optional embedded freshness
// resolver. With no resolver configured, verified JWT entitlement claims are the
// authoritative short-lived snapshot.
func (uc UserContext) HasEntitlementFresh(ctx context.Context, resolver EntitlementFreshnessResolver, entitlement string) (bool, error) {
	if resolver != nil {
		return resolver.HasFreshEntitlement(ctx, uc, entitlement)
	}
	return uc.HasEntitlement(entitlement), nil
}

// HasAnyMerchantRole returns true if the user holds any of the listed roles
// within Merchant (case-insensitive). Always returns false when Merchant is empty
// or want is empty.
func (uc UserContext) HasAnyMerchantRole(want ...string) bool {
	if uc.Merchant == "" || len(want) == 0 {
		return false
	}
	for _, r := range uc.MerchantRoles {
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
