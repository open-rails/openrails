package embed

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/open-rails/openrails/internal/http/embedhttp"
	"github.com/open-rails/openrails/internal/http/router"
)

// HTTPConfig configures the external runtime surface once at construction.
type HTTPConfig = embedhttp.HTTPConfig

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
	table, err := embedhttp.ConfiguredRoutes(r.app, r.httpConfig, r.delegatedAuthenticator)
	if err != nil {
		return nil, err
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
		path := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.EscapedPath(), "/"), "/"), "/")
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
