// Package openrailsgin registers an OpenRails Client's routes natively in Gin.
package openrailsgin

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/open-rails/openrails"
)

// Mount registers one native route per method and path of client.Routes under
// target, e.g. Mount(r.Group("/billing"), client). Gin keeps its own 404, 405
// and redirect policy.
func Mount(target gin.IRoutes, client *openrails.Client) error {
	if client == nil || target == nil {
		return fmt.Errorf("openrails Gin: client and router are required")
	}
	routes, err := client.Routes()
	if err != nil {
		return err
	}
	return mount(target, routes, client.RoutesRequireRoot())
}

func mount(target gin.IRoutes, routes []openrails.Route, rootOnly bool) error {
	if rootOnly {
		if _, ok := target.(*gin.Engine); !ok {
			return fmt.Errorf("openrails Gin: standalone routes must mount on the root Engine")
		}
	}
	heads := map[string]bool{}
	for _, route := range routes {
		if route.Method == http.MethodHead {
			heads[route.Path] = true
		}
	}
	for _, route := range routes {
		target.Handle(route.Method, nativePath(route.Path), gin.WrapH(route.Handler))
		if route.Method == http.MethodGet && !heads[route.Path] {
			target.Handle(http.MethodHead, nativePath(route.Path), gin.WrapH(route.Handler))
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
