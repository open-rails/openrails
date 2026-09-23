// Package openrailsgin registers the runtime's configured routes natively in Gin.
package openrailsgin

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/open-rails/openrails/embed"
)

type Bundle struct {
	routes   []embed.HTTPRoute
	rootOnly bool
}

type RouteSource interface {
	HTTPRoutes() ([]embed.HTTPRoute, error)
	HTTPRequiresRoot() bool
}

func Routes(runtime RouteSource) (*Bundle, error) {
	routes, err := runtime.HTTPRoutes()
	if err != nil {
		return nil, err
	}
	return &Bundle{routes: routes, rootOnly: runtime.HTTPRequiresRoot()}, nil
}

// Mount registers one route per method/path under the host's router group.
// Gin owns its usual 404/405 and redirect policy (HandleMethodNotAllowed, etc.).
func (b *Bundle) Mount(target gin.IRoutes) error {
	if b == nil || target == nil {
		return fmt.Errorf("openrails Gin: bundle and router are required")
	}
	if b.rootOnly {
		if _, ok := target.(*gin.Engine); !ok {
			return fmt.Errorf("openrails Gin: standalone routes must mount on the root Engine")
		}
	}
	heads := map[string]bool{}
	for _, route := range b.routes {
		if route.Method == http.MethodHead {
			heads[route.Path] = true
		}
	}
	for _, route := range b.routes {
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
