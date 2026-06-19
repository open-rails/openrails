package ginmw

import (
	"github.com/gin-gonic/gin"
	"github.com/open-rails/openrails/pkg/merchant"
)

// ResolveMerchant pins the engine's CONSTRUCTION-TIME merchant onto the request
// context BEFORE authorization and before any merchant-owned DB access (#223,
// #336). `configured` scopes one engine instance to a merchant (embedded hosts run
// one engine per merchant); it is zero in standalone, where the per-credential
// middleware resolves the merchant per request. OpenRails is multi-merchant either
// way — there is NO default merchant, so an unpinned request hard-fails.
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
