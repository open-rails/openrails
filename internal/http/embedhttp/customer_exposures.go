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
)

// CustomerHTTPConfig exposes a customer profile with its own explicit authority.
// Prefix is the exact customer base, relative to the bundle's mount (for example
// /billing/v1/me). Whole-segment {parameters} are available to the authenticator
// through Request.PathValue. This surface never includes merchant administration,
// treasury, provider callbacks or credential management.
type CustomerHTTPConfig struct {
	Prefix                 string
	Scope                  CustomerHTTPScope
	DelegatedAuthenticator billingauth.DelegatedAuthenticator
}

func validateCustomerExposures(exposures []CustomerHTTPConfig) error {
	for _, e := range exposures {
		if e.DelegatedAuthenticator == nil {
			return fmt.Errorf("openrails HTTP: customer exposure %q requires its own authenticator", e.Prefix)
		}
		if e.Scope != CustomerSelfService && e.Scope != CustomerSubscriptionManagement {
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

// CustomerExposureRoutes builds additional customer audiences from the same
// authoritative registrations used by the canonical customer surface.
func CustomerExposureRoutes(a *app.App, exposures []CustomerHTTPConfig) (*router.Table, error) {
	if err := validateCustomerExposures(exposures); err != nil {
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
		table := &router.Table{}
		rr := router.NewMux(table, e.Prefix, a.Runtime)
		delegated := middleware.DelegatedPrincipalRequired(e.DelegatedAuthenticator)
		if e.Scope == CustomerSubscriptionManagement {
			httproutes.RegisterCustomerSubscriptionManagementRoutes(rr, a.Runtime, delegated)
		} else {
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
	for _, entry := range table.Entries {
		// Gin shares one wildcard name at each method/path-tree position, even
		// when the routes diverge later. Validate this before any host mutation;
		// ServeMux alone permits names that Gin cannot register together.
		prefix := entry.Method
		for _, part := range strings.Split(entry.Path, "/") {
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
