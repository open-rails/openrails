package embedhttp

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"

	auth "github.com/open-rails/helpers/auth"

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
		return fmt.Errorf("openrails: Routes.Auth is required: it says who each request is")
	}
	_, err := RoutePermissions(sel)
	return err
}

// RoutePermissions is sel's route groups as the routes check them: each
// staff group that is on, with its permission, and each programmatic
// permission. It refuses a staff group on without its permission, a
// permission for a group that is off or the Auth does not know, a
// permission without Routes.Scope or a Scope without one, and any group
// without Routes.Auth: nothing is ever mounted open.
func RoutePermissions(sel config.Routes) (httproutes.Permissions, error) {
	b := sel.RouteGroups
	perms := configuredPermissions(sel.Permissions)
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
	for _, n := range httproutes.AppNeeds {
		if !b.Programmatic && *perms.Field(n) != "" {
			return perms, fmt.Errorf("openrails: Permissions.%s is given, but RouteGroups.Programmatic is off", n)
		}
	}
	if err := perms.Validate(); err != nil {
		return perms, err
	}
	given := perms != (httproutes.Permissions{})
	if (given || b.Programmatic) && httproutes.IsNilAuth(sel.Auth) {
		return perms, fmt.Errorf("openrails: RouteGroups need Routes.Auth (it says who each staff and programmatic request is)")
	}
	switch {
	case given && (strings.TrimSpace(sel.Scope.Authority) == "" || strings.TrimSpace(sel.Scope.ID) == ""):
		return perms, fmt.Errorf("openrails: a permission is given without Routes.Scope (where callers hold Permissions), or with its Authority or ID empty")
	case !given && sel.Scope != (billingauth.Scope{}):
		return perms, fmt.Errorf("openrails: Routes.Scope is given, but no permission is")
	}
	if catalog, ok := sel.Auth.(auth.PermissionCatalog); ok {
		for _, n := range httproutes.AllNeeds() {
			if perm := *perms.Field(n); perm != "" && !catalog.KnownPermission(perm) {
				return perms, fmt.Errorf("openrails: Routes.Auth does not know the permission %q (Routes.Permissions)", perm)
			}
		}
	}
	return perms, nil
}

// configuredPermissions is the host's Permissions as the routes check them,
// each field read by its Need's name.
func configuredPermissions(given config.Permissions) httproutes.Permissions {
	var perms httproutes.Permissions
	v := reflect.ValueOf(given)
	for _, n := range httproutes.AllNeeds() {
		if f := v.FieldByName(string(n)); f.IsValid() {
			if perm, ok := f.Interface().(fmt.Stringer); ok {
				*perms.Field(n) = permissionText(perm)
			}
		}
	}
	return perms
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
	asm.Auth, asm.Scope = sel.Auth, sel.Scope
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
