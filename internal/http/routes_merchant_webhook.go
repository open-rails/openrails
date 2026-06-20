package server

import (
	"github.com/gin-gonic/gin"

	"github.com/open-rails/openrails/internal/http/router/ginrouter"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
)

// registerMerchantWebhookRoutes mounts the merchant-scoped webhook surface
// (/v1/merchants/:merchant/webhooks/:provider, issue #529) — the canonical inbound
// webhook surface for every deployment. The handler resolves the merchant from the
// URL slug, loads THAT merchant's signing secret, and verifies the signature AFTER
// merchant resolution (the router is not the trust boundary; the signature is).
//
// Both the standalone gin server and the embedded mux mount the SAME neutral
// httphandlers.MerchantWebhook (Stripe + NMI/Mobius + CCBill), so the two
// deployments cannot drift. This replaced a gin-only reimplementation that handled
// Stripe ONLY — it silently rejected NMI/CCBill on the standalone per-merchant
// surface while the embedded mux (already on the neutral handler) accepted them.
func (s *Server) registerMerchantWebhookRoutes(e *gin.Engine) {
	api := e.Group(StandaloneV1Prefix)
	httproutes.RegisterMerchantWebhookRoutes(ginrouter.New(api, s.runtime), s.runtime)
}
