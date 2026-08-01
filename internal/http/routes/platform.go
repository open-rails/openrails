package routes

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/controlplane"
	httphandlers "github.com/open-rails/openrails/internal/http/handlers"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/pkg/billingauth"
)

// RootPermissionChecker authorizes a user against the singleton ROOT
// permission-group (#721) — the platform-operator tier. Implemented by
// *controlplane.ControlPlane.HasRootPermission.
type RootPermissionChecker interface {
	HasRootPermission(ctx context.Context, userID, perm string) (bool, error)
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

// RegisterPlatformRoutes mounts the cross-merchant platform operator directory
// (#721; openrails-saas #16). STANDALONE ONLY — the platform tier does not
// exist on the embedded surface (an embedded host controls exactly one
// merchant). Explicitly NO platform create/patch, NO hard delete, and NO routes
// touching a merchant's customers/payments/subscriptions: creation stays the
// self-service flow and destructive purge stays the #225 gated path.
func RegisterPlatformRoutes(rr router.Router, rt *app.Runtime, opts PlatformOptions) {
	read := opts.platformPermissionMW(controlplane.PermRootMerchantsRead)
	del := opts.platformPermissionMW(controlplane.PermRootMerchantsDelete)
	restore := opts.platformPermissionMW(controlplane.PermRootMerchantsRestore)
	unlock := opts.platformPermissionMW(controlplane.PermRootAdminRateLimitsUnlock)

	merchants := rr.Group("/merchants")
	merchants.Handle(http.MethodGet, "", h(httphandlers.PlatformListMerchants), read)
	merchants.Handle(http.MethodGet, "/:id", h(httphandlers.PlatformGetMerchant), read)
	merchants.Handle(http.MethodDelete, "/:id", h(httphandlers.PlatformSoftDeleteMerchant), del)
	merchants.Handle(http.MethodPost, "/:id/restore", h(httphandlers.PlatformRestoreMerchant), restore)

	// Root-owner-only manual override. Bounded merchant-directory roles do not
	// hold this distinct permission.
	rr.Handle(http.MethodDelete, "/admin-rate-limit-lockouts/:user_id", h(func(r *httprequest.Request) {
		if opts.AdminLimiter == nil {
			r.AbortJSON(http.StatusServiceUnavailable, "admin rate limit unlock unavailable")
			return
		}
		target := r.Param("user_id")
		if _, err := uuid.Parse(target); err != nil {
			r.AbortJSON(http.StatusBadRequest, "invalid user_id")
			return
		}
		actor, ok := r.UserContext()
		if !ok || actor.UserID == "" {
			r.AbortJSON(http.StatusUnauthorized, "authentication required")
			return
		}
		if err := opts.AdminLimiter.Unlock(r.Request.Context(), target, actor.UserID); err != nil {
			r.AbortJSON(http.StatusServiceUnavailable, "admin rate limit unlock unavailable")
			return
		}
		r.SuccessJSONMessage("admin rate limit lockout cleared")
	}), unlock)
}

// platformPermissionMW authenticates the user session and requires perm in the
// root group: 401 without a valid user credential, 403 without the grant.
func (opts PlatformOptions) platformPermissionMW(perm string) router.Middleware {
	return func(next router.Handler) router.Handler {
		return func(r *httprequest.Request) {
			if opts.Authenticator == nil || opts.Root == nil {
				r.AbortJSON(http.StatusInternalServerError, "authorization unavailable")
				return
			}
			uc, err := opts.Authenticator.Authenticate(r.Request.Context(), r.Request)
			if err != nil {
				r.AbortJSON(http.StatusUnauthorized, billingauth.UnauthenticatedMessage(err))
				return
			}
			if verr := uc.ValidateSubject(); verr != nil {
				r.AbortJSON(http.StatusUnauthorized, verr.Error())
				return
			}
			allowed, err := opts.Root.HasRootPermission(r.Request.Context(), uc.UserID, perm)
			if err != nil {
				r.AbortJSON(http.StatusInternalServerError, "failed to check permission")
				return
			}
			if !allowed {
				r.AbortJSON(http.StatusForbidden, "permission_required")
				return
			}
			r.SetUserContext(uc)
			next(r)
		}
	}
}
