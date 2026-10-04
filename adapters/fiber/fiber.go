// Package openrailsfiber registers an OpenRails Client's routes natively in Fiber v3.
package openrailsfiber

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
	"github.com/open-rails/openrails"
)

// RouteNamePrefix identifies native OpenRails registrations in Fiber inspection.
const RouteNamePrefix = "openrails."

// Mount registers one native route per method and path of client.Routes under
// target. Fiber keeps its native matching and 404/405 behavior; configure
// CaseSensitive and StrictRouting on the host for exact matching.
func Mount(target fiber.Router, client *openrails.Client) error {
	if client == nil || target == nil {
		return fmt.Errorf("openrails Fiber: client and router are required")
	}
	routes, err := client.Routes()
	if err != nil {
		return err
	}
	return mount(target, routes, client.RoutesRequireRoot())
}

func mount(target fiber.Router, routes []openrails.Route, rootOnly bool) error {
	if rootOnly {
		if _, ok := target.(*fiber.App); !ok {
			return fmt.Errorf("openrails Fiber: standalone routes must mount on the root App")
		}
	}
	heads := map[string]bool{}
	for _, route := range routes {
		if route.Method == http.MethodHead {
			heads[route.Path] = true
		}
	}
	for _, route := range routes {
		h := adaptor.HTTPHandlerWithContext(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ctx, ok := adaptor.LocalContextFromHTTPRequest(r); ok {
				r = r.WithContext(ctx)
			}
			route.Handler.ServeHTTP(w, r)
		}))
		methods := []string{route.Method}
		if route.Method == http.MethodGet && !heads[route.Path] {
			methods = append(methods, http.MethodHead)
		}
		for _, method := range methods {
			target.Add([]string{method}, nativePath(route.Path), h).Name(RouteNamePrefix + method + " " + route.Path)
		}
	}
	return nil
}

func nativePath(path string) string {
	parts := strings.Split(path, "/")
	for i, part := range parts {
		if strings.HasPrefix(part, "{") && strings.HasSuffix(part, "}") {
			name := part[1 : len(part)-1]
			if strings.HasSuffix(name, "...") {
				parts[i] = "*"
			} else {
				parts[i] = ":" + name
			}
		} else {
			parts[i] = strings.ReplaceAll(part, ":", `\:`)
		}
	}
	return strings.Join(parts, "/")
}

// CheckoutFramePolicy is middleware for the route serving the hosted checkout
// page (billing-ui's <CheckoutPage>): only the host and
// Config.HTTP.Checkout.EmbedOrigins may frame it.
func CheckoutFramePolicy(client *openrails.Client) fiber.Handler {
	policy := client.CheckoutFrameAncestors()
	return func(c fiber.Ctx) error {
		c.Set("Content-Security-Policy", policy)
		return c.Next()
	}
}
