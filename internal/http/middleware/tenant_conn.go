package middleware

import (
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db"
)

// TenantDBConn pins a tenant-scoped database connection for the request and sets
// the `app.tenant_id` GUC on it, so every tenant-owned query the request issues
// through db.Q(ctx) is constrained by the migration-050 RLS policies (issue
// #227). It MUST be mounted AFTER the tenant has been resolved onto the request
// context (ResolveTenant for single-tenant; OATRequired / DelegatedSelfRequired
// for the multi-tenant service/self/tenant-admin groups, which override the
// tenant) — otherwise the connection would be pinned to the wrong tenant.
//
// The connection (not a transaction) is held for the request and released at the
// end, with the GUC reset so it returns to the pool clean. When the deployment
// connects as a privileged role (self-hosted single-tenant), RLS is a no-op and
// this simply scopes to the default tenant; when it connects as openrails_app
// (managed multi-tenant), this is what makes isolation actually enforce.
func TenantDBConn(database *db.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		if database == nil {
			c.Next()
			return
		}
		ctx, release, err := database.WithTenantConn(c.Request.Context())
		if err != nil {
			// Acquiring/scoping the connection failed: fail the request rather than
			// run it on an unscoped connection (which, under openrails_app, would be
			// fail-closed anyway).
			log.WithError(err).Error("tenant db connection setup failed")
			c.AbortWithStatus(500)
			return
		}
		defer release()
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}
