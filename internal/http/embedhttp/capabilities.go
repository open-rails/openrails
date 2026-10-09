package embedhttp

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/internal/http/routesurface"
)

// Capabilities is the capabilities section of GET /v1/config.
type Capabilities = billing.Capabilities

// CapabilitiesFor reports the staff bundles perms mounts, the features the
// runtime's configuration enables, and the assembly's extra features.
func CapabilitiesFor(rt *app.Runtime, perms routes.Permissions, providerRoutes routesurface.ProviderRoutes, extra map[string]bool) Capabilities {
	admin, config := perms.AdminRead != "", perms.MerchantConfig != ""
	groups := map[string]bool{string(routes.Admin): admin, string(routes.CatalogWrite): perms.CatalogWrite != "", string(routes.MerchantConfig): config}
	caps := Capabilities{RouteGroups: groups, Features: map[string]bool{
		"stripe_billing_portal":          providerRoutes.StripePortal,
		"solana_one_time_payments":       providerRoutes.Solana,
		"solana_subscription_management": providerRoutes.SolanaSigning,
		"provider_credential_writes":     config && providerRoutes.SecretWrite,
		"api_host":                       config && rt != nil && rt.Merchants != nil,
		"catalog_copilot":                admin && rt != nil && rt.CopilotService.Configured(),
		"metrics_ask":                    admin && rt != nil && rt.DashboardService.AskConfigured(),
		"dashboard_generation":           config && rt != nil && rt.DashboardService.NLConfigured(),
	}}
	for name, on := range extra {
		caps.Features[name] = on
	}
	return caps
}
