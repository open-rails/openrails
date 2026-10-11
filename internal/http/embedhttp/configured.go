package embedhttp

import (
	"context"
	"fmt"
	"net/http"

	auth "github.com/open-rails/helpers/auth"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/internal/merchanttarget"
)

// ValidateRoutes refuses a selection without the Auth its routes need,
// before anything mounts: nothing is ever mounted open. A staff or
// programmatic group needs an Auth with a root scope (helpers/auth Root),
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
		return fmt.Errorf("openrails: RouteGroups need a Routes.Auth that is a helpers/auth Root (its RootScope is where root: permissions are held); %T is not", sel.Auth)
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
	providers, err := ConfiguredProviderRoutes(context.Background(), a.Runtime)
	if err != nil {
		return nil, err
	}
	// Generic callbacks remain registered as API-owned accounts are added after
	// startup. Request-time account/signature verification is authoritative.
	providers.Webhooks = true
	table := asm.NewRoutes(Options{Groups: sel.RouteGroups, ProviderRoutes: &providers})
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
