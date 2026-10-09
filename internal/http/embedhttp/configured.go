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
	"github.com/open-rails/openrails/internal/merchanttarget"
)

// CustomerProfiles are the customer surfaces a selection mounts: /v1/me,
// then CustomerProfiles. A profile without its own Auth uses Routes.Auth; one
// without a merchant serves the configured one.
func CustomerProfiles(sel config.Routes) []config.CustomerRoutes {
	out := append([]config.CustomerRoutes{{}}, sel.CustomerProfiles...)
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

// ValidateRoutes refuses a selection without the Auth its routes need, or
// with a bundle or customer surface it cannot mount, before anything mounts:
// nothing is ever mounted open.
func ValidateRoutes(sel config.Routes) error {
	if httproutes.IsNilAuth(sel.Auth) {
		return fmt.Errorf("openrails: Routes.Auth is required: the customer routes ask it who is signed in")
	}
	if _, err := RoutePermissions(sel); err != nil {
		return err
	}
	return validateCustomerRoutes(CustomerProfiles(sel))
}

// RoutePermissions is sel's Permissions as the routes check them. It refuses
// AdminWrite or CatalogWrite without AdminRead, and any bundle without
// Routes.Auth: nothing is ever mounted open.
func RoutePermissions(sel config.Routes) (httproutes.Permissions, error) {
	perms := httproutes.Permissions{
		AdminRead:      permissionText(sel.Permissions.AdminRead),
		AdminWrite:     permissionText(sel.Permissions.AdminWrite),
		CatalogWrite:   permissionText(sel.Permissions.CatalogWrite),
		MerchantConfig: permissionText(sel.Permissions.MerchantConfig),
	}
	if err := perms.Validate(); err != nil {
		return perms, err
	}
	if perms != (httproutes.Permissions{}) && httproutes.IsNilAuth(sel.Auth) {
		return perms, fmt.Errorf("openrails: Routes.Permissions need Routes.Auth (its RequirePermission and Sensitive gate every admin route)")
	}
	return perms, nil
}

// permissionText is a permission's text, "" for a nil one.
func permissionText(perm fmt.Stringer) (text string) {
	if perm == nil {
		return ""
	}
	if v := reflect.ValueOf(perm); v.Kind() == reflect.Pointer && v.IsNil() {
		return ""
	}
	return perm.String()
}

// ConfiguredRoutes is the embedded HTTP surface sel selects, under /billing.
func ConfiguredRoutes(a *app.App, sel config.Routes) (*router.Table, error) {
	if a == nil || a.Runtime == nil || a.Config == nil {
		return nil, fmt.Errorf("openrails HTTP: runtime is not initialized")
	}
	if err := ValidateRoutes(sel); err != nil {
		return nil, err
	}
	perms, _ := RoutePermissions(sel)
	profiles := CustomerProfiles(sel)
	asm := FromApp(a)
	asm.Auth = sel.Auth
	providers, err := ConfiguredProviderRoutes(context.Background(), a.Runtime)
	if err != nil {
		return nil, err
	}
	// Generic callbacks remain registered as API-owned accounts are added after
	// startup. Request-time account/signature verification is authoritative.
	providers.Webhooks = true
	table := asm.NewRoutes(Options{Permissions: perms, ProviderRoutes: &providers})
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
