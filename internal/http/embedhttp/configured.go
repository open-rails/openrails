package embedhttp

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/pkg/billingauth"
)

// HTTPConfig declares externally accessible HTTP capabilities. Nil Options.HTTP
// disables HTTP entirely. A non-nil configuration includes capability discovery
// and provider callbacks; accepting a callback still requires an armed account
// and valid provider verification. Management and buyer surfaces are opt-in and
// independent of in-process Client or CatalogClient access.
type HTTPConfig struct {
	// Standalone selects the attached control plane's billing, identity and console
	// surface. Health endpoints remain the host's responsibility.
	Standalone        bool
	CustomerExposures []CustomerHTTPConfig

	// DelegatedAuthenticator verifies customer HTTP credentials. When nil, the
	// embedded runtime's Options.DelegatedAuthenticator is used.
	DelegatedAuthenticator billingauth.DelegatedAuthenticator
	Checkout               bool
	Customer               bool
	MerchantAdmin          bool
	Catalog                bool
	PaymentProviders       bool
	MerchantAPI            bool
	Authenticator          billingauth.Authenticator
	Gate                   billingauth.Gate
}

func ValidateHTTPConfig(cfg *HTTPConfig, delegated billingauth.DelegatedAuthenticator) error {
	if cfg == nil {
		return nil
	}
	if err := validateCustomerExposures(cfg.CustomerExposures); err != nil {
		return err
	}
	if cfg.Standalone {
		if cfg.Checkout || cfg.Customer || cfg.MerchantAdmin || cfg.Catalog || cfg.PaymentProviders || cfg.MerchantAPI || cfg.Authenticator != nil || cfg.Gate != nil || cfg.DelegatedAuthenticator != nil {
			return fmt.Errorf("openrails HTTP: Standalone cannot be combined with embedded surface options")
		}
		return nil
	}
	if cfg.Checkout && cfg.Authenticator == nil {
		return fmt.Errorf("openrails HTTP: Checkout requires HTTP.Authenticator")
	}
	if cfg.Customer && cfg.DelegatedAuthenticator == nil && delegated == nil {
		return fmt.Errorf("openrails HTTP: Customer requires HTTP.DelegatedAuthenticator or Options.DelegatedAuthenticator")
	}
	if (cfg.MerchantAdmin || cfg.Catalog || cfg.PaymentProviders || cfg.MerchantAPI) && cfg.Gate == nil {
		return fmt.Errorf("openrails HTTP: management surfaces require HTTP.Gate")
	}
	return nil
}

func (cfg HTTPConfig) routeSets() []RouteSet {
	sets := []RouteSet{RouteSetWebhooks}
	for _, v := range []struct {
		enabled bool
		set     RouteSet
	}{
		{cfg.Checkout, RouteSetCheckout}, {cfg.Customer, RouteSetCustomer},
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
func ConfiguredRoutes(a *app.App, policy *HTTPConfig, delegated billingauth.DelegatedAuthenticator) (*router.Table, error) {
	if a == nil || a.Runtime == nil {
		return nil, fmt.Errorf("openrails HTTP: runtime is not initialized")
	}
	if policy == nil {
		return &router.Table{}, nil
	}
	cfg := *policy
	if cfg.DelegatedAuthenticator != nil {
		delegated = cfg.DelegatedAuthenticator
	}
	if err := ValidateHTTPConfig(&cfg, delegated); err != nil {
		return nil, err
	}
	asm := FromApp(a)
	asm.Authenticator = cfg.Authenticator
	asm.Gate = cfg.Gate
	active := cfg.routeSets()
	providers, err := ConfiguredProviderRoutes(context.Background(), a.Runtime, cfg.Checkout || cfg.Customer)
	if err != nil {
		return nil, err
	}
	// Generic callbacks remain registered as API-owned accounts are added after
	// startup. Request-time account/signature verification is authoritative.
	providers.Webhooks = true
	table := asm.NewRoutes(Options{RouteSets: withoutRouteSet(active, RouteSetCustomer), AdvertiseRouteSets: active, ProviderRoutes: &providers})
	if cfg.Customer {
		self := NewSelfRoutes(a.Runtime, delegated, &providers, asm.HostResolve)
		table.Entries = append(table.Entries, self.Entries...)
	}
	return table, nil
}
