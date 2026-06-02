package controlplane

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	authhttp "github.com/open-rails/authkit/http"
)

// IntentionalRouteGroups is the set of AuthKit route groups OpenRails
// intentionally exposes in locked-down / self-hosted mode (issue #224 task 4).
//
// It deliberately EXCLUDES the full DefaultAPI surface. We mount only the
// login/session/user/JWKS-adjacent capabilities OpenRails needs:
//
//   - RouteCore: /token, /sessions/current, /logout (authentication).
//   - RoutePassword: password login + reset (authentication, not registration).
//   - RouteUser: self-service account routes (me, sessions, password change).
//
// NOT mounted by default in locked-down mode:
//   - RouteRegister (public user self-registration — disabled in self-hosted).
//   - RouteOrganizations (public org onboarding/management — owned by OpenRails
//     product routes + in-process core calls instead).
//   - RouteAdmin (AuthKit's own admin surface — OpenRails owns admin routes).
//   - RouteSolana / RouteOIDCBrowser / RouteFederation / 2FA / verification —
//     opt-in per deployment, not part of the locked-down default.
var IntentionalRouteGroups = []authhttp.RouteGroup{
	authhttp.RouteCore,
	authhttp.RoutePassword,
	authhttp.RouteUser,
}

// MountedRouteGroups returns the AuthKit route groups this control plane mounts.
// In locked-down mode it returns the intentional (non-DefaultAPI) subset; when
// not locked down (hosted-SaaS posture) it returns nil to signal "mount the full
// DefaultAPI surface" to the caller.
func (c *ControlPlane) MountedRouteGroups() []authhttp.RouteGroup {
	if c == nil {
		return nil
	}
	if c.LockedDown() {
		out := make([]authhttp.RouteGroup, len(IntentionalRouteGroups))
		copy(out, IntentionalRouteGroups)
		return out
	}
	// Hosted-SaaS posture: caller may choose to mount DefaultAPI. We still avoid
	// returning DefaultAPI() implicitly here — see RouteSpecs.
	return nil
}

// RouteSpecs returns the concrete AuthKit route specs to mount. In locked-down
// mode this is ONLY the intentional groups (never DefaultAPI). When not locked
// down it returns the full DefaultAPI surface.
func (c *ControlPlane) RouteSpecs() []authhttp.RouteSpec {
	if c == nil || c.authSvc == nil {
		return nil
	}
	routes := c.authSvc.Routes()
	if c.LockedDown() {
		return routes.Groups(IntentionalRouteGroups...)
	}
	return routes.DefaultAPI()
}

// MountAuthRoutes mounts the selected AuthKit route specs onto a gin router
// group. AuthKit specs use net/http ServeMux path syntax ("/owners/{slug}");
// they are translated to gin syntax ("/owners/:slug") and adapted to gin
// handlers. Returns the number of routes mounted.
//
// This is the selective-mounting entrypoint: in locked-down mode it mounts only
// IntentionalRouteGroups, never AuthKit DefaultAPI().
func (c *ControlPlane) MountAuthRoutes(group *gin.RouterGroup) int {
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
