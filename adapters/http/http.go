// Package openrailshttp mounts configured OpenRails routes on net/http or Chi.
package openrailshttp

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/open-rails/openrails"
)

// Mount registers client.Routes on a *http.ServeMux or any router with Chi's
// Method(string, string, http.Handler). prefix is for routers without groups,
// e.g. Mount(mux, client, "/billing"); a Chi Route group needs none.
func Mount(target any, client *openrails.Client, prefix ...string) error {
	routes, root, err := clientRoutes(client)
	if err != nil {
		return err
	}
	return mount(target, routes, root, false, prefix...)
}

// MountRoot mounts the control plane's standalone surface on a router whose
// root cannot be identified through its API, such as Chi. The caller asserts
// target is the application's root router, not a Route/Group subrouter.
func MountRoot(target any, client *openrails.Client) error {
	routes, root, err := clientRoutes(client)
	if err != nil {
		return err
	}
	return mount(target, routes, root, true)
}

func clientRoutes(client *openrails.Client) ([]openrails.Route, bool, error) {
	if client == nil {
		return nil, false, fmt.Errorf("openrails HTTP: client is required")
	}
	routes, err := client.Routes()
	return routes, client.RoutesRequireRoot(), err
}

func mount(target any, routes []openrails.Route, rootOnly, rootAsserted bool, prefix ...string) error {
	if len(prefix) > 1 {
		return fmt.Errorf("openrails HTTP: at most one mount prefix")
	}
	base := ""
	if len(prefix) == 1 {
		base = strings.TrimRight(prefix[0], "/")
	}
	if base != "" && (!strings.HasPrefix(base, "/") || strings.ContainsAny(base, "{}?# ")) {
		return fmt.Errorf("openrails HTTP: invalid mount prefix %q", base)
	}
	if rootOnly {
		if base != "" {
			return fmt.Errorf("openrails HTTP: standalone routes must mount at root without a prefix")
		}
		if _, ok := target.(*http.ServeMux); !ok && !rootAsserted {
			return fmt.Errorf("openrails HTTP: standalone routes require a root ServeMux or MountRoot(rootRouter)")
		}
	}
	switch r := target.(type) {
	case interface {
		Method(string, string, http.Handler)
	}:
		// Chi owns method/path matching. Bind net/http PathValue for the neutral
		// handlers without coupling this adapter to Chi or rewriting signed URLs.
		heads := map[string]bool{}
		for _, route := range routes {
			if route.Method == http.MethodHead {
				heads[route.Path] = true
			}
		}
		for _, route := range routes {
			r.Method(route.Method, chiPath(base+route.Path), route.Handler)
			if route.Method == http.MethodGet && !heads[route.Path] {
				r.Method(http.MethodHead, chiPath(base+route.Path), route.Handler)
			}
		}
	case interface{ Handle(string, http.Handler) }:
		for _, route := range routes {
			r.Handle(route.Method+" "+base+route.Path, route.Handler)
		}
	default:
		return fmt.Errorf("openrails HTTP: router must implement Handle or Method")
	}
	return nil
}

func chiPath(path string) string {
	parts := strings.Split(path, "/")
	for i, part := range parts {
		if strings.HasPrefix(part, "{") && strings.HasSuffix(part, "...}") {
			parts[i] = "*"
		}
	}
	return strings.Join(parts, "/")
}

// CheckoutFramePolicy wraps the handler serving the hosted checkout page
// (billing-ui's <CheckoutPage>): only the host and
// Config.HTTP.Checkout.EmbedOrigins may frame it.
func CheckoutFramePolicy(client *openrails.Client) func(http.Handler) http.Handler {
	policy := client.CheckoutFrameAncestors()
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Security-Policy", policy)
			next.ServeHTTP(w, r)
		})
	}
}
