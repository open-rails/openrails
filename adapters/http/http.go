// Package openrailshttp mounts configured OpenRails routes on net/http or Chi.
package openrailshttp

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/embed"
)

type Bundle struct {
	routes   []embed.HTTPRoute
	rootOnly bool
}

// Routes materializes the HTTP configuration declared when the runtime was built.
func Routes(runtime *embed.Runtime) (*Bundle, error) {
	routes, err := runtime.HTTPRoutes()
	if err != nil {
		return nil, err
	}
	return &Bundle{routes: routes, rootOnly: runtime.HTTPRequiresRoot()}, nil
}

// Mount registers every route natively. target may be a *http.ServeMux or any
// router with Chi's Method(string,string,http.Handler) signature. An optional
// prefix is for routers without a group; a Chi Route group needs no prefix.
func (b *Bundle) Mount(target any, prefix ...string) error { return b.mount(target, false, prefix...) }

// MountRoot mounts an anchored standalone bundle on a router whose root cannot
// be identified through its public API, such as Chi. The caller asserts that
// target is the application's root router, not a Route/Group subrouter.
func (b *Bundle) MountRoot(target any) error { return b.mount(target, true) }

func (b *Bundle) mount(target any, rootAsserted bool, prefix ...string) error {
	if b == nil {
		return fmt.Errorf("openrails HTTP: nil route bundle")
	}
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
	if b.rootOnly {
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
		for _, route := range b.routes {
			if route.Method == http.MethodHead {
				heads[route.Path] = true
			}
		}
		for _, route := range b.routes {
			r.Method(route.Method, chiPath(base+route.Path), route.Handler)
			if route.Method == http.MethodGet && !heads[route.Path] {
				r.Method(http.MethodHead, chiPath(base+route.Path), route.Handler)
			}
		}
	case interface{ Handle(string, http.Handler) }:
		for _, route := range b.routes {
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
