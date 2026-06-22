package ginmw

import (
	"context"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/http/response"
	"github.com/open-rails/openrails/pkg/authprovider/ginauth"
	"github.com/open-rails/openrails/pkg/merchant"
)

// PrincipalContextKey holds the resolved bearer principal for a request.
const PrincipalContextKey = "openrails.principal"

// CredentialType identifies the bearer credential profile that authenticated
// the request. Webhooks are intentionally outside this model.
type CredentialType string

const (
	CredentialAPIKey            CredentialType = "api_key"
	CredentialRemoteApplication CredentialType = "remote_application"
	CredentialServiceJWT        CredentialType = "service_jwt"
	CredentialDelegatedUser     CredentialType = "delegated_user"
	CredentialHostDelegatedUser CredentialType = "host_delegated_user"
	CredentialUserSession       CredentialType = "user_session"
)

// Principal is the common bearer-auth result used by route permission gates.
// It carries only the data handlers and authorization need: merchant binding,
// credential provenance, acting end-user subject, and a permission evaluator
// preserving the credential-specific authority source. Subject is intentionally
// empty for machine credentials such as API keys and remote-application JWTs.
type Principal struct {
	MerchantID     merchant.ID
	MerchantSlug   string
	MerchantSource string
	CredentialType CredentialType
	Subject        string

	can func(context.Context, string) bool
}

// Can reports whether the resolved principal has perm. The implementation is
// intentionally credential-specific: API keys use stored AuthKit authority,
// delegated JWTs use signed delegated permissions, and future user sessions can
// use live authority checks.
func (p *Principal) Can(ctx context.Context, perm string) bool {
	if p == nil || p.can == nil {
		return false
	}
	return p.can(ctx, strings.TrimSpace(perm))
}

// PrincipalFromGin returns the bearer principal attached to the request.
func PrincipalFromGin(c *gin.Context) (*Principal, bool) {
	if c == nil {
		return nil, false
	}
	v, ok := c.Get(PrincipalContextKey)
	if !ok {
		return nil, false
	}
	p, ok := v.(*Principal)
	return p, ok && p != nil
}

// AdminPermissionChecker evaluates live user-session authority in the caller's
// own org. It matches the control plane without coupling this middleware to the
// policy package.
type AdminPermissionChecker interface {
	HasAdminPermission(ctx context.Context, orgSlug, userID, perm string) (bool, error)
}

// UserSessionAdminPrincipalRequired turns the authenticated AuthKit user session
// already attached by the auth provider into a Principal whose permission
// evaluator remains a live AuthKit check.
func UserSessionAdminPrincipalRequired(checker AdminPermissionChecker) gin.HandlerFunc {
	return func(c *gin.Context) {
		uc, ok := ginauth.UserContextFromGin(c)
		if !ok || strings.TrimSpace(uc.UserID) == "" {
			response.UnauthorizedWithMessage(c, "authentication required")
			c.Abort()
			return
		}
		if checker == nil {
			response.InternalError(c, "authorization unavailable")
			c.Abort()
			return
		}
		c.Set(PrincipalContextKey, userSessionPrincipal(uc.UserID, uc.Org, checker))
		c.Next()
	}
}

// userSessionPrincipal builds the Principal for a live AuthKit user session acting
// in its OWN org. Its permission evaluator stays a live AuthKit check and admits
// only org-plane namespaces — AuthKit org-management (`org:`) and OpenRails
// app-defined merchant resources (`merchant:`, #554). The separate `platform:`
// layer is never reachable from an org session.
func userSessionPrincipal(userID, orgSlug string, checker AdminPermissionChecker) *Principal {
	userID = strings.TrimSpace(userID)
	return &Principal{
		MerchantSource: "user_session_org",
		CredentialType: CredentialUserSession,
		Subject:        userID,
		can: func(ctx context.Context, perm string) bool {
			perm = strings.TrimSpace(perm)
			if !strings.HasPrefix(perm, "org:") && !strings.HasPrefix(perm, "merchant:") {
				return false
			}
			allowed, err := checker.HasAdminPermission(ctx, orgSlug, userID, perm)
			return err == nil && allowed
		},
	}
}

// RequirePermission gates a route on a permission held by the resolved bearer
// principal. It must run after the route group's authentication middleware.
func RequirePermission(perm string) gin.HandlerFunc {
	return requirePermissionWithMessage(perm, "permission_required")
}

func requirePermissionWithMessage(perm, message string) gin.HandlerFunc {
	perm = strings.TrimSpace(perm)
	return func(c *gin.Context) {
		principal, ok := PrincipalFromGin(c)
		if !ok {
			response.UnauthorizedWithMessage(c, "bearer principal required")
			c.Abort()
			return
		}
		if !principal.Can(c.Request.Context(), perm) {
			response.ForbiddenWithMessage(c, message)
			c.Abort()
			return
		}
		c.Next()
	}
}

func principalFromServiceCredential(resolved *controlplane.ResolvedServiceCredential, typ CredentialType) *Principal {
	if resolved == nil {
		return nil
	}
	return &Principal{
		MerchantID:     resolved.MerchantID,
		MerchantSlug:   resolved.MerchantSlug,
		MerchantSource: "service_credential",
		CredentialType: typ,
		can: func(_ context.Context, perm string) bool {
			return resolved.HasPermission(perm)
		},
	}
}

func principalFromDelegated(resolved *controlplane.ResolvedDelegated, typ CredentialType) *Principal {
	if resolved == nil {
		return nil
	}
	return &Principal{
		MerchantID:     resolved.MerchantID,
		MerchantSlug:   resolved.MerchantSlug,
		MerchantSource: "delegated_issuer",
		CredentialType: typ,
		Subject:        strings.TrimSpace(resolved.DelegatedSubject),
		can: func(_ context.Context, perm string) bool {
			// #564: resolved.Permissions is already the claim ∩ signer authority;
			// gate purely on the permission, never the credential type.
			return resolved.HasPermission(perm)
		},
	}
}
