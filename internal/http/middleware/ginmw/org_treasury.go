package ginmw

import (
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/http/response"
	"github.com/open-rails/openrails/pkg/authprovider"
)

// OrgIDMatchesDelegated reports whether the :org_id path segment names the same
// org as the resolved delegated principal — by org slug, merchant string, or
// merchant id. The org-treasury surface forces the payer from :org_id (it never
// accepts a customer_id), so a credential may act ONLY on its OWN org balance; a
// mismatch is a cross-org attempt.
func OrgIDMatchesDelegated(orgID string, resolved *controlplane.ResolvedDelegated) bool {
	orgID = strings.TrimSpace(orgID)
	if orgID == "" || resolved == nil {
		return false
	}
	candidates := []string{resolved.Merchant, resolved.MerchantSlug}
	if !resolved.MerchantID.IsZero() {
		candidates = append(candidates, resolved.MerchantID.String())
	}
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate) == orgID {
			return true
		}
	}
	return false
}

// OrgTreasuryScopeRequired gates the org-as-payer treasury surface
// (/v1/orgs/:org_id/*, #566). It runs AFTER the delegated authentication
// middleware and BEFORE the per-route permission gates and handlers, and:
//
//   - confirms the :org_id segment matches the resolved principal's org
//     (403 org_scope_mismatch on a cross-org attempt), and
//   - REBINDS the acting payer subject to the org's PAYABLE SUBJECT (the
//     merchant/org uuid) so the SHARED /v1/me money handlers — which resolve the
//     acting subject via GetUser()/selfAccountPayer — operate on the ORG balance
//     rather than the individual delegated subject. An org is a payer; its
//     members consume.
//
// The rebind carries org-level identity only (no individual PII) and NEVER
// touches permissions: those stay on the resolved principal (PrincipalContextKey)
// and are enforced by RequirePermission, so rebinding the payer can never widen
// authority. The pinned merchant (set by the delegated middleware) is unchanged,
// so RLS stays scoped to the org.
func OrgTreasuryScopeRequired() gin.HandlerFunc {
	return func(c *gin.Context) {
		resolved, ok := DelegatedFromGin(c)
		if !ok {
			response.UnauthorizedWithMessage(c, "delegated principal required")
			c.Abort()
			return
		}
		if resolved.MerchantID.IsZero() {
			response.UnauthorizedWithMessage(c, "delegated principal invalid")
			c.Abort()
			return
		}
		if !OrgIDMatchesDelegated(c.Param("org_id"), resolved) {
			response.ForbiddenWithMessage(c, "org_scope_mismatch")
			c.Abort()
			return
		}

		uc := authprovider.UserContext{
			UserID:   resolved.MerchantID.UUID().String(),
			Username: resolved.Merchant,
			Org:      resolved.Merchant,
		}
		c.Request = c.Request.WithContext(authprovider.SetUserContext(c.Request.Context(), uc))
		c.Set("openrails.user_context", uc)

		c.Next()
	}
}
