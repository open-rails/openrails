package embedhttp

// RouteSet names a mountable billing HTTP route group.
type RouteSet string

const (
	// RouteSetCheckout mounts buyer-facing products, prices, config, and checkout routes.
	RouteSetCheckout RouteSet = "checkout"
	// RouteSetCustomer mounts customer-facing billing routes.
	RouteSetCustomer RouteSet = "customer"
	// RouteSetMerchantAdmin mounts human merchant-admin customer/support routes.
	RouteSetMerchantAdmin RouteSet = "merchant_admin"
	// RouteSetCatalog mounts merchant catalog routes.
	RouteSetCatalog RouteSet = "catalog"
	// RouteSetPaymentProviders mounts provider config and secret routes.
	RouteSetPaymentProviders RouteSet = "payment_providers"
	// RouteSetMerchantAPI mounts host-internal service/API-key routes.
	RouteSetMerchantAPI RouteSet = "merchant_api"
	// RouteSetWebhooks mounts merchant-scoped inbound webhook routes.
	RouteSetWebhooks RouteSet = "webhooks"
)

// EmbeddedDefaultRouteSets is the default embedded HTTP surface. It excludes
// host-internal merchant API routes because embedded hosts normally call the
// in-process service facade instead of looping through HTTP.
var EmbeddedDefaultRouteSets = []RouteSet{
	RouteSetCheckout,
	RouteSetCustomer,
	RouteSetMerchantAdmin,
	RouteSetCatalog,
	RouteSetWebhooks,
}

// StandaloneDefaultRouteSets is the full standalone HTTP surface, including
// host-internal merchant API routes.
var StandaloneDefaultRouteSets = []RouteSet{
	RouteSetCheckout,
	RouteSetCustomer,
	RouteSetMerchantAdmin,
	RouteSetCatalog,
	RouteSetPaymentProviders,
	RouteSetMerchantAPI,
	RouteSetWebhooks,
}

// AllRouteSets is the canonical universe of mountable route groups, in stable
// order. Adding a new RouteSet means appending it here once: capability
// discovery ranges over this list, so nothing else needs editing.
var AllRouteSets = []RouteSet{
	RouteSetCheckout,
	RouteSetCustomer,
	RouteSetMerchantAdmin,
	RouteSetCatalog,
	RouteSetPaymentProviders,
	RouteSetMerchantAPI,
	RouteSetWebhooks,
}

// ResolveRouteSets normalizes a selection: nil/empty → EmbeddedDefaultRouteSets,
// empty strings dropped, original order preserved, deduped.
func ResolveRouteSets(sets []RouteSet) []RouteSet {
	if len(sets) == 0 {
		sets = EmbeddedDefaultRouteSets
	}
	seen := make(map[RouteSet]bool, len(sets))
	out := make([]RouteSet, 0, len(sets))
	for _, rs := range sets {
		if rs == "" || seen[rs] {
			continue
		}
		seen[rs] = true
		out = append(out, rs)
	}
	return out
}

func defaultRouteSets() []RouteSet {
	return append([]RouteSet(nil), EmbeddedDefaultRouteSets...)
}

func routeSetMap(routeSets []RouteSet) map[RouteSet]bool {
	if len(routeSets) == 0 {
		routeSets = defaultRouteSets()
	}
	out := make(map[RouteSet]bool, len(routeSets))
	for _, routeSet := range routeSets {
		if routeSet == "" {
			continue
		}
		out[routeSet] = true
	}
	return out
}
