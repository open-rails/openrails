package embed

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/open-rails/openrails/internal/http/embedhttp"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/pkg/billingauth"
)

// HTTPConfig declares externally accessible HTTP capabilities. Nil Options.HTTP
// disables HTTP entirely. A non-nil configuration includes capability discovery
// and provider callbacks; accepting a callback still requires an armed account
// and valid provider verification. Management and buyer surfaces are opt-in and
// independent of in-process Client or CatalogClient access.
type HTTPConfig struct {
	Checkout         bool
	Customer         bool
	MerchantAdmin    bool
	Catalog          bool
	PaymentProviders bool
	MerchantAPI      bool
	Authenticator    billingauth.Authenticator
	Gate             billingauth.Gate
}

func validateHTTPConfig(cfg *HTTPConfig, delegated billingauth.DelegatedAuthenticator) error {
	if cfg == nil {
		return nil
	}
	if cfg.Checkout && cfg.Authenticator == nil {
		return fmt.Errorf("openrails HTTP: Checkout requires HTTP.Authenticator")
	}
	if cfg.Customer && delegated == nil {
		return fmt.Errorf("openrails HTTP: Customer requires Options.DelegatedAuthenticator")
	}
	if (cfg.MerchantAdmin || cfg.Catalog || cfg.PaymentProviders || cfg.MerchantAPI) && cfg.Gate == nil {
		return fmt.Errorf("openrails HTTP: management surfaces require HTTP.Gate")
	}
	return nil
}

func (cfg HTTPConfig) routeSets() []RouteSet {
	sets := []RouteSet{RouteSetWebhooks}
	for _, v := range []struct {
		enabled bool
		set     RouteSet
	}{
		{cfg.Checkout, RouteSetCheckout}, {cfg.Customer, RouteSetCustomer},
		{cfg.MerchantAdmin, RouteSetMerchantAdmin}, {cfg.Catalog, RouteSetCatalog},
		{cfg.PaymentProviders, RouteSetPaymentProviders}, {cfg.MerchantAPI, RouteSetMerchantAPI},
	} {
		if v.enabled {
			sets = append(sets, v.set)
		}
	}
	return sets
}

// HTTPRoute is one native registration. Path uses net/http whole-segment
// wildcards, relative to the host mount (for example /v1/me/{id}). Handler expects
// the host router to set Request.PathValue for those wildcards. The original URL
// and raw body must be retained for request-bound authorization and signatures.
type HTTPRoute struct {
	Method  string
	Path    string
	Handler http.Handler
}

// HTTPRoutes materializes the runtime's configured surface for framework
// adapters. Call after merchant/provider configuration and River composition,
// before starting the host server. No server, database or worker is created.
// Nil HTTP configuration returns an empty bundle.
func (r *Runtime) HTTPRoutes() ([]HTTPRoute, error) {
	if r == nil || r.app == nil || r.app.Runtime == nil {
		return nil, fmt.Errorf("openrails HTTP: runtime is not initialized")
	}
	if r.httpConfig == nil {
		return nil, nil
	}
	cfg := *r.httpConfig
	if err := validateHTTPConfig(&cfg, r.delegatedAuthenticator); err != nil {
		return nil, err
	}
	asm := embedhttp.FromApp(r.app)
	asm.Authenticator = cfg.Authenticator
	asm.Gate = cfg.Gate
	active := cfg.routeSets()
	providers := embedhttp.ProviderRoutesForRuntime(r.app.Runtime, nil)
	// Generic callbacks remain registered as API-owned accounts are added after
	// startup. Request-time account/signature verification is authoritative.
	providers.Webhooks = true
	table := asm.NewRoutes(embedhttp.Options{RouteSets: routeSetsWithoutCustomer(active), AdvertiseRouteSets: active, ProviderRoutes: &providers})
	if cfg.Customer {
		self := embedhttp.NewSelfRoutes(r.app.Runtime, r.delegatedAuthenticator, &providers, asm.HostResolve)
		table.Entries = append(table.Entries, self.Entries...)
	}
	return publicHTTPRoutes(table), nil
}

func publicHTTPRoutes(table *router.Table) []HTTPRoute {
	routes := make([]HTTPRoute, 0, len(table.Entries))
	for _, entry := range table.Entries {
		routes = append(routes, HTTPRoute{Method: entry.Method, Path: strings.TrimPrefix(entry.Path, "/billing"), Handler: bindHTTPPathValues(strings.TrimPrefix(entry.Path, "/billing"), entry.Handler)})
	}
	return routes
}

func bindHTTPPathValues(pattern string, next http.Handler) http.Handler {
	parts := strings.Split(strings.TrimPrefix(pattern, "/"), "/")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Mount groups add leading segments; route patterns describe the suffix.
		path := strings.Split(strings.TrimPrefix(r.URL.EscapedPath(), "/"), "/")
		if len(path) < len(parts) {
			http.NotFound(w, r)
			return
		}
		path = path[len(path)-len(parts):]
		for i, part := range parts {
			if strings.HasPrefix(part, "{") && strings.HasSuffix(part, "}") {
				value, err := url.PathUnescape(path[i])
				if err != nil {
					http.Error(w, "invalid path", 400)
					return
				}
				r.SetPathValue(part[1:len(part)-1], value)
			}
		}
		next.ServeHTTP(w, r)
	})
}
