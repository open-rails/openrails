package embed

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/open-rails/openrails/internal/http/embedhttp"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/operator"
)

// HTTPConfig configures the external runtime surface once at construction.
type HTTPConfig = embedhttp.HTTPConfig
type CustomerHTTPConfig = embedhttp.CustomerHTTPConfig
type CustomerHTTPScope = embedhttp.CustomerHTTPScope

const (
	CustomerSelfService            = embedhttp.CustomerSelfService
	CustomerSubscriptionManagement = embedhttp.CustomerSubscriptionManagement
)

// HTTPRoute is one native registration. Path uses net/http whole-segment
// wildcards, relative to the mount (for example /v1/me/{id}). Handler binds
// Request.PathValue while retaining the original URL and body for authorization.
type HTTPRoute struct {
	Method  string
	Path    string
	Handler http.Handler
}

// ConfigureHTTP declares the runtime's HTTP policy once, after host identity and
// merchant provisioning if needed. Options.HTTP is the constructor convenience.
// Invalid policy leaves the runtime unconfigured. Routes freezes configuration;
// subsequent configuration or configuration after Close is refused.
func (r *Runtime) ConfigureHTTP(cfg HTTPConfig) error {
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
	if err := embedhttp.ValidateHTTPConfig(&cfg, r.delegatedAuthenticator); err != nil {
		return err
	}
	cfg.CustomerExposures = append([]CustomerHTTPConfig(nil), cfg.CustomerExposures...)
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
		return nil, fmt.Errorf("openrails HTTP: disabled; configure Options.HTTP or call ConfigureHTTP before Routes")
	}
	if !r.httpBuilt {
		var table *router.Table
		var err error
		trimPrefix := "/billing"
		if r.httpConfig.Standalone {
			standalone, buildErr := operator.StandaloneRoutes(r.app)
			if buildErr != nil {
				return nil, buildErr
			}
			table = standalone
			trimPrefix = ""
		} else {
			table, err = embedhttp.ConfiguredRoutes(r.app, r.httpConfig, r.delegatedAuthenticator)
			if err != nil {
				return nil, err
			}
		}
		for i := range table.Entries {
			table.Entries[i].Path = strings.TrimPrefix(table.Entries[i].Path, trimPrefix)
		}
		extra, err := embedhttp.CustomerExposureRoutes(r.app, r.httpConfig.CustomerExposures)
		if err != nil {
			return nil, err
		}
		table.Entries = append(table.Entries, extra.Entries...)
		if err := embedhttp.ValidateRouteTable(table); err != nil {
			return nil, err
		}
		r.httpRoutes = publicHTTPRoutes(table)
		r.httpBuilt = true
	}
	return append([]HTTPRoute(nil), r.httpRoutes...), nil
}

func publicHTTPRoutes(table *router.Table) []HTTPRoute {
	routes := make([]HTTPRoute, 0, len(table.Entries))
	for _, entry := range table.Entries {
		routes = append(routes, HTTPRoute{Method: entry.Method, Path: entry.Path, Handler: bindHTTPPathValues(entry.Path, entry.Handler)})
	}
	return routes
}

func bindHTTPPathValues(pattern string, next http.Handler) http.Handler {
	parts := strings.Split(strings.TrimPrefix(pattern, "/"), "/")
	if strings.HasSuffix(pattern, "...}") {
		// Only root-anchored console assets use subtree routes. Their handler
		// reads the original URL; customer exposure prefixes forbid subtrees.
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Mount groups add leading segments; route patterns describe the suffix.
		path := strings.Split(strings.Trim(r.URL.EscapedPath(), "/"), "/")
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

// HTTPRequiresRoot reports whether configured routes include issuer-anchored
// control-plane and console URLs. Such bundles mount at the host root.
func (r *Runtime) HTTPRequiresRoot() bool {
	if r == nil {
		return false
	}
	r.httpMu.Lock()
	defer r.httpMu.Unlock()
	return r.httpConfig != nil && r.httpConfig.Standalone
}
