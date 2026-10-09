// Package openrailsfiber registers an OpenRails Client's routes natively on a Fiber v3 app.
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

// Mount registers one native route per method and path of the routes
// selection selects (Client.Routes) on the root app: the API under
// selection.Prefix. Fiber keeps its native matching and 404/405 behavior;
// configure CaseSensitive and StrictRouting on the host for exact matching.
func Mount(app *fiber.App, client *openrails.Client, selection openrails.Routes) error {
	if client == nil || app == nil {
		return fmt.Errorf("openrailsfiber: Mount requires a Fiber app and a client")
	}
	routes, err := client.Routes(selection)
	if err != nil {
		return err
	}
	return mount(app, routes)
}

func mount(app *fiber.App, routes []openrails.Route) error {
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
			app.Add([]string{method}, nativePath(route.Path), h).Name(RouteNamePrefix + method + " " + route.Path)
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
// Config.Checkout.EmbedOrigins may frame it.
func CheckoutFramePolicy(client *openrails.Client) fiber.Handler {
	policy := client.CheckoutFrameAncestors()
	return func(c fiber.Ctx) error {
		c.Set("Content-Security-Policy", policy)
		return c.Next()
	}
}
