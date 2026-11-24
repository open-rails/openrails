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

// unlock is the root operator authority that clears any hold.
var unlock = Pair{Root: billing.RootAdminRateLimitsUnlock}

// incidentClear requires additional authority: root operator authority
// (root:admin-rate-limits:unlock), or for the merchant's own erasure hold its
// provisioning authority (merchant:provisioning:manage). Merchant config
// permission alone, application identities and delegation cannot clear
// holds: only a self-acting human, or the person the in-process host names.
func incidentClear(e *Env) router.Handler {
	return func(r *httprequest.Request) {
		needs := []Pair{unlock}
		if erasureHold(r) {
			needs = append(needs, needProvisioning)
		}
		clearHold(e, r, needs)
	}
}

// erasureClear clears the merchant's erasure hold, with the provisioning
// authority the route's gate checked; any other hold is not this route's.
func erasureClear(e *Env) router.Handler {
	return func(r *httprequest.Request) {
		if !erasureHold(r) {
			r.ErrorCode(billing.CodeResourceNotFound, "no erasure hold has this id")
			return
		}
		clearHold(e, r, []Pair{needProvisioning})
	}
}

// clearHold clears the hold the path names for a self-acting person holding
// one of needs. A permission the host never declared is held by no one.
func clearHold(e *Env, r *httprequest.Request, needs []Pair) {
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
	for _, need := range needs {
		switch allowed, err := holds(r.Request.Context(), v, root, staff.Merchant, asked(e.Auth, need)); {
		case err != nil:
			r.ErrorCode(billing.CodeAuthorizationUnavailable, "")
			return
		case allowed:
			handlers.ClearDestructiveHold(r)
			return
		}
	}
	r.ErrorCode(billing.CodePermissionRequired, "")
}

// hostNamedPerson is the in-process host acting for a person it named
// (Client.ActingFor), whom it authenticated itself.
func hostNamedPerson(r *httprequest.Request, who billingauth.Identity) bool {
	host, inProcess := requestauth.HostPrincipalFromContext(r.Request.Context())
	return inProcess && host.Invoker != nil && who.Issuer == billingauth.HostIssuer && who.Invoker == *host.Invoker
}

// erasureHold reports whether the hold the path names is the admitted
// merchant's erasure hold.
func erasureHold(r *httprequest.Request) bool {
	staff, ok := r.Staff()
	id, err := billing.ParseDestructiveHoldID(r.Param("id"))
	if !ok || err != nil || r.State == nil || r.State.DestructiveAdmissions == nil {
		return false
	}
	hold, err := r.State.DestructiveAdmissions.GetHold(r.Request.Context(), staff.Merchant.UUID(), id.UUID())
	return err == nil && hold.Scope == destructive.ScopeErasure
}
