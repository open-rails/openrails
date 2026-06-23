package ginmw

import (
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/http/response"
	"github.com/open-rails/openrails/pkg/authprovider"
)

// CustomerIDMatchesDelegated reports whether the :customer_id path segment names
// the same customer as the resolved delegated principal — by slug, merchant
// string, or merchant id. The customer-treasury surface forces the payer from
// :customer_id (it never accepts a body customer_id), so a credential may act
// ONLY on its OWN balance; a mismatch is a cross-customer attempt.
func CustomerIDMatchesDelegated(customerID string, resolved *controlplane.ResolvedDelegated) bool {
	customerID = strings.TrimSpace(customerID)
	if customerID == "" || resolved == nil {
		return false
	}
	candidates := []string{resolved.Merchant, resolved.MerchantSlug}
	if !resolved.MerchantID.IsZero() {
		candidates = append(candidates, resolved.MerchantID.String())
	}
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate) == customerID {
			return true
		}
	}
	return false
}

// CustomerScopeRequired gates the customer-as-payer treasury surface
// (/v1/customers/:customer_id/*, #567 — collapses the old org-treasury #566
// surface into the universal customer persona). It runs AFTER the delegated
// authentication middleware and BEFORE the per-route permission gates and
// handlers, and:
//
//   - confirms the :customer_id segment matches the resolved principal's
//     customer (403 customer_scope_mismatch on a cross-customer attempt), and
//   - REBINDS the acting payer subject to the customer's PAYABLE SUBJECT (the
//     customer/merchant uuid) so the SHARED /v1/me money handlers — which
//     resolve the acting subject via GetUser()/selfAccountPayer — operate on the
//     customer balance rather than the individual delegated subject. Every
//     customer is a payer that can delegate spend of its balance.
//
// The rebind carries customer-level identity only (no individual PII) and NEVER
// touches permissions: those stay on the resolved principal (PrincipalContextKey)
// and are enforced by RequirePermission, so rebinding the payer can never widen
// authority. The pinned merchant (set by the delegated middleware) is unchanged,
// so RLS stays scoped to the customer.
func CustomerScopeRequired() gin.HandlerFunc {
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
		if !CustomerIDMatchesDelegated(c.Param("customer_id"), resolved) {
			response.ForbiddenWithMessage(c, "customer_scope_mismatch")
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
