package policy

import (
	"net/http"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/pkg/billingauth"
)

// userContext reads the authenticated UserContext from the request, working on
// both transports: it checks r.Get (gin-side stash) and the request context
// (net/http-side, set by billingauth.SetUserContext).
func userContext(r *request.Request) (billingauth.UserContext, bool) {
	if v, ok := r.Get("openrails.user_context"); ok {
		if uc, ok := v.(billingauth.UserContext); ok {
			return uc, true
		}
	}
	if r.Request != nil {
		return billingauth.FromContext(r.Request.Context())
	}
	return billingauth.UserContext{}, false
}

// AdminPermissionRequiredMW is the framework-neutral analogue of
// AdminPermissionRequired (#282/#312). HARDCUT: admin authority is the live
// openrails:admin permission held in the CALLER'S OWN tenant — there is no
// claim-based operator-tenant gate. A nil checker is a config error and fails
// closed with 500: a deployment that mounts admin routes must wire the live
// permission checker (the control plane).
func AdminPermissionRequiredMW(checker AdminPermissionChecker, perm string) router.Middleware {
	return func(next router.Handler) router.Handler {
		return func(r *request.Request) {
			uc, ok := userContext(r)
			if !ok || uc.UserID == "" {
				r.AbortJSON(http.StatusUnauthorized, "authentication required")
				return
			}
			if checker == nil {
				log.Error("admin permission middleware misconfigured: nil checker")
				r.AbortJSON(http.StatusInternalServerError, "authorization unavailable")
				return
			}
			allowed, err := checker.HasAdminPermission(r.Request.Context(), uc.Org, uc.UserID, perm)
			if err != nil {
				log.WithError(err).Error("failed to evaluate admin permission")
				r.AbortJSON(http.StatusInternalServerError, "failed to check permission")
				return
			}
			if !allowed {
				log.WithFields(log.Fields{"user_id": uc.UserID, "permission": perm}).
					Warn("admin permission denied")
				r.AbortJSON(http.StatusForbidden, "admin_permission_required")
				return
			}
			next(r)
		}
	}
}
