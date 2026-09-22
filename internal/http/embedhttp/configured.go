package embedhttp

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/http/routesurface"
	"github.com/open-rails/openrails/internal/merchanttarget"
	"github.com/open-rails/openrails/pkg/billingauth"
	"net/http"
)

// HTTPConfig declares externally accessible HTTP capabilities. Nil Options.HTTP
// disables HTTP entirely. A non-nil configuration includes capability discovery
// and provider callbacks; accepting a callback still requires an armed account
// and valid provider verification. Management and buyer surfaces are opt-in and
// independent of in-process Client access and its catalog-scoped views.
type HTTPConfig struct {
	// Standalone selects the attached control plane's billing, identity and console
	// surface. Health endpoints remain the host's responsibility.
	Standalone     bool
	CustomerRoutes []CustomerRoutesConfig

	Checkout         bool
	MerchantAdmin    bool
	Catalog          bool
	PaymentProviders bool
	MerchantAPI      bool
}

func ValidateHTTPConfig(cfg *HTTPConfig, auth *billingauth.Integration) error {
	if cfg == nil {
		return nil
	}
	if err := validateCustomerRoutes(cfg.CustomerRoutes, auth); err != nil {
		return err
	}
	if cfg.Standalone {
		if cfg.Checkout || cfg.MerchantAdmin || cfg.Catalog || cfg.PaymentProviders || cfg.MerchantAPI {
			return fmt.Errorf("openrails HTTP: Standalone cannot be combined with embedded surface options")
		}
		return nil
	}
	if cfg.Checkout && (auth == nil || auth.Authentication == nil) {
		return fmt.Errorf("openrails HTTP: Checkout requires Options.Auth.Authentication")
	}
	if (cfg.MerchantAdmin || cfg.Catalog || cfg.PaymentProviders || cfg.MerchantAPI) && (auth == nil || auth.Authentication == nil || auth.Authorization == nil) {
		return fmt.Errorf("openrails HTTP: management surfaces require Options.Auth authentication and authorization")
	}
	return nil
}

func (cfg HTTPConfig) routeSets() []RouteSet {
	sets := []RouteSet{RouteSetWebhooks}
	for _, v := range []struct {
		enabled bool
		set     RouteSet
	}{
		{cfg.Checkout, RouteSetCheckout},
		{cfg.MerchantAdmin, RouteSetMerchantAdmin}, {cfg.Catalog, RouteSetCatalog},
		{cfg.PaymentProviders, RouteSetPaymentProviders}, {cfg.MerchantAPI, RouteSetMerchantAPI},
	} {
		if v.enabled {
			sets = append(sets, v.set)
		}
	}
	return sets
}

// ConfiguredRoutes is the shared configured HTTP assembly used by native adapters.
func ConfiguredRoutes(a *app.App, policy *HTTPConfig) (*router.Table, error) {
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

func buildConfiguredRoutes(a *app.App, cfg HTTPConfig, asm *Assembler) (*router.Table, error) {
	active := cfg.routeSets()
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

func configuredCapabilities(cfg HTTPConfig, providers routesurface.ProviderRoutes) Capabilities {
	active := cfg.routeSets()
	fullCustomer, solanaManagement := false, false
	for _, exposure := range cfg.CustomerRoutes {
		fullCustomer = fullCustomer || exposure.Scope == CustomerSelfService
		solanaManagement = solanaManagement || exposure.Scope == CustomerSelfService || exposure.Scope == CustomerBillingManagement
	}
	if len(cfg.CustomerRoutes) > 0 {
		active = append(active, RouteSetCustomer)
	}
	caps := buildCapabilities(active, providers)
	caps.Features["stripe_billing_portal"] = fullCustomer && providers.StripePortal
	caps.Features["solana_subscription_management"] = solanaManagement && providers.SolanaSigning
	return caps
}
