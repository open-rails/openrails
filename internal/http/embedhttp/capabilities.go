package embedhttp

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/internal/http/routesurface"
)

// Capabilities is the capabilities section of GET /v1/config.
type Capabilities = billing.Capabilities

// CapabilitiesFor reports the staff route groups perms mounts, whether the
// programmatic routes are, the features the runtime's configuration enables,
// and the assembly's extra features.
func CapabilitiesFor(rt *app.Runtime, perms routes.Permissions, programmatic bool, providerRoutes routesurface.ProviderRoutes, extra map[string]bool) Capabilities {
	config, catalog, metrics := perms.MerchantConfig != "", perms.Catalog != "", perms.Metrics != ""
	groups := map[string]bool{string(routes.Admin): perms.AdminRead != "", string(routes.CatalogAdmin): catalog, string(routes.MerchantConfig): config, string(routes.Metrics): metrics, string(routes.App): programmatic}
	caps := Capabilities{RouteGroups: groups, Features: map[string]bool{
		"stripe_billing_portal":          providerRoutes.StripePortal,
		"solana_one_time_payments":       providerRoutes.Solana,
		"solana_subscription_management": providerRoutes.SolanaSigning,
		"provider_credential_writes":     config && providerRoutes.SecretWrite,
		"api_host":                       config && rt != nil && rt.Merchants != nil,
		"catalog_copilot":                catalog && rt != nil && rt.CopilotService.Configured(),
		"metrics_ask":                    metrics && rt != nil && rt.DashboardService.AskConfigured(),
		"dashboard_generation":           metrics && rt != nil && rt.DashboardService.NLConfigured(),
	}}
	for name, on := range extra {
		caps.Features[name] = on
	}
	return caps
}
