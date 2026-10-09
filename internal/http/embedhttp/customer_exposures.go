package embedhttp

import (
	"context"
	"fmt"
	"net/http"
	"path"
	"strings"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchanttarget"
)

func validateCustomerRoutes(profiles []config.CustomerRoutes, rt *app.Runtime) error {
	for _, e := range profiles {
		if e.Prefix == "" {
			e.Prefix = "/v1/me"
		}
		if httproutes.IsNilAuth(e.Auth) {
			return fmt.Errorf("openrails: customer routes %q need Routes.Auth (or the profile's own Auth)", e.Prefix)
		}
		if e.Scope != config.CustomerSelfService && e.Scope != config.CustomerSubscriptionManagement && e.Scope != config.CustomerBillingManagement {
			return fmt.Errorf("openrails: customer routes %q need a Scope (CustomerSelfService, CustomerSubscriptionManagement or CustomerBillingManagement)", e.Prefix)
		}
		if e.Prefix == "" || e.Prefix == "/" || !strings.HasPrefix(e.Prefix, "/") || path.Clean(e.Prefix) != e.Prefix || strings.ContainsAny(e.Prefix, "*+?#%\\ \t\r\n") {
			return fmt.Errorf("openrails: invalid customer routes prefix %q", e.Prefix)
		}
		for _, part := range strings.Split(e.Prefix, "/") {
			if strings.ContainsAny(part, "{}") && (!strings.HasPrefix(part, "{") || !strings.HasSuffix(part, "}") || strings.ContainsAny(part[1:len(part)-1], "{}.") || len(part) < 3) {
				return fmt.Errorf("openrails: invalid customer routes prefix %q", e.Prefix)
			}
		}
	}
	return nil
}

// CustomerPrefixes are the paths, beneath mount, at which the exposures serve
// customer routes.
func CustomerPrefixes(mount string, exposures []config.CustomerRoutes) []string {
	out := make([]string, 0, len(exposures))
	for _, e := range exposures {
		if e.Prefix != "" {
			out = append(out, mount+e.Prefix)
		}
	}
	return out
}

// BuildCustomerRoutes mounts each customer profile, gated by its Auth at its
// merchant (or at the merchant each verdict names, when it has none). host
// resolves a merchant's API host (#734): the standalone server's; nil
// embedded.
func BuildCustomerRoutes(a *app.App, exposures []config.CustomerRoutes, host merchant.HostResolver) (*router.Table, error) {
	if err := validateCustomerRoutes(exposures, a.Runtime); err != nil {
		return nil, err
	}
	out := &router.Table{}
	if len(exposures) == 0 {
		return out, nil
	}
	providers, err := ConfiguredProviderRoutes(context.Background(), a.Runtime, true)
	if err != nil {
		return nil, err
	}
	for _, e := range exposures {
		if e.Prefix == "" {
			e.Prefix = "/v1/me"
		}
		mount := httproutes.CustomerMount{Auth: e.Auth, Providers: providers}
		if strings.TrimSpace(e.Merchant) != "" {
			target, err := merchanttarget.Resolve(context.Background(), nil, a.Runtime.Merchants, a.Runtime.ConfiguredMerchant(), e.Merchant)
			if err != nil {
				return nil, fmt.Errorf("customer merchant %q: %w", e.Merchant, err)
			}
			mount.Merchant = target
		}
		table := &router.Table{}
		rr := router.NewMux(table, e.Prefix, a.Runtime)
		switch e.Scope {
		case config.CustomerSubscriptionManagement:
			httproutes.RegisterCustomerSubscriptionManagementRoutes(rr, a.Runtime, mount)
		case config.CustomerBillingManagement:
			httproutes.RegisterCustomerBillingManagementRoutes(rr, a.Runtime, mount)
		default:
			httproutes.RegisterSelfServiceRoutes(rr, a.Runtime, mount)
		}
		wrapped := wrapCustomerRoutes(a.Runtime, table, host, e.Prefix)
		out.Entries = append(out.Entries, wrapped.Entries...)
	}
	return out, nil
}

// ValidateRouteTable rejects duplicate and ambiguous native registrations before
// an adapter mutates its host router. ServeMux validates wildcard syntax too.
func ValidateRouteTable(table *router.Table) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("openrails HTTP: conflicting configured routes: %v", recovered)
		}
	}()
	mux := http.NewServeMux()
	wildcards := make(map[string]string)
	subtrees := make(map[string]string)
	descendants := make(map[string]string)
	for _, entry := range table.Entries {
		// Gin shares one wildcard name at each method/path-tree position, even
		// when the routes diverge later. Validate this before any host mutation;
		// ServeMux alone permits names that Gin cannot register together.
		prefix := entry.Method
		for _, part := range strings.Split(entry.Path, "/") {
			// Native catch-all routes own every descendant for their method.
			// ServeMux instead allows more-specific routes below that subtree.
			if previous, ok := subtrees[prefix]; ok {
				return fmt.Errorf("openrails HTTP: conflicting native subtree routes %q and %q", previous, entry.Path)
			}
			if strings.HasPrefix(part, "{") && strings.HasSuffix(part, "...}") {
				if previous, ok := descendants[prefix]; ok {
					return fmt.Errorf("openrails HTTP: conflicting native subtree routes %q and %q", previous, entry.Path)
				}
				subtrees[prefix] = entry.Path
			} else {
				descendants[prefix] = entry.Path
			}
			if strings.HasPrefix(part, "{") && strings.HasSuffix(part, "}") {
				prefix += "/{}"
				if previous, ok := wildcards[prefix]; ok && previous != part {
					return fmt.Errorf("openrails HTTP: conflicting native wildcard names %q and %q at %s", previous, part, prefix)
				}
				wildcards[prefix] = part
			} else {
				prefix += "/" + part
			}
		}
		mux.Handle(entry.Method+" "+entry.Path, entry.Handler)
	}
	return nil
}
