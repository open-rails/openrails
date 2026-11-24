package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/http/handlers"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/requestauth"
)

// incidentClear requires additional authority: root operator authority, or
// for the merchant's own erasure hold its provisioning authority
// (merchant:provisioning:manage). Merchant config permission alone,
// application identities and delegation cannot clear holds: only a
// self-acting human, or the person the in-process host names.
func incidentClear(e *Env) router.Handler {
	return func(r *httprequest.Request) {
		staff, ok := r.Staff()
		v, verified := billingauth.VerifiedFrom(r.Request.Context())
		if !ok || !verified || !billingauth.Interactive(staff.Identity) && !hostNamedPerson(r, staff.Identity) {
			r.ErrorCode(billing.CodePermissionRequired, "incident clearance requires a self-acting human operator")
			return
		}
		root, err := rootScope(r.Request.Context(), e.Auth)
		if err != nil {
			r.ErrorCode(billing.CodeAuthorizationUnavailable, "")
			return
		}
		// A permission the host never declared is held by no one.
		need := Pair{Root: billing.RootAdminRateLimitsUnlock}
		if erasureHold(r, staff.Merchant) {
			need = needProvisioning
		}
		allowed, err := holds(r.Request.Context(), v, root, staff.Merchant, asked(e.Auth, need))
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

// hostNamedPerson is the in-process host acting for a person it named
// (Client.ActingFor), whom it authenticated itself.
func hostNamedPerson(r *httprequest.Request, who billingauth.Identity) bool {
	host, inProcess := requestauth.HostPrincipalFromContext(r.Request.Context())
	return inProcess && host.Invoker != nil && who.Issuer == billingauth.HostIssuer && who.Invoker == *host.Invoker
}

// erasureHold reports whether the hold the path names is the merchant's
// erasure hold.
func erasureHold(r *httprequest.Request, mid billing.MerchantID) bool {
	id, err := billing.ParseDestructiveHoldID(r.Param("id"))
	if err != nil || r.State == nil || r.State.DestructiveAdmissions == nil {
		return false
	}
	hold, err := r.State.DestructiveAdmissions.GetHold(r.Request.Context(), mid.UUID(), id.UUID())
	return err == nil && hold.Scope == destructive.ScopeErasure
}
