package policy

import (
	"net/http"
	"strings"

	"github.com/uptrace/bun"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/pkg/billingauth"
)

// userContext reads the authenticated UserContext from the request, working on
// both transports: it checks r.Get (gin-side stash) and the request context
// (net/http-side, set by billingauth.SetUserContext).
func userContext(r *request.Request) (billingauth.UserContext, bool) {
	if v, ok := r.Get("billing.user_context"); ok {
		if uc, ok := v.(billingauth.UserContext); ok {
			return uc, true
		}
	}
	if r.Request != nil {
		return billingauth.FromContext(r.Request.Context())
	}
	return billingauth.UserContext{}, false
}

// OperatorAdminRequiredMW is the framework-neutral analogue of
// OperatorAdminRequired (issue #282). Same authority model: operator-org slug +
// admin-equivalent org roles ONLY (no global-admin fallback). `db` is unused and
// retained for call-site symmetry; pass nil.
func OperatorAdminRequiredMW(cfg *config.Config, db bun.IDB) router.Middleware {
	return func(next router.Handler) router.Handler {
		return func(r *request.Request) {
			uc, ok := userContext(r)
			if !ok || uc.UserID == "" {
				r.AbortJSON(http.StatusUnauthorized, "authentication required")
				return
			}
			if cfg == nil || !cfg.Auth.OperatorOrgEnabled() {
				log.WithField("user_id", uc.UserID).
					Warn("admin access denied: operator org not configured; global-admin fallback removed (#221/#224)")
				r.AbortJSON(http.StatusForbidden, "admin_required")
				return
			}
			operatorSlug := strings.TrimSpace(cfg.Auth.OperatorOrgSlug)
			if strings.TrimSpace(uc.Org) == "" {
				log.WithField("user_id", uc.UserID).Warn("operator admin denied: org claim missing")
				r.AbortJSON(http.StatusForbidden, "operator_org_required")
				return
			}
			if !strings.EqualFold(strings.TrimSpace(uc.Org), operatorSlug) {
				log.WithFields(log.Fields{
					"user_id":      uc.UserID,
					"caller_org":   uc.Org,
					"operator_org": operatorSlug,
				}).Warn("operator admin denied: org mismatch")
				r.AbortJSON(http.StatusForbidden, "operator_org_mismatch")
				return
			}
			adminRoles := cfg.Auth.EffectiveOperatorOrgAdminRoles()
			if !uc.HasAnyOrgRole(adminRoles...) {
				log.WithFields(log.Fields{
					"user_id":    uc.UserID,
					"org":        uc.Org,
					"want_roles": adminRoles,
				}).Warn("operator admin denied: required org role missing")
				r.AbortJSON(http.StatusForbidden, "operator_org_role_required")
				return
			}
			next(r)
		}
	}
}

// OperatorPermissionRequiredMW is the framework-neutral analogue of
// OperatorPermissionRequired (issue #282). It evaluates a LIVE operator-org
// permission. A nil checker is a config error and fails closed with 500.
func OperatorPermissionRequiredMW(checker OperatorPermissionChecker, perm string) router.Middleware {
	return func(next router.Handler) router.Handler {
		return func(r *request.Request) {
			uc, ok := userContext(r)
			if !ok || uc.UserID == "" {
				r.AbortJSON(http.StatusUnauthorized, "authentication required")
				return
			}
			if checker == nil {
				log.Error("operator permission middleware misconfigured: nil checker")
				r.AbortJSON(http.StatusInternalServerError, "authorization unavailable")
				return
			}
			allowed, err := checker.HasOperatorPermission(r.Request.Context(), uc.Org, uc.UserID, perm)
			if err != nil {
				log.WithError(err).Error("failed to evaluate operator permission")
				r.AbortJSON(http.StatusInternalServerError, "failed to check permission")
				return
			}
			if !allowed {
				log.WithFields(log.Fields{"user_id": uc.UserID, "permission": perm}).
					Warn("operator permission denied")
				r.AbortJSON(http.StatusForbidden, "operator_permission_required")
				return
			}
			next(r)
		}
	}
}
