// Package openrailsgin registers an OpenRails Client's routes natively in Gin.
package openrailsgin

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/open-rails/openrails"
)

// Mount registers one native route per method and path of the routes
// selection selects (Client.Routes) on the root engine: the API under
// selection.Prefix, e.g. Mount(r, client, openrails.Routes{Prefix: "/billing",
// Storefront: true}) serves /billing/v1/*. Gin keeps its own 404, 405 and
// redirect policy.
func Mount(router *gin.Engine, client *openrails.Client, selection openrails.Routes) error {
	if client == nil || router == nil {
		return fmt.Errorf("openrailsgin: Mount requires a Gin engine and a client")
	}
	routes, err := client.Routes(selection)
	if err != nil {
		return err
	}
	return MountRoutes(router, routes)
}

// MountRoutes registers a host-selected subset of Client.Routes on the root
// engine. Hosts can own an individual route without duplicating path or HEAD
// handling.
func MountRoutes(router *gin.Engine, routes []openrails.Route) error {
	if router == nil {
		return fmt.Errorf("openrailsgin: MountRoutes requires a Gin engine")
	}
	heads := map[string]bool{}
	for _, route := range routes {
		if route.Method == http.MethodHead {
			heads[route.Path] = true
		}
	}
	for _, route := range routes {
		router.Handle(route.Method, nativePath(route.Path), gin.WrapH(route.Handler))
		if route.Method == http.MethodGet && !heads[route.Path] {
			router.Handle(http.MethodHead, nativePath(route.Path), gin.WrapH(route.Handler))
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
				parts[i] = "*" + strings.TrimSuffix(name, "...")
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
func CheckoutFramePolicy(client *openrails.Client) gin.HandlerFunc {
	policy := client.CheckoutFrameAncestors()
	return func(c *gin.Context) {
		c.Header("Content-Security-Policy", policy)
		c.Next()
	}
}
