package controlplane

import (
	"net/http"

	authhttp "github.com/open-rails/authkit/authhttp"
)

// excludedAuthRoutes are AuthKit routes OpenRails shadows or withholds. JWKS
// is not served on this surface. The merchant group's settings routes read and
// rename AuthKit's group name, which is not the merchant's name (#1106).
var excludedAuthRoutes = []authhttp.RouteRef{
	{Method: http.MethodGet, Path: authhttp.JWKSPath},
	{Method: http.MethodGet, Path: "/" + string(MerchantType) + "/{instance_slug}"},
	{Method: http.MethodPatch, Path: "/" + string(MerchantType) + "/{instance_slug}"},
}

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
