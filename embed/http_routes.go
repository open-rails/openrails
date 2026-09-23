package embed

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/internal/http/routebundle"

	"github.com/open-rails/openrails/internal/http/embedhttp"
	"github.com/open-rails/openrails/internal/requestauth"
)

// HTTPConfig configures the external runtime surface once at construction.
type HTTPConfig = embedhttp.HTTPConfig
type CustomerRoutesConfig = embedhttp.CustomerRoutesConfig
type CustomerHTTPScope = embedhttp.CustomerHTTPScope

const (
	CustomerSelfService            = embedhttp.CustomerSelfService
	CustomerSubscriptionManagement = embedhttp.CustomerSubscriptionManagement
	CustomerBillingManagement      = embedhttp.CustomerBillingManagement
)

// HTTPRoute is one native registration. Path uses net/http whole-segment
// wildcards, relative to the mount (for example /v1/me/{id}). Handler binds
// Request.PathValue while retaining the original URL and body for authorization.
type HTTPRoute = routebundle.Route

// configureHTTP copies the constructor-owned HTTP policy once.
// Invalid policy leaves the runtime unconfigured. Routes freezes configuration;
// subsequent configuration or configuration after Close is refused.
func (r *Runtime) configureHTTP(cfg HTTPConfig) error {
	if r == nil || r.app == nil || r.app.Runtime == nil {
		return fmt.Errorf("openrails HTTP: runtime is not initialized")
	}
	r.httpMu.Lock()
	defer r.httpMu.Unlock()
	if r.closed {
		return fmt.Errorf("openrails HTTP: runtime is closed")
	}
	if r.httpFrozen {
		return fmt.Errorf("openrails HTTP: routes have already frozen configuration")
	}
	if r.httpConfig != nil {
		return fmt.Errorf("openrails HTTP: already configured")
	}
	if err := embedhttp.ValidateHTTPConfig(&cfg, r.app.Runtime.Auth); err != nil {
		return err
	}
	cfg.CustomerRoutes = append([]CustomerRoutesConfig(nil), cfg.CustomerRoutes...)

	r.httpConfig = &cfg
	return nil
}

// HTTPRoutes materializes the configured surface once for framework adapters.
// Call after provisioning and River composition, before starting HTTP. No server,
// database or worker is created. An unconfigured runtime refuses HTTP explicitly.
func (r *Runtime) HTTPRoutes() ([]HTTPRoute, error) {
	if r == nil || r.app == nil || r.app.Runtime == nil {
		return nil, fmt.Errorf("openrails HTTP: runtime is not initialized")
	}
	r.httpMu.Lock()
	defer r.httpMu.Unlock()
	if r.closed {
		return nil, fmt.Errorf("openrails HTTP: runtime is closed")
	}
	r.httpFrozen = true
	if r.httpConfig == nil {
		return nil, fmt.Errorf("openrails HTTP: disabled; supply Options.HTTP at construction")
	}
	if !r.httpBuilt {
		table, err := embedhttp.ConfiguredRoutes(r.app, r.httpConfig)
		if err != nil {
			return nil, err
		}
		for i := range table.Entries {
			table.Entries[i].Path = strings.TrimPrefix(table.Entries[i].Path, "/billing")
		}
		if err := embedhttp.ValidateRouteTable(table); err != nil {
			return nil, err
		}
		r.httpRoutes = routebundle.FromTable(table)
		r.httpBuilt = true
	}
	return append([]HTTPRoute(nil), r.httpRoutes...), nil
}

// HTTPRequiresRoot is false for the mount-relative embedded billing surface.
func (r *Runtime) HTTPRequiresRoot() bool { return false }

func bindHTTPPathValues(pattern string, next http.Handler) http.Handler {
	return routebundle.BindPathValues(pattern, next)
}

func withVerificationMemo(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { next.ServeHTTP(w, requestauth.Begin(r)) })
}
