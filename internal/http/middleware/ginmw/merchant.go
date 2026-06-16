package ginmw

import (
	"github.com/gin-gonic/gin"
	"github.com/open-rails/openrails/pkg/merchant"
)

// ResolveMerchant pins the engine's CONSTRUCTION-TIME merchant onto the request
// context BEFORE authorization and before any merchant-owned DB access (#223,
// #336). An OpenRails engine/client is bound to a single merchant at construction
// (configured for both standalone and embedded — there is NO default merchant),
// so `configured` is that id.
//
// If `configured` is zero, NOTHING is pinned: downstream merchant.Require then
// fails, so a missing merchant is a hard error rather than a silent default.
// Per-request multi-merchant resolution (host/path/token, #222), when added,
// layers on top by pinning the resolved merchant instead.
func ResolveMerchant(configured merchant.ID) gin.HandlerFunc {
	return func(c *gin.Context) {
		if configured.IsZero() {
			c.Next()
			return
		}
		ctx := merchant.WithID(c.Request.Context(), configured)
		c.Request = c.Request.WithContext(ctx)
		c.Set("openrails.merchant_id", configured)
		c.Next()
	}
}
