package embedhttp

import (
	"context"
	"fmt"
	"net/http"
	"path"
	"strings"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/http/middleware"
	"github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/internal/merchanttarget"
)

func validateCustomerRoutes(exposures []config.CustomerRoutesConfig, auth *billingauth.Integration) error {
	for _, e := range exposures {
		if e.Prefix == "" {
			e.Prefix = "/v1/me"
		}
		if !e.Delegated && (auth == nil || auth.Authentication == nil) {
			return fmt.Errorf("openrails HTTP: customer exposure %q requires its own authenticator", e.Prefix)
		}
		if !e.Delegated && strings.TrimSpace(e.Merchant) == "" {
			return fmt.Errorf("openrails HTTP: native customer routes require an explicit merchant slug")
		}
		if e.Scope != config.CustomerSelfService && e.Scope != config.CustomerSubscriptionManagement && e.Scope != config.CustomerBillingManagement {
			return fmt.Errorf("openrails HTTP: invalid customer scope %d", e.Scope)
		}
		if e.Prefix == "" || e.Prefix == "/" || !strings.HasPrefix(e.Prefix, "/") || path.Clean(e.Prefix) != e.Prefix || strings.ContainsAny(e.Prefix, "*+?#%\\ \t\r\n") {
			return fmt.Errorf("openrails HTTP: invalid customer prefix %q", e.Prefix)
		}
		for _, part := range strings.Split(e.Prefix, "/") {
			if strings.ContainsAny(part, "{}") && (!strings.HasPrefix(part, "{") || !strings.HasSuffix(part, "}") || strings.ContainsAny(part[1:len(part)-1], "{}.") || len(part) < 3) {
				return fmt.Errorf("openrails HTTP: invalid customer prefix %q", e.Prefix)
			}
		}
	}
	return nil
}

// CustomerPrefixes are the paths, beneath mount, at which the exposures serve
// customer routes.
func CustomerPrefixes(mount string, exposures []config.CustomerRoutesConfig) []string {
	out := make([]string, 0, len(exposures))
	for _, e := range exposures {
		if e.Prefix != "" {
			out = append(out, mount+e.Prefix)
		}
	}
	return out
}

// BuildCustomerRoutes builds additional customer audiences from the same
// authoritative registrations used by the canonical customer surface.
func BuildCustomerRoutes(a *app.App, exposures []config.CustomerRoutesConfig, auth *billingauth.Integration) (*router.Table, error) {
	if err := validateCustomerRoutes(exposures, auth); err != nil {
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
	host := FromApp(a).HostResolve
	for _, e := range exposures {
		if e.Prefix == "" {
			e.Prefix = "/v1/me"
		}
		var authn billingauth.DelegatedAuthenticator
		if e.Delegated {
			authenticate, profile := a.Runtime.AuthenticateCustomer, e.Prefix
			if authenticate == nil {
				return nil, fmt.Errorf("openrails HTTP: customer exposure %q is Delegated; set Deps.AuthenticateCustomer", e.Prefix)
			}
			authn = billingauth.DelegatedAuthenticatorFunc(func(_ context.Context, r *http.Request) (*billingauth.DelegatedPrincipal, error) {
				return authenticate(r, profile)
			})
		}
		if authn == nil {
			target, err := merchanttarget.Resolve(context.Background(), nil, a.Runtime.Merchants, a.Runtime.ConfiguredMerchant(), e.Merchant)
			if err != nil {
				return nil, fmt.Errorf("customer merchant %q: %w", e.Merchant, err)
			}
			authn = nativeCustomer(auth, target)
		}
		table := &router.Table{}
		rr := router.NewMux(table, e.Prefix, a.Runtime)
		delegated := middleware.DelegatedPrincipalRequired(authn)
		switch e.Scope {
		case config.CustomerSubscriptionManagement:
			httproutes.RegisterCustomerSubscriptionManagementRoutes(rr, a.Runtime, delegated)
		case config.CustomerBillingManagement:
			httproutes.RegisterCustomerBillingManagementRoutes(rr, a.Runtime, delegated, providers)
		default:
			httproutes.RegisterSelfServiceRoutes(rr, a.Runtime, delegated, providers)
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
