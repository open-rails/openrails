package embedhttp

import (
	"context"
	"fmt"
	"net/http"
	"reflect"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/internal/http/routesurface"
	"github.com/open-rails/openrails/internal/merchanttarget"
)

// CustomerProfiles are the customer surfaces a selection mounts: Customers
// at /v1/me, then CustomerProfiles. A profile without its own Auth uses
// Routes.Auth; one without a merchant serves the configured one.
func CustomerProfiles(sel config.Routes) []config.CustomerRoutes {
	var out []config.CustomerRoutes
	if sel.Customers != config.CustomersNone {
		out = append(out, config.CustomerRoutes{Scope: sel.Customers})
	}
	out = append(out, sel.CustomerProfiles...)
	for i := range out {
		if out[i].Prefix == "" {
			out[i].Prefix = "/v1/me"
		}
		if httproutes.IsNilAuth(out[i].Auth) {
			out[i].Auth = sel.Auth
		}
	}
	return out
}

// ValidateRoutes refuses a selection whose groups lack the Auth they need,
// before anything mounts: nothing is ever mounted open.
func ValidateRoutes(sel config.Routes, profiles []config.CustomerRoutes, rt *app.Runtime) error {
	if (sel.Merchant || sel.MerchantConfig) && httproutes.IsNilAuth(sel.Auth) {
		return fmt.Errorf("openrails: Routes.Merchant and Routes.MerchantConfig need Routes.Auth (its RequirePermission and Sensitive gate every staff route)")
	}
	return validateCustomerRoutes(profiles, rt)
}

// StaffGuard resolves sel.Guards over the staff routes sel mounts on rt:
// each route's permission. It fails when a mounted staff route has no guard
// or a guard is wrong (httproutes.ResolveGuards).
func StaffGuard(rt *app.Runtime, sel config.Routes) (func(httproutes.Route) string, error) {
	guards := make(map[httproutes.GuardKey]string, len(sel.Guards))
	for key, perm := range sel.Guards {
		guards[httproutes.GuardKey(key)] = permissionText(perm)
	}
	var groups []httproutes.Group
	if sel.Merchant {
		groups = append(groups, httproutes.Merchant)
	}
	if sel.MerchantConfig {
		groups = append(groups, httproutes.MerchantConfig)
	}
	if len(groups) == 0 {
		if len(guards) > 0 {
			return nil, fmt.Errorf("openrails: Routes.Guards guard staff routes; set Routes.Merchant or Routes.MerchantConfig")
		}
		return nil, nil
	}
	return httproutes.ResolveGuards(httproutes.PlanStaffRoutes(rt, httproutes.Options{}, groups...), guards)
}

// permissionText is a guard's permission, "" for a nil one.
func permissionText(perm fmt.Stringer) (text string) {
	if perm == nil {
		return ""
	}
	if v := reflect.ValueOf(perm); v.Kind() == reflect.Pointer && v.IsNil() {
		return ""
	}
	return perm.String()
}

func routeSets(sel config.Routes) []RouteSet {
	sets := []RouteSet{RouteSetWebhooks}
	if sel.Storefront {
		sets = append(sets, RouteSetCheckout)
	}
	if sel.Merchant {
		sets = append(sets, RouteSetMerchant)
	}
	if sel.MerchantConfig {
		sets = append(sets, RouteSetMerchantConfig)
	}
	return sets
}

// ConfiguredRoutes is the embedded HTTP surface sel selects, under /billing.
func ConfiguredRoutes(a *app.App, sel config.Routes) (*router.Table, error) {
	if a == nil || a.Runtime == nil || a.Config == nil {
		return nil, fmt.Errorf("openrails HTTP: runtime is not initialized")
	}
	profiles := CustomerProfiles(sel)
	if err := ValidateRoutes(sel, profiles, a.Runtime); err != nil {
		return nil, err
	}
	asm := FromApp(a)
	asm.Auth = sel.Auth
	active := routeSets(sel)
	providers, err := ConfiguredProviderRoutes(context.Background(), a.Runtime, sel.Storefront || len(profiles) > 0)
	if err != nil {
		return nil, err
	}
	// Generic callbacks remain registered as API-owned accounts are added after
	// startup. Request-time account/signature verification is authoritative.
	providers.Webhooks = true
	capabilities := configuredCapabilities(a.Runtime, active, profiles, providers)
	guard, err := StaffGuard(a.Runtime, sel)
	if err != nil {
		return nil, err
	}
	table := asm.NewRoutes(Options{RouteSets: active, AdvertiseRouteSets: active, ProviderRoutes: &providers, Capabilities: &capabilities, Guard: guard})
	extra, err := BuildCustomerRoutes(a, profiles, nil)
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
