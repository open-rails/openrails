package controlplane

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/open-rails/authkit/iam"
	riverhelpers "github.com/open-rails/helpers/river"
)

// IntentionalRouteGroups are the AuthKit route groups OpenRails exposes when
// registration is closed, deliberately not AuthKit's full surface:
//
//   - RouteAuth: discovery, login, refresh, logout, password reset, JWKS.
//   - RouteAccount: self-service account routes (me, sessions, password change).
//   - RoutePermissionGroups: merchant membership and credentials, and explicit
//     customer portal membership, authorized by AuthKit.
//
// Registration, AuthKit's admin surface and browser OIDC are not mounted.
var IntentionalRouteGroups = []iam.RouteGroup{iam.RouteAuth, iam.RouteAccount, iam.RoutePermissionGroups}

// registrationRouteGroups is AuthKit's JSON API, mounted when people can
// register (open or invite-only). Browser OIDC stays unmounted.
var registrationRouteGroups = []iam.RouteGroup{
	iam.RouteAuth, iam.RouteRegistration, iam.RouteAccount, iam.RouteAdmin,
	iam.RoutePermissionGroups, iam.RouteDeviceKeys,
}

// MountedRouteGroups is the explicit list of AuthKit route groups this control
// plane mounts (a nil list would mount AuthKit's default surface plus browser
// OIDC).
func (c *ControlPlane) MountedRouteGroups() []iam.RouteGroup {
	if !c.registers() {
		return append([]iam.RouteGroup(nil), IntentionalRouteGroups...)
	}
	return append([]iam.RouteGroup(nil), registrationRouteGroups...)
}

// AuthKit's routes live beneath the issuer's path, AuthKit's base path, or
// beneath /auth when the issuer is an origin. JWKS is served at the issuer
// plus /.well-known/jwks.json.
func authPrefix(issuer string) string {
	if path := issuerPath(issuer); path != "" {
		return path
	}
	return "/auth"
}

// authAPIPath is AuthKit's APIPath: authPrefix beneath AuthKit's base path.
func authAPIPath(issuer string) string {
	if issuerPath(issuer) != "" {
		return "/"
	}
	return "/auth"
}

func issuerPath(issuer string) string {
	u, err := url.Parse(strings.TrimSpace(issuer))
	if err != nil || strings.Trim(u.Path, "/") == "" {
		return ""
	}
	return "/" + strings.Trim(u.Path, "/")
}

// AuthPrefix is the path prefix AuthKit's JSON API is served beneath.
func (c *ControlPlane) AuthPrefix() string {
	if c == nil || c.authPrefix == "" {
		return "/auth"
	}
	return c.authPrefix
}

// AuthAPIBase is the path AuthKit's JSON API is served at: AuthPrefix plus
// AuthKit's version segment. Empty without a control plane.
func (c *ControlPlane) AuthAPIBase() string {
	if c == nil || c.client == nil {
		return ""
	}
	return c.client.APIBase()
}

// AuthRoutes is the mounted AuthKit route catalog, each served by AuthHandler.
// A GET route's pattern also serves HEAD. Without local sign-in only the
// issuer's JWKS is served.
func (c *ControlPlane) AuthRoutes() []iam.Route {
	if c == nil || c.client == nil {
		return nil
	}
	var out []iam.Route
	for _, route := range c.client.Routes() {
		if route.Method == http.MethodHead || (!c.localSignIn && !strings.HasSuffix(route.Path, iam.JWKSPath)) {
			continue
		}
		out = append(out, route)
	}
	return out
}

// LocalSignIn reports whether the control plane serves sign-in to its own
// accounts.
func (c *ControlPlane) LocalSignIn() bool { return c != nil && c.localSignIn }

// AuthHandler serves AuthKit's mounted HTTP surface.
func (c *ControlPlane) AuthHandler() http.Handler {
	if c == nil || c.client == nil {
		return nil
	}
	return c.client.Handler()
}

// RiverJobs contributes AuthKit's jobs to OpenRails' River fleet.
func (c *ControlPlane) RiverJobs() riverhelpers.Contribution { return c.client.RiverJobs() }
