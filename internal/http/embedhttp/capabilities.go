package embedhttp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"

	"github.com/open-rails/openrails/internal/http/routesurface"
)

// Capabilities is the public, static billing feature-discovery response. It
// mirrors AuthKit's hand-written capabilities document (NOT OpenAPI — nothing is
// generated or reflected from the route table). Today it advertises route-group
// availability; future capability sections fold in alongside RouteGroups.
type Capabilities struct {
	// RouteGroups reports every known route group (AllRouteSets) as on/off for
	// this deployment, so a client can ask `route_groups.payment_providers`
	// without knowing the closed-world set.
	RouteGroups map[RouteSet]bool `json:"route_groups"`
	// Features reports provider-specific actions available on the mounted HTTP
	// surface. Provider support alone does not expose an action.
	Features map[string]bool `json:"features"`
}

// buildCapabilities reports each known route group as on/off against the resolved
// active selection. Ranging AllRouteSets is the zero-toil part: a new RouteSet
// appears here automatically.
func buildCapabilities(active []RouteSet, providerRoutes routesurface.ProviderRoutes) Capabilities {
	on := make(map[RouteSet]bool, len(active))
	for _, rs := range active {
		on[rs] = true
	}
	groups := make(map[RouteSet]bool, len(AllRouteSets))
	for _, rs := range AllRouteSets {
		groups[rs] = on[rs]
	}
	return Capabilities{RouteGroups: groups, Features: map[string]bool{
		"stripe_billing_portal":          groups[RouteSetCustomer] && providerRoutes.StripePortal,
		"solana_one_time_payments":       groups[RouteSetCheckout] && providerRoutes.Solana,
		"solana_subscription_management": groups[RouteSetCustomer] && providerRoutes.SolanaSigning,
		"provider_credential_writes":     groups[RouteSetMerchantConfig] && providerRoutes.SecretWrite,
	}}
}

// CapabilitiesHandler returns the public GET handler for the capability document.
// The active selection is fixed at mount, so the body + ETag are precomputed.
// Mirrors AuthKit's handleCapabilitiesGET: ETag + Cache-Control: public,
// max-age=300, plus standard If-None-Match → 304.
func CapabilitiesHandler(active []RouteSet, providerRouteOpts ...routesurface.ProviderRoutes) http.Handler {
	providerRoutes := routesurface.AllProviderRoutes()
	if len(providerRouteOpts) > 0 {
		providerRoutes = providerRouteOpts[0]
	}
	return capabilitiesHandler(buildCapabilities(active, providerRoutes))
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
