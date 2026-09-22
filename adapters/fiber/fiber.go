// Package openrailsfiber registers configured OpenRails routes natively in Fiber v3.
package openrailsfiber

import (
	"fmt"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
	"github.com/open-rails/openrails/embed"
	"net/http"
	"strings"
)

type Bundle struct{ routes []embed.HTTPRoute }

func Routes(runtime *embed.Runtime) (*Bundle, error) {
	routes, err := runtime.HTTPRoutes()
	if err != nil {
		return nil, err
	}
	return &Bundle{routes: routes}, nil
}

// Mount registers one route per method/path under the host's router group.
// Fiber retains its native matching and 404/405 behavior. For case-sensitive,
// exact slash matching, configure CaseSensitive and StrictRouting on the host.
func (b *Bundle) Mount(target fiber.Router) error {
	if b == nil || target == nil {
		return fmt.Errorf("openrails Fiber: bundle and router are required")
	}
	for _, route := range b.routes {
		h := adaptor.HTTPHandlerWithContext(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ctx, ok := adaptor.LocalContextFromHTTPRequest(r); ok {
				r = r.WithContext(ctx)
			}
			route.Handler.ServeHTTP(w, r)
		}))
		target.Add([]string{route.Method}, nativePath(route.Path), h)
	}
	return nil
}

func nativePath(path string) string {
	parts := strings.Split(path, "/")
	for i, part := range parts {
		if strings.HasPrefix(part, "{") && strings.HasSuffix(part, "}") {
			parts[i] = ":" + part[1:len(part)-1]
		} else {
			parts[i] = strings.ReplaceAll(part, ":", `\:`)
		}
	}
	return strings.Join(parts, "/")
}
