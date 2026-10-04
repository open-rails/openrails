package embedhttp

// RouteSet names a mountable billing HTTP route group.
type RouteSet string

const (
	// RouteSetCheckout mounts buyer-facing products, prices, config, and checkout routes.
	RouteSetCheckout RouteSet = "checkout"
	// RouteSetCustomer mounts customer-facing billing routes.
	RouteSetCustomer RouteSet = "customer"
	// RouteSetMerchant mounts the merchant API and creator-owned catalogs,
	// each route gated by its merchant permission.
	RouteSetMerchant RouteSet = "merchant"
	// RouteSetWebhooks mounts merchant-scoped inbound webhook routes.
	RouteSetWebhooks RouteSet = "webhooks"
)

// AllRouteSets is the canonical universe of mountable route groups, in stable
// order: the default embedded selection and the standalone surface. Capability
// discovery ranges over this list.
var AllRouteSets = []RouteSet{
	RouteSetCheckout,
	RouteSetCustomer,
	RouteSetMerchant,
	RouteSetWebhooks,
}

// ResolveRouteSets normalizes a selection: nil/empty → AllRouteSets,
// empty strings dropped, original order preserved, deduped.
func ResolveRouteSets(sets []RouteSet) []RouteSet {
	if len(sets) == 0 {
		sets = AllRouteSets
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
	return append([]RouteSet(nil), AllRouteSets...)
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
