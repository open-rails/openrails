package embedhttp

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/http/routesurface"
	"github.com/open-rails/openrails/internal/merchanttarget"
)

// CustomerProfiles are the customer surfaces a selection mounts: Customers
// at /v1/me, then CustomerProfiles, each native one defaulting to merchant.
func CustomerProfiles(sel config.Routes, merchant string) []config.CustomerRoutes {
	var out []config.CustomerRoutes
	if sel.Customers != config.CustomersNone {
		out = append(out, config.CustomerRoutes{Scope: sel.Customers})
	}
	out = append(out, sel.CustomerProfiles...)
	for i := range out {
		if out[i].Prefix == "" {
			out[i].Prefix = "/v1/me"
		}
		if !out[i].Delegated && strings.TrimSpace(out[i].Merchant) == "" {
			out[i].Merchant = merchant
		}
	}
	return out
}

// ValidateRoutes checks that every selected group has the authority it needs,
// before anything mounts.
func ValidateRoutes(sel config.Routes, profiles []config.CustomerRoutes, rt *app.Runtime) error {
	var auth *billingauth.Integration
	if rt != nil {
		auth = rt.Auth
	}
	if sel.CatalogEdits && !sel.Merchant {
		return fmt.Errorf("openrails: Routes.CatalogEdits adds the merchant API's catalog writes; set Routes.Merchant")
	}
	if sel.Merchant && (auth == nil || auth.Authentication == nil || auth.Authorization == nil) {
		return fmt.Errorf("openrails: Routes.Merchant needs Deps.AuthKit with Deps.AuthorityFor, or Deps.Authenticate with Deps.Authorize")
	}
	return validateCustomerRoutes(profiles, rt)
}

func routeSets(sel config.Routes) []RouteSet {
	sets := []RouteSet{RouteSetWebhooks}
	if sel.Storefront {
		sets = append(sets, RouteSetCheckout)
	}
	if sel.Merchant {
		sets = append(sets, RouteSetMerchant)
	}
	return sets
}

// ConfiguredRoutes is the embedded HTTP surface sel selects, under /billing.
func ConfiguredRoutes(a *app.App, sel config.Routes) (*router.Table, error) {
	if a == nil || a.Runtime == nil || a.Config == nil {
		return nil, fmt.Errorf("openrails HTTP: runtime is not initialized")
	}
	profiles := CustomerProfiles(sel, a.Config.Merchant.Slug)
	if err := ValidateRoutes(sel, profiles, a.Runtime); err != nil {
		return nil, err
	}
	asm := FromApp(a)
	if a.Runtime.Auth != nil {
		asm.Authenticator = integrationAuthenticator{auth: a.Runtime.Auth}
		asm.Gate = integrationGate{auth: a.Runtime.Auth, runtime: a.Runtime}
	}
	active := routeSets(sel)
	providers, err := ConfiguredProviderRoutes(context.Background(), a.Runtime, sel.Storefront || len(profiles) > 0)
	if err != nil {
		return nil, err
	}
	// Generic callbacks remain registered as API-owned accounts are added after
	// startup. Request-time account/signature verification is authoritative.
	providers.Webhooks = true
	capabilities := configuredCapabilities(a.Runtime, active, profiles, providers)
	table := asm.NewRoutes(Options{RouteSets: active, AdvertiseRouteSets: active, ProviderRoutes: &providers, Capabilities: &capabilities, CatalogWrites: sel.CatalogEdits})
	extra, err := BuildCustomerRoutes(a, profiles, a.Runtime.Auth)
	if err != nil {
		return nil, err
	}
	for _, entry := range extra.Entries {
		entry.Path = "/billing" + entry.Path
		table.Entries = append(table.Entries, entry)
	}
	router.ResolveMerchantSelectors(table, "/billing", func(ctx context.Context, r *http.Request) (billingauth.Target, error) {
		return merchanttarget.Resolve(ctx, r, a.Runtime.Merchants, a.Runtime.ConfiguredMerchant(), "")
	}, CustomerPrefixes("/billing", profiles)...)
	return table, nil
}

func configuredCapabilities(rt *app.Runtime, active []RouteSet, profiles []config.CustomerRoutes, providers routesurface.ProviderRoutes) Capabilities {
	fullCustomer := false
	for _, profile := range profiles {
		fullCustomer = fullCustomer || profile.Scope == config.CustomerSelfService
	}
	if len(profiles) > 0 {
		active = append(append([]RouteSet(nil), active...), RouteSetCustomer)
	}
	caps := buildCapabilities(rt, active, providers)
	caps.Features["stripe_billing_portal"] = fullCustomer && providers.StripePortal
	return caps
}
