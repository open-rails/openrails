package ginmw

import (
	"github.com/gin-gonic/gin"
	"github.com/open-rails/openrails/pkg/merchant"
)

// ResolveTenant pins the engine's CONSTRUCTION-TIME tenant onto the request
// context BEFORE authorization and before any tenant-owned DB access (#223,
// #336). An OpenRails engine/client is bound to a single tenant at construction
// (configured for both standalone and embedded — there is NO default tenant),
// so `configured` is that id.
//
// If `configured` is zero, NOTHING is pinned: downstream merchant.Require then
// fails, so a missing tenant is a hard error rather than a silent default.
// Per-request multi-tenant resolution (host/path/token, #222), when added,
// layers on top by pinning the resolved tenant instead.
func ResolveTenant(configured merchant.ID) gin.HandlerFunc {
	return func(c *gin.Context) {
		if configured.IsZero() {
			c.Next()
			return
		}
		ctx := merchant.WithID(c.Request.Context(), configured)
		c.Request = c.Request.WithContext(ctx)
		c.Set("openrails.tenant_id", configured)
		c.Next()
	}
}
