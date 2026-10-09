// Package openrailshttp mounts OpenRails routes on a net/http or Chi root router.
package openrailshttp

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/open-rails/openrails"
)

// Mount registers the routes selection selects (Client.Routes) on the root
// router: a *http.ServeMux, or a Chi root Mux (any router with Chi's
// Method(string, string, http.Handler)). The API lands under
// selection.Prefix: openrails.Routes{Prefix: "/billing"} serves /billing/v1/*.
func Mount(target any, client *openrails.Client, selection openrails.Routes) error {
	if client == nil {
		return fmt.Errorf("openrailshttp: Mount requires a client")
	}
	routes, err := client.Routes(selection)
	if err != nil {
		return err
	}
	return mount(target, routes)
}

func mount(target any, routes []openrails.Route) error {
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
			r.Method(route.Method, chiPath(route.Path), route.Handler)
			if route.Method == http.MethodGet && !heads[route.Path] {
				r.Method(http.MethodHead, chiPath(route.Path), route.Handler)
			}
		}
	case interface{ Handle(string, http.Handler) }:
		for _, route := range routes {
			r.Handle(route.Method+" "+route.Path, route.Handler)
		}
	default:
		return fmt.Errorf("openrailshttp: router must implement Handle or Method")
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
// Config.Checkout.EmbedOrigins may frame it.
func CheckoutFramePolicy(client *openrails.Client) func(http.Handler) http.Handler {
	policy := client.CheckoutFrameAncestors()
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Security-Policy", policy)
			next.ServeHTTP(w, r)
		})
	}
}
