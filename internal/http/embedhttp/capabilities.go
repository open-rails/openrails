package embedhttp

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/http/routesurface"
)

// Capabilities is the capabilities section of GET /v1/config.
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

// CapabilitiesFor is the capabilities of a mount serving active, plus the
// assembly's extra features.
func CapabilitiesFor(rt *app.Runtime, active []RouteSet, providerRoutes routesurface.ProviderRoutes, extra map[string]bool) Capabilities {
	caps := buildCapabilities(rt, active, providerRoutes)
	for name, on := range extra {
		caps.Features[name] = on
	}
	return caps
}
