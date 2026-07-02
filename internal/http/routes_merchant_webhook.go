package server

import (
	"net/http"

	"github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
)

// registerMerchantWebhookRoutes mounts the merchant-scoped webhook surface
// (/v1/merchants/:merchant/webhooks/:provider, issue #529). The handler resolves
// the merchant from the URL slug, loads THAT merchant's signing secret, and
// verifies the signature AFTER merchant resolution (the router is not the trust
// boundary; the signature is). Both deployments mount the SAME neutral
// httphandlers.MerchantWebhook (Stripe + NMI-backed rails + CCBill).
func (s *Server) registerMerchantWebhookRoutes(mux *http.ServeMux) {
	httproutes.RegisterMerchantWebhookRoutes(router.NewMuxRecorded(mux, StandaloneV1Prefix, s.runtime, s.recordRoute), s.runtime)
}
