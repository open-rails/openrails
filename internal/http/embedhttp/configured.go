package embedhttp

import (
	"context"
	"fmt"

	auth "github.com/open-rails/authkit/auth"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/http/handlers"
	"github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/internal/http/routesurface"
)

// ValidateRoutes refuses a selection without the Auth its routes need,
// before anything mounts: nothing is ever mounted open. A staff or
// programmatic group needs an Auth with a root scope (authkit/auth Root),
// where its routes' root: permissions are held. The mount itself refuses a
// route whose permissions the Auth knows none of.
func ValidateRoutes(sel config.Routes) error {
	if httproutes.IsNilAuth(sel.Auth) {
		return fmt.Errorf("openrails: Routes.Auth is required: it says who each request is")
	}
	if b := sel.RouteGroups; !httproutes.StaffGroups(b) && !b.Programmatic {
		return nil
	}
	if _, ok := sel.Auth.(auth.Root); !ok {
		return fmt.Errorf("openrails: RouteGroups need a Routes.Auth that is a authkit/auth Root (its RootScope is where root: permissions are held); %T is not", sel.Auth)
	}
	return nil
}

// ConfiguredRoutes is the embedded HTTP surface sel selects, under /billing.
func ConfiguredRoutes(a *app.App, sel config.Routes) (*router.Table, error) {
	if a == nil || a.Runtime == nil || a.Config == nil {
		return nil, fmt.Errorf("openrails HTTP: runtime is not initialized")
	}
	if err := ValidateRoutes(sel); err != nil {
		return nil, err
	}
	asm := FromApp(a)
	asm.Auth = sel.Auth
	providers, err := mountedProviderRoutes(a)
	if err != nil {
		return nil, err
	}
	table := asm.NewRoutes(Options{Groups: sel.RouteGroups, ProviderRoutes: &providers})
	extra, err := BuildCustomerRoutes(a, sel.Auth)
	if err != nil {
		return nil, err
	}
	for _, entry := range extra.Entries {
		entry.Path = "/billing" + entry.Path
		table.Entries = append(table.Entries, entry)
	}
	return table, nil
}

// mountedProviderRoutes is the provider routes ConfiguredRoutes mounts.
func mountedProviderRoutes(a *app.App) (routesurface.ProviderRoutes, error) {
	providers, err := ConfiguredProviderRoutes(context.Background(), a.Runtime)
	if err != nil {
		return providers, err
	}
	// Generic callbacks remain registered as API-owned accounts are added after
	// startup. Request-time account/signature verification is authoritative.
	providers.Webhooks = true
	return providers, nil
}

// ConfiguredPublicConfig is the document GET /v1/config serves on the mount
// ConfiguredRoutes builds for sel, without a request.
func ConfiguredPublicConfig(a *app.App, sel config.Routes) (billing.PublicConfig, error) {
	if a == nil || a.Runtime == nil || a.Config == nil {
		return billing.PublicConfig{}, fmt.Errorf("openrails HTTP: runtime is not initialized")
	}
	providers, err := mountedProviderRoutes(a)
	if err != nil {
		return billing.PublicConfig{}, err
	}
	return handlers.PublicConfigDocument(a.Runtime, CapabilitiesFor(a.Runtime, sel.RouteGroups, providers, nil)), nil
}
