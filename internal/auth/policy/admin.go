package policy

import (
	"context"
	"strings"

	"github.com/doujins-org/ginapi/response"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/pkg/authprovider"
	log "github.com/sirupsen/logrus"
	"github.com/uptrace/bun"
)

func IsAdmin(ctx context.Context, db bun.IDB, userID string) (bool, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return false, nil
	}

	// Always check admin role live in Postgres (immediate revocation).
	return db.NewSelect().
		TableExpr("profiles.user_roles ur").
		Join("JOIN profiles.roles r ON ur.role_id = r.id").
		Where("ur.user_id = ? AND r.slug = 'admin' AND r.deleted_at IS NULL AND EXISTS (SELECT 1 FROM profiles.users u WHERE u.id = ? AND u.deleted_at IS NULL AND u.banned_at IS NULL)", uid, uid).
		Exists(ctx)
}

// IsOperatorAdmin reports whether the given UserContext is an admin under the
// deployment's configured policy:
//
//   - When cfg.Auth.OperatorOrgSlug is empty (single-org / legacy deployments):
//     equivalent to IsAdmin — checks the live `admin` role in profiles.user_roles.
//
//   - When cfg.Auth.OperatorOrgSlug is set (multi-org AuthKit deployments):
//     requires uc.Org to equal the configured operator org slug AND uc.OrgRoles
//     to contain one of cfg.Auth.EffectiveOperatorOrgAdminRoles (case-insensitive).
//     The OpenRails DB is not consulted in this mode — the AuthKit JWT claims
//     are the source of truth, with revocation handled by short JWT TTL.
//
// `db` is only consulted in legacy mode; pass nil in multi-org mode if convenient.
func IsOperatorAdmin(ctx context.Context, cfg *config.Config, db bun.IDB, uc authprovider.UserContext) (bool, error) {
	if cfg != nil && cfg.Auth.OperatorOrgEnabled() {
		operatorSlug := strings.TrimSpace(cfg.Auth.OperatorOrgSlug)
		if !strings.EqualFold(strings.TrimSpace(uc.Org), operatorSlug) {
			return false, nil
		}
		adminRoles := cfg.Auth.EffectiveOperatorOrgAdminRoles()
		return uc.HasAnyOrgRole(adminRoles...), nil
	}
	if uc.UserID == "" {
		return false, nil
	}
	return IsAdmin(ctx, db, uc.UserID)
}

// OperatorAdminRequired ensures the caller is an admin under the configured policy.
//
// Gates all /admin/* routes. Behavior depends on config.Auth.OperatorOrgSlug:
//
//   - Unset (default): legacy global-`admin`-role check against profiles.user_roles.
//     Returns 403 "admin_required" on denial. Use case: cozy.art embedded, doujins
//     / hentai0 self-hosted, any deployment whose AuthKit is single-org.
//
//   - Set: caller must have UserContext.Org equal to the configured slug AND
//     hold one of the configured admin-equivalent roles in UserContext.OrgRoles.
//     Returns 403 "operator_org_required" / "operator_org_mismatch" /
//     "operator_org_role_required" with a structured error code, depending on
//     which condition failed. Use case: tensorhub, hosted OpenRails accessed
//     by callers from a multi-org AuthKit.
//
// In both modes, an unauthenticated caller gets 401 "authentication required".
//
// `cfg` may be nil — that case is treated as legacy mode.
func OperatorAdminRequired(cfg *config.Config, db bun.IDB) gin.HandlerFunc {
	return func(c *gin.Context) {
		uc, ok := authprovider.UserContextFromGin(c)
		if !ok || uc.UserID == "" {
			response.UnauthorizedWithMessage(c, "authentication required")
			c.Abort()
			return
		}

		// Multi-org mode: structured per-condition errors so clients can distinguish.
		if cfg != nil && cfg.Auth.OperatorOrgEnabled() {
			operatorSlug := strings.TrimSpace(cfg.Auth.OperatorOrgSlug)
			if strings.TrimSpace(uc.Org) == "" {
				log.WithField("user_id", uc.UserID).Warn("operator admin denied: org claim missing")
				response.ForbiddenWithMessage(c, "operator_org_required")
				c.Abort()
				return
			}
			if !strings.EqualFold(strings.TrimSpace(uc.Org), operatorSlug) {
				log.WithFields(log.Fields{
					"user_id":      uc.UserID,
					"caller_org":   uc.Org,
					"operator_org": operatorSlug,
				}).Warn("operator admin denied: org mismatch")
				response.ForbiddenWithMessage(c, "operator_org_mismatch")
				c.Abort()
				return
			}
			adminRoles := cfg.Auth.EffectiveOperatorOrgAdminRoles()
			if !uc.HasAnyOrgRole(adminRoles...) {
				log.WithFields(log.Fields{
					"user_id":    uc.UserID,
					"org":        uc.Org,
					"want_roles": adminRoles,
				}).Warn("operator admin denied: required org role missing")
				response.ForbiddenWithMessage(c, "operator_org_role_required")
				c.Abort()
				return
			}
			c.Next()
			return
		}

		// Legacy single-org mode: live DB role check.
		isAdmin, err := IsAdmin(c.Request.Context(), db, uc.UserID)
		if err != nil {
			log.WithError(err).Error("failed to check admin role")
			response.InternalError(c, "failed to check admin role")
			c.Abort()
			return
		}
		if !isAdmin {
			log.WithField("user_id", uc.UserID).Warn("admin access denied")
			response.ForbiddenWithMessage(c, "admin_required")
			c.Abort()
			return
		}
		c.Next()
	}
}
