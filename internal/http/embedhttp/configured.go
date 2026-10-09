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

// ValidateRoutes refuses a selection without the Auth its routes need, or
// with a route group it cannot mount, before anything mounts: nothing is ever
// mounted open.
func ValidateRoutes(sel config.Routes) error {
	if httproutes.IsNilAuth(sel.Auth) {
		return fmt.Errorf("openrails: Routes.Auth is required: the customer routes ask it who is signed in")
	}
	_, err := RoutePermissions(sel)
	return err
}

// RoutePermissions is sel's staff route groups as the routes check them:
// each group that is on, with its permission. It refuses a group on without
// its permission, a permission for a group that is off, and any group without
// Routes.Auth: nothing is ever mounted open.
func RoutePermissions(sel config.Routes) (httproutes.Permissions, error) {
	b, p := sel.RouteGroups, sel.Permissions
	perms := httproutes.Permissions{
		AdminRead:      permissionText(p.AdminRead),
		AdminUpdate:    permissionText(p.AdminUpdate),
		Catalog:        permissionText(p.Catalog),
		MerchantConfig: permissionText(p.MerchantConfig),
		Metrics:        permissionText(p.Metrics),
	}
	for _, c := range []struct {
		on         bool
		group      string
		perm, name string
	}{
		{b.Admin, "Admin", perms.AdminRead, "AdminRead"},
		{b.Catalog, "Catalog", perms.Catalog, "Catalog"},
		{b.MerchantConfig, "MerchantConfig", perms.MerchantConfig, "MerchantConfig"},
		{b.Metrics, "Metrics", perms.Metrics, "Metrics"},
	} {
		switch {
		case c.on && c.perm == "":
			return perms, fmt.Errorf("openrails: RouteGroups.%s is on without Permissions.%s", c.group, c.name)
		case !c.on && c.perm != "":
			return perms, fmt.Errorf("openrails: Permissions.%s is given, but RouteGroups.%s is off", c.name, c.group)
		}
	}
	if !b.Admin && perms.AdminUpdate != "" {
		return perms, fmt.Errorf("openrails: Permissions.AdminUpdate is given, but RouteGroups.Admin is off")
	}
	if err := perms.Validate(); err != nil {
		return perms, err
	}
	if (perms != (httproutes.Permissions{}) || b.Programmatic) && httproutes.IsNilAuth(sel.Auth) {
		return perms, fmt.Errorf("openrails: RouteGroups need Routes.Auth (it gates every staff and programmatic route)")
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
	asm := FromApp(a)
	asm.Auth = sel.Auth
	providers, err := ConfiguredProviderRoutes(context.Background(), a.Runtime)
	if err != nil {
		return nil, err
	}
	// Generic callbacks remain registered as API-owned accounts are added after
	// startup. Request-time account/signature verification is authoritative.
	providers.Webhooks = true
	table := asm.NewRoutes(Options{Permissions: perms, ProviderRoutes: &providers, Programmatic: sel.RouteGroups.Programmatic})
	extra, err := BuildCustomerRoutes(a, sel.Auth, nil)
	if err != nil {
		return nil, err
	}
	for _, entry := range extra.Entries {
		entry.Path = "/billing" + entry.Path
		table.Entries = append(table.Entries, entry)
	}
	router.ResolveMerchantSelectors(table, "/billing", func(ctx context.Context, r *http.Request) (billingauth.Target, error) {
		return merchanttarget.Resolve(ctx, r, a.Runtime.Merchants, a.Runtime.ConfiguredMerchant(), "")
	}, embeddedMount+CustomerPrefix)
	return table, nil
}
