package routes

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"
	auth "github.com/open-rails/helpers/auth"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/http/handlers"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
)

// platformRoutes is the cross-merchant operator directory (#721):
// standalone only, human operator sessions only.
// Deliberately no platform create or patch, no hard delete, and nothing that
// touches a merchant's customers, payments or subscriptions.
var platformRoutes = []Route{
	// #SEC-22: cross-merchant worker health (last_error is another merchant's
	// verbatim job error) lives on the platform tier; the merchant tier keeps
	// the same list with the error text withheld.
	{Method: GET, Path: "/v1/platform/worker-health", Group: Platform, Auth: AuthOperator, Perm: billing.RootWorkerHealthRead, NoConn: true,
		Responses: []Reply{{200, billing.ListPage[billing.WorkerHealth]{}}}, Handler: h(handlers.GetPlatformWorkerHealth)},
	{Method: GET, Path: "/v1/platform/merchants", Group: Platform, Auth: AuthOperator, Perm: billing.RootMerchantsRead, NoConn: true,
		Query: params(pageParams, queryOf(handlers.PlatformMerchantListQuery{})), Responses: []Reply{{200, billing.ListPage[handlers.PlatformMerchant]{}}}, Errors: codes("invalid_cursor"), Handler: h(handlers.PlatformListMerchants)},
	{Method: GET, Path: "/v1/platform/merchants/{id}", Group: Platform, Auth: AuthOperator, Perm: billing.RootMerchantsRead, NoConn: true,
		Responses: []Reply{{200, handlers.PlatformMerchant{}}}, Errors: codes("invalid_param", "resource_not_found"), Handler: h(handlers.PlatformGetMerchant)},
	{Method: DELETE, Path: "/v1/platform/merchants/{id}", Group: Platform, Auth: AuthOperator, Perm: billing.RootMerchantsDelete, NoConn: true,
		Responses: []Reply{{200, handlers.PlatformMerchant{}}}, Errors: codes("invalid_param", "resource_not_found"), Handler: h(handlers.PlatformSoftDeleteMerchant)},
	{Method: POST, Path: "/v1/platform/merchants/{id}/restore", Group: Platform, Auth: AuthOperator, Perm: billing.RootMerchantsRestore, NoConn: true,
		Responses: []Reply{{200, handlers.PlatformMerchant{}}}, Errors: codes("invalid_param", "name_taken", "resource_conflict", "resource_not_found"), Handler: h(handlers.PlatformRestoreMerchant)},
	// Root-owner-only manual override. Bounded merchant-directory roles do not
	// hold this distinct permission.
	{Method: DELETE, Path: "/v1/platform/admin-rate-limit-lockouts/{user_id}", Group: Platform, Auth: AuthOperator, Perm: billing.RootAdminRateLimitsUnlock, NoConn: true,
		Responses: []Reply{{204, nil}}, Errors: codes("authentication_required", "invalid_param", "service_unavailable"), Bind: unlockAdminRateLimit},
}

// RootPermissionChecker authorizes the user r authenticates as against the
// singleton ROOT permission group (#721), the platform-operator tier, with the
// token's session. Implemented by the control plane.
type RootPermissionChecker interface {
	HasRootPermission(ctx context.Context, r *http.Request, perm string) (bool, error)
}

// PlatformOptions wires the /v1/platform/* tier. Deliberately narrower than
// Options: platform routes accept HUMAN operator sessions only (user access
// tokens checked against root-group grants) — no API keys, no delegated or
// host principals, no merchant context.
type PlatformOptions struct {
	Authenticator billingauth.Authenticator
	Root          RootPermissionChecker
	AdminLimiter  AdminRateLimitUnlocker
}

// AdminRateLimitUnlocker is the explicit root-operator override seam for an
// active administrative-operation lockout.
type AdminRateLimitUnlocker interface {
	Unlock(ctx context.Context, userID, actorID string) error
}

// RegisterPlatformRoutes mounts the platform tier on a router rooted at
// /v1/platform. Standalone only: an embedded host controls exactly one
// merchant.
func RegisterPlatformRoutes(rr router.Router, rt *app.Runtime, opts PlatformOptions) {
	env := newEnv(rt, Options{Authenticator: opts.Authenticator})
	env.Root, env.Unlocker = opts.Root, opts.AdminLimiter
	env.mount(rr, "/v1/platform", in(Platform))
}

func unlockAdminRateLimit(e *Env) router.Handler {
	return func(r *httprequest.Request) {
		if e.Unlocker == nil {
			r.AbortCode(billing.CodeServiceUnavailable, "admin rate limit unlock unavailable")
			return
		}
		target := r.Param("user_id")
		if _, err := uuid.Parse(target); err != nil {
			r.AbortAPIError(api.Coded(billing.CodeInvalidParam, "invalid user_id").WithParam("user_id"))
			return
		}
		actor, ok := r.UserContext()
		if !ok || actor.UserID == "" {
			r.AbortCode(billing.CodeAuthenticationRequired, "")
			return
		}
		if err := e.Unlocker.Unlock(r.Request.Context(), target, actor.UserID); err != nil {
			r.AbortCode(billing.CodeServiceUnavailable, "admin rate limit unlock unavailable")
			return
		}
		r.NoContent()
	}
}

// platformPermissionMW authenticates the user session and requires perm in the
// root group: 401 without a valid user credential, 403 without the grant.
func (e *Env) platformPermissionMW(perm string) router.Middleware {
	return func(next router.Handler) router.Handler {
		return func(r *httprequest.Request) {
			if e.Authenticator == nil || e.Root == nil {
				r.AbortCode(billing.CodeInternalError, "authorization unavailable")
				return
			}
			uc, err := e.Authenticator.Authenticate(r.Request.Context(), r.Request)
			if err != nil {
				r.AbortGate(billingauth.Unauthenticated(err))
				return
			}
			if verr := uc.ValidateSubject(); verr != nil {
				r.AbortCode(billing.CodeAuthenticationRequired, verr.Error())
				return
			}
			allowed, err := e.Root.HasRootPermission(r.Request.Context(), r.Request, perm)
			if errors.Is(err, auth.ErrRevoked) || errors.Is(err, billingauth.ErrUnauthenticated) {
				r.AbortGate(billingauth.Unauthenticated(err))
				return
			}
			if err != nil {
				r.AbortCode(billing.CodeInternalError, "failed to check permission")
				return
			}
			if !allowed {
				r.AbortCode(billing.CodePermissionRequired, "")
				return
			}
			r.SetUserContext(uc)
			next(r)
		}
	}
}
