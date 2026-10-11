package embedhttp

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/internal/http/routesurface"
)

// Capabilities is the capabilities section of GET /v1/config.
type Capabilities = billing.Capabilities

// CapabilitiesFor reports the route groups that are on, the features the
// runtime's configuration enables, and the assembly's extra features.
func CapabilitiesFor(rt *app.Runtime, on config.RouteGroups, providerRoutes routesurface.ProviderRoutes, extra map[string]bool) Capabilities {
	config, catalog, metrics := on.MerchantConfig, on.Catalog, on.Metrics
	groups := map[string]bool{string(routes.Admin): on.Admin, string(routes.CatalogAdmin): catalog, string(routes.MerchantConfig): config, string(routes.Metrics): metrics, string(routes.App): on.Programmatic}
	caps := Capabilities{RouteGroups: groups, Features: map[string]bool{
		"stripe_billing_portal":          providerRoutes.StripePortal,
		"solana_one_time_payments":       providerRoutes.Solana,
		"solana_subscription_management": providerRoutes.SolanaSigning,
		"merchant_config_edits":          config && rt.MerchantConfigEditable(),
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
