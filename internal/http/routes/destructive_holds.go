package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/http/handlers"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
)

// incidentClear requires additional root operator authority. Merchant config
// permission alone, application identities and delegation cannot clear holds.
func incidentClear(e *Env) router.Handler {
	return func(r *httprequest.Request) {
		staff, ok := r.Staff()
		v, verified := billingauth.VerifiedFrom(r.Request.Context())
		if !ok || !verified || !billingauth.Interactive(staff.Identity) {
			r.ErrorCode(billing.CodePermissionRequired, "incident clearance requires a self-acting human operator")
			return
		}
		root, err := rootScope(r.Request.Context(), e.Auth)
		if err != nil {
			r.ErrorCode(billing.CodeAuthorizationUnavailable, "")
			return
		}
		// A permission the host never declared is held by no one.
		unlock := asked(e.Auth, Pair{Root: billing.RootAdminRateLimitsUnlock}).Root
		allowed, err := can(r.Request.Context(), v, root, string(unlock))
		if err != nil {
			r.ErrorCode(billing.CodeAuthorizationUnavailable, "")
			return
		}
		if !allowed {
			r.ErrorCode(billing.CodePermissionRequired, "")
			return
		}
		handlers.ClearDestructiveHold(r)
	}
}
