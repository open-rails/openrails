// Package openrailsfiber registers configured OpenRails routes natively in Fiber v3.
package openrailsfiber

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
	"github.com/open-rails/openrails/embed"
)

// RouteNamePrefix identifies native OpenRails registrations in Fiber inspection.
const RouteNamePrefix = "openrails."

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
// Fiber retains its native matching and 404/405 behavior. For case-sensitive,
// exact slash matching, configure CaseSensitive and StrictRouting on the host.
func (b *Bundle) Mount(target fiber.Router) error {
	if b == nil || target == nil {
		return fmt.Errorf("openrails Fiber: bundle and router are required")
	}
	if b.rootOnly {
		if _, ok := target.(*fiber.App); !ok {
			return fmt.Errorf("openrails Fiber: standalone routes must mount on the root App")
		}
	}
	heads := map[string]bool{}
	for _, route := range b.routes {
		if route.Method == http.MethodHead {
			heads[route.Path] = true
		}
	}
	for _, route := range b.routes {
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
