package controlplane

import (
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
//   - RouteOrganizations (public tenant onboarding/management — owned by OpenRails
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
	if c.SelfHostedPosture() {
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
	if c.SelfHostedPosture() {
		return routes.Groups(IntentionalRouteGroups...)
	}
	return routes.DefaultAPI()
}

// The gin mounting of these route specs lives in
// internal/controlplane/ginroutes.MountAuthRoutes (#285): it consumes RouteSpecs
// and adapts each net/http handler to gin, keeping this core gin-free.
