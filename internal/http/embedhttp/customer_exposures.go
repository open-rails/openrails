package embedhttp

import (
	"context"
	"fmt"
	"net/http"
	"path"
	"strings"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/http/middleware"
	"github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/internal/merchanttarget"
	"github.com/open-rails/openrails/pkg/billingauth"
)

// CustomerHTTPScope selects a library-owned customer capability profile.
type CustomerHTTPScope uint8

const (
	// CustomerSelfService exposes the full customer self-service API.
	CustomerSelfService CustomerHTTPScope = iota
	// CustomerSubscriptionManagement exposes cancellation, resumption, subscription
	// payment-method changes and invoice collection-method selection only.
	CustomerSubscriptionManagement
	// CustomerBillingManagement adds existing billing history, purchased access,
	// saved methods and payment recovery without checkout or plan purchases.
	CustomerBillingManagement
)

// CustomerRoutesConfig exposes a customer profile with its own explicit authority.
// Prefix is the exact customer base, relative to the bundle's mount (for example
// /billing/v1/me). Whole-segment {parameters} are available to the authenticator
// through Request.PathValue. This surface never includes merchant administration,
// treasury, provider callbacks or credential management.
type CustomerRoutesConfig struct {
	// Prefix defaults to /v1/me. Additional audiences may mount the same
	// registrations elsewhere with their own explicit delegated payer authority.
	Prefix string
	// Merchant is the fixed merchant slug for native customer identity.
	Merchant string
	// Treasury adds the separately permission-gated /v1/customers group.
	Treasury               bool
	Scope                  CustomerHTTPScope
	DelegatedAuthenticator billingauth.DelegatedAuthenticator
}

func validateCustomerRoutes(exposures []CustomerRoutesConfig, auth *billingauth.Integration) error {
	for _, e := range exposures {
		if e.Prefix == "" {
			e.Prefix = "/v1/me"
		}
		if e.DelegatedAuthenticator == nil && (auth == nil || auth.Authentication == nil) {
			return fmt.Errorf("openrails HTTP: customer exposure %q requires its own authenticator", e.Prefix)
		}
		if e.DelegatedAuthenticator == nil && strings.TrimSpace(e.Merchant) == "" {
			return fmt.Errorf("openrails HTTP: native customer routes require an explicit merchant slug")
		}
		if e.Treasury && e.Prefix != "/v1/me" {
			return fmt.Errorf("openrails HTTP: customer treasury requires the canonical customer mount")
		}
		if e.Scope != CustomerSelfService && e.Scope != CustomerSubscriptionManagement && e.Scope != CustomerBillingManagement {
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

// BuildCustomerRoutes builds additional customer audiences from the same
// authoritative registrations used by the canonical customer surface.
func BuildCustomerRoutes(a *app.App, exposures []CustomerRoutesConfig, auth *billingauth.Integration) (*router.Table, error) {
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
		authn := e.DelegatedAuthenticator
		native := authn == nil
		if native {
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
		case CustomerSubscriptionManagement:
			httproutes.RegisterCustomerSubscriptionManagementRoutes(rr, a.Runtime, delegated)
		case CustomerBillingManagement:
			httproutes.RegisterCustomerBillingManagementRoutes(rr, a.Runtime, delegated, providers)
		default:
			httproutes.RegisterSelfServiceRoutes(rr, a.Runtime, delegated, providers)
		}
		if e.Treasury {
			treasury := delegated
			if native {
				treasury = nativeTreasury(delegated, auth)
			}
			httproutes.RegisterCustomerTreasuryRoutes(router.NewMux(table, "/v1/customers", a.Runtime), a.Runtime, treasury, providers)
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
