package embedhttp

// RouteSet names a mountable billing HTTP route group.
type RouteSet string

type CredentialMode string

const (
	// RouteSetCheckout mounts buyer-facing products, prices, config, and checkout routes.
	RouteSetCheckout RouteSet = "checkout"
	// RouteSetCustomer mounts customer-facing billing routes.
	RouteSetCustomer RouteSet = "customer"
	// RouteSetMerchantAdmin mounts human merchant-admin customer/support routes.
	RouteSetMerchantAdmin RouteSet = "merchant_admin"
	// RouteSetMerchantSettings mounts provider secrets, catalog pushes, and merchant config routes.
	RouteSetMerchantSettings RouteSet = "merchant_settings"
	// RouteSetMerchantAPI mounts host-internal service/API-key routes.
	RouteSetMerchantAPI RouteSet = "merchant_api"
	// RouteSetWebhooks mounts merchant-scoped inbound webhook routes.
	RouteSetWebhooks RouteSet = "webhooks"

	CredentialModeFixed   CredentialMode = "fixed_credentials"
	CredentialModeMutable CredentialMode = "mutable_credentials"
)

// EmbeddedDefaultRouteSets is the default embedded HTTP surface. It excludes
// host-internal merchant API routes because embedded hosts normally call the
// in-process service facade instead of looping through HTTP.
var EmbeddedDefaultRouteSets = []RouteSet{
	RouteSetCheckout,
	RouteSetCustomer,
	RouteSetMerchantAdmin,
	RouteSetWebhooks,
}

// StandaloneDefaultRouteSets is the full standalone HTTP surface, including
// host-internal merchant API routes.
var StandaloneDefaultRouteSets = []RouteSet{
	RouteSetCheckout,
	RouteSetCustomer,
	RouteSetMerchantAdmin,
	RouteSetMerchantSettings,
	RouteSetMerchantAPI,
	RouteSetWebhooks,
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
