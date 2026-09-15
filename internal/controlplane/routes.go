package controlplane

import (
	"net/http"

	authhttp "github.com/open-rails/authkit/authhttp"
)

// IntentionalRouteGroups is the set of AuthKit route groups OpenRails
// intentionally exposes in locked-down / self-hosted mode (issue #224 task 4).
//
// It deliberately EXCLUDES the full DefaultAPI surface. We mount only the
// login/session/user/JWKS-adjacent capabilities and declared group-management
// routes OpenRails needs:
//
//   - RouteAuth: public AuthKit discovery plus login, refresh, logout, password reset.
//   - RouteAccount: self-service account routes (me, sessions, password change).
//   - RoutePermissionGroups: merchant membership/credentials and explicitly
//     created customer portal membership; AuthKit applies the declared group
//     authorizer. Customer groups expose no machine credentials.
//
// NOT mounted by default in locked-down mode:
//   - RouteRegister (public user self-registration — disabled in self-hosted).
//   - RouteAdmin (AuthKit's own admin surface — OpenRails owns admin routes).
//   - RouteBrowserOIDC (browser redirects mount separately when enabled).
var IntentionalRouteGroups = []authhttp.RouteGroup{
	authhttp.RouteAuth,
	authhttp.RouteAccount,
	authhttp.RoutePermissionGroups,
}

// MountedRouteGroups returns the AuthKit route groups this control plane
// mounts, always as an EXPLICIT allow-list (a nil MountOptions.Groups would
// mount AuthKit's default surface plus browser OIDC). Locked-down mode returns
// the intentional subset; hosted-SaaS posture returns the groups present in
// AuthKit's DefaultAPI surface — still never browser OIDC or JWKS.
func (c *ControlPlane) MountedRouteGroups() []authhttp.RouteGroup {
	if c == nil {
		return nil
	}
	if c.SelfHostedPosture() {
		out := make([]authhttp.RouteGroup, len(IntentionalRouteGroups))
		copy(out, IntentionalRouteGroups)
		return out
	}
	if c.authSvc == nil {
		return nil
	}
	var out []authhttp.RouteGroup
	seen := map[authhttp.RouteGroup]bool{}
	for _, spec := range c.authSvc.APIRoutes() {
		if !seen[spec.Group] {
			seen[spec.Group] = true
			out = append(out, spec.Group)
		}
	}
	return out
}

// RouteSpecs returns the concrete AuthKit route specs the control plane
// serves (the posture's groups). The HTTP layer mounts
// this exact surface via authhttp.MountHandler (#250) with the same
// MountedRouteGroups + WrapAuthRoute inputs; the route-surface test pins the
// two against each other.
func (c *ControlPlane) RouteSpecs() []authhttp.RouteSpec {
	if c == nil || c.authSvc == nil {
		return nil
	}
	groups := c.MountedRouteGroups()
	if len(groups) == 0 {
		return nil
	}
	specs := c.authSvc.APIRoutes(groups...)
	out := make([]authhttp.RouteSpec, len(specs))
	copy(out, specs)
	for i := range out {
		out[i].Handler = c.WrapAuthRoute(out[i], out[i].Handler)
	}
	return out
}

// WrapAuthRoute attaches the merchant directory row around explicit hosted
// merchant creation. Ordinary auth/billing requests never create portal groups.
func (c *ControlPlane) WrapAuthRoute(spec authhttp.RouteSpec, h http.Handler) http.Handler {
	if c != nil && spec.Group == authhttp.RoutePermissionGroups && c.merchantCreation != nil && spec.Method == http.MethodPost && spec.Path == "/"+string(MerchantType) {
		return c.merchantCreationAttachHandler(h)
	}
	return h
}
