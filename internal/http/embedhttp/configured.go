package embedhttp

import (
	"context"
	"fmt"

	"net/http"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/http/routesurface"
	"github.com/open-rails/openrails/internal/merchanttarget"
)

// ValidateHTTPConfig checks that every published group has the authority it needs.
func ValidateHTTPConfig(cfg *config.HTTPConfig, auth *billingauth.Integration) error {
	if cfg == nil {
		return nil
	}
	if err := validateCustomerRoutes(cfg.CustomerRoutes, auth); err != nil {
		return err
	}
	if cfg.Checkout && (auth == nil || auth.Authentication == nil) {
		return fmt.Errorf("openrails HTTP: Checkout requires Deps.Authenticate")
	}
	if (cfg.MerchantAdmin || cfg.Catalog || cfg.MerchantConfig || cfg.MerchantAPI) && (auth == nil || auth.Authentication == nil || auth.Authorization == nil) {
		return fmt.Errorf("openrails HTTP: management surfaces require Deps.Authenticate and Deps.Authorize")
	}
	return nil
}

func routeSets(cfg config.HTTPConfig) []RouteSet {
	sets := []RouteSet{RouteSetWebhooks}
	for _, v := range []struct {
		enabled bool
		set     RouteSet
	}{
		{cfg.Checkout, RouteSetCheckout},
		{cfg.MerchantAdmin, RouteSetMerchantAdmin}, {cfg.Catalog, RouteSetCatalog},
		{cfg.MerchantConfig, RouteSetMerchantConfig}, {cfg.MerchantAPI, RouteSetMerchantAPI},
	} {
		if v.enabled {
			sets = append(sets, v.set)
		}
	}
	return sets
}

// ConfiguredRoutes is the shared configured HTTP assembly used by native adapters.
func ConfiguredRoutes(a *app.App, policy *config.HTTPConfig) (*router.Table, error) {
	if a == nil || a.Runtime == nil {
		return nil, fmt.Errorf("openrails HTTP: runtime is not initialized")
	}
	if policy == nil {
		return &router.Table{}, nil
	}
	cfg := *policy
	if err := ValidateHTTPConfig(&cfg, a.Runtime.Auth); err != nil {
		return nil, err
	}
	asm := FromApp(a)
	if a.Runtime.Auth != nil {
		asm.Authenticator = integrationAuthenticator{auth: a.Runtime.Auth}
		asm.Gate = integrationGate{auth: a.Runtime.Auth, runtime: a.Runtime}
	}
	return buildConfiguredRoutes(a, cfg, asm)
}

func buildConfiguredRoutes(a *app.App, cfg config.HTTPConfig, asm *Assembler) (*router.Table, error) {
	active := routeSets(cfg)
	providers, err := ConfiguredProviderRoutes(context.Background(), a.Runtime, cfg.Checkout || len(cfg.CustomerRoutes) > 0)
	if err != nil {
		return nil, err
	}
	// Generic callbacks remain registered as API-owned accounts are added after
	// startup. Request-time account/signature verification is authoritative.
	providers.Webhooks = true
	capabilities := configuredCapabilities(cfg, providers)
	table := asm.NewRoutes(Options{RouteSets: withoutRouteSet(active, RouteSetCustomer), AdvertiseRouteSets: active, ProviderRoutes: &providers, Capabilities: &capabilities})
	extra, err := BuildCustomerRoutes(a, cfg.CustomerRoutes, a.Runtime.Auth)
	if err != nil {
		return nil, err
	}
	for _, entry := range extra.Entries {
		entry.Path = "/billing" + entry.Path
		table.Entries = append(table.Entries, entry)
	}
	router.AddMerchantSelectorRoutes(table, "/billing", func(ctx context.Context, r *http.Request) (billingauth.Target, error) {
		return merchanttarget.Resolve(ctx, r, a.Runtime.Merchants, a.Runtime.ConfiguredMerchant(), "")
	})
	return table, nil
}

func configuredCapabilities(cfg config.HTTPConfig, providers routesurface.ProviderRoutes) Capabilities {
	active := routeSets(cfg)
	fullCustomer, solanaManagement := false, false
	for _, exposure := range cfg.CustomerRoutes {
		fullCustomer = fullCustomer || exposure.Scope == config.CustomerSelfService
		solanaManagement = solanaManagement || exposure.Scope == config.CustomerSelfService || exposure.Scope == config.CustomerBillingManagement
	}
	if len(cfg.CustomerRoutes) > 0 {
		active = append(active, RouteSetCustomer)
	}
	caps := buildCapabilities(active, providers)
	caps.Features["stripe_billing_portal"] = fullCustomer && providers.StripePortal
	caps.Features["solana_subscription_management"] = solanaManagement && providers.SolanaSigning
	return caps
}
