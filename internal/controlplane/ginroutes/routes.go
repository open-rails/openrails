// Package ginroutes holds the gin-specific mounting of the control plane's
// selective AuthKit route surface. It is imported only by the standalone (gin)
// HTTP path. The control plane core (RouteSpecs/MountedRouteGroups, all gin-free)
// lives in internal/controlplane, so that gin-free importers (ultimately
// pkg/embedded) do not transitively pull in github.com/gin-gonic/gin (#285).
package ginroutes

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/open-rails/openrails/internal/controlplane"
)

// MountAuthRoutes mounts the selected AuthKit route specs onto a gin router
// group. AuthKit specs use net/http ServeMux path syntax ("/owners/{slug}");
// they are translated to gin syntax ("/owners/:slug") and adapted to gin
// handlers. Returns the number of routes mounted.
//
// This is the selective-mounting entrypoint: in locked-down mode it mounts only
// IntentionalRouteGroups, never AuthKit DefaultAPI().
func MountAuthRoutes(c *controlplane.ControlPlane, group *gin.RouterGroup) int {
	if c == nil || group == nil {
		return 0
	}
	specs := c.RouteSpecs()
	for _, spec := range specs {
		group.Handle(spec.Method, toGinPath(spec.Path), adaptHandler(spec.Handler))
	}
	return len(specs)
}

// toGinPath converts net/http ServeMux path params ("{id}") to gin params
// (":id").
func toGinPath(p string) string {
	if !strings.ContainsAny(p, "{}") {
		return p
	}
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		switch p[i] {
		case '{':
			b.WriteByte(':')
		case '}':
			// drop
		default:
			b.WriteByte(p[i])
		}
	}
	return b.String()
}

// adaptHandler wraps a net/http handler so gin can serve it.
func adaptHandler(h http.Handler) gin.HandlerFunc {
	return func(ctx *gin.Context) {
		h.ServeHTTP(ctx.Writer, ctx.Request)
	}
}
