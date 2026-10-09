package embedhttp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/http/routesurface"
)

// Capabilities is GET /v1/capabilities.
type Capabilities = billing.Capabilities

// buildCapabilities reports each known route group as on/off against the
// active selection, and the features the runtime's configuration enables.
func buildCapabilities(rt *app.Runtime, active []RouteSet, providerRoutes routesurface.ProviderRoutes) Capabilities {
	on := make(map[RouteSet]bool, len(active))
	for _, rs := range active {
		on[rs] = true
	}
	groups := make(map[string]bool, len(AllRouteSets))
	for _, rs := range AllRouteSets {
		groups[string(rs)] = on[rs]
	}
	merchant, config := groups[string(RouteSetMerchant)], groups[string(RouteSetMerchantConfig)]
	return Capabilities{RouteGroups: groups, Features: map[string]bool{
		"stripe_billing_portal":          groups[string(RouteSetCustomer)] && providerRoutes.StripePortal,
		"solana_one_time_payments":       groups[string(RouteSetCheckout)] && providerRoutes.Solana,
		"solana_subscription_management": groups[string(RouteSetCustomer)] && providerRoutes.SolanaSigning,
		"provider_credential_writes":     config && providerRoutes.SecretWrite,
		"api_host":                       config && rt != nil && rt.Merchants != nil,
		"catalog_copilot":                merchant && rt != nil && rt.CopilotService.Configured(),
		"metrics_ask":                    merchant && rt != nil && rt.DashboardService.AskConfigured(),
		"dashboard_generation":           config && rt != nil && rt.DashboardService.NLConfigured(),
	}}
}

// CapabilitiesHandler returns the public GET handler for the capability document.
// The active selection is fixed at mount, so the body + ETag are precomputed.
// ETag + Cache-Control: public, max-age=300, and If-None-Match → 304.
func CapabilitiesHandler(rt *app.Runtime, active []RouteSet, providerRoutes routesurface.ProviderRoutes, extra map[string]bool) http.Handler {
	caps := buildCapabilities(rt, active, providerRoutes)
	for name, on := range extra {
		caps.Features[name] = on
	}
	return capabilitiesHandler(caps)
}

func capabilitiesHandler(capabilities Capabilities) http.Handler {
	body, _ := json.Marshal(capabilities)
	etag := `"` + hex.EncodeToString(sha256Sum(body)) + `"`
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=300")
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	})
}

func sha256Sum(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}
