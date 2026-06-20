package ginmw

import (
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/http/response"
)

// MerchantDBConn pins a merchant-scoped database connection for the request and sets
// the `app.merchant_id` GUC on it, so every merchant-owned query the request issues
// through db.Qx(ctx)/Gen(ctx) is constrained by the migration-050 RLS policies (issue
// #227). It MUST be mounted AFTER the merchant has been resolved onto the request
// context (ResolveMerchant for the configured merchant; ServiceCredentialRequired / DelegatedSelfRequired
// for the multi-merchant service/self/delegated-admin groups, which override the
// merchant) — otherwise the connection would be pinned to the wrong merchant.
//
// The connection (not a transaction) is held for the request and released at the
// end, with the GUC reset so it returns to the pool clean. When the deployment
// connects as a privileged role (self-hosted, BYPASSRLS), RLS is a no-op and
// this simply scopes to the configured merchant; when it connects as openrails_app
// (managed multi-merchant), this is what makes isolation actually enforce.
func MerchantDBConn(database *db.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		if database == nil {
			c.Next()
			return
		}
		ctx, release, err := database.WithMerchantConn(c.Request.Context())
		if err != nil {
			// Acquiring/scoping the connection failed: fail the request rather than
			// run it on an unscoped connection (which, under openrails_app, would be
			// fail-closed anyway).
			log.WithError(err).Error("merchant db connection setup failed")
			response.InternalError(c, "merchant database unavailable")
			c.Abort()
			return
		}
		defer release()
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}
