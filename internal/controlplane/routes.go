package controlplane

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/open-rails/authkit/iam"
	riverhelpers "github.com/open-rails/helpers/river"
)

// IntentionalRouteGroups are the AuthKit route groups OpenRails exposes in
// locked-down / self-hosted mode (issue #224 task 4), deliberately not AuthKit's
// full surface:
//
//   - RouteAuth: discovery, login, refresh, logout, password reset, JWKS.
//   - RouteAccount: self-service account routes (me, sessions, password change).
//   - RoutePermissionGroups: merchant membership and credentials, and explicit
//     customer portal membership, authorized by AuthKit.
//
// Registration, AuthKit's admin surface and browser OIDC are not mounted.
var IntentionalRouteGroups = []iam.RouteGroup{iam.RouteAuth, iam.RouteAccount, iam.RoutePermissionGroups}

// hostedRouteGroups is AuthKit's whole JSON API. Browser OIDC stays unmounted.
var hostedRouteGroups = []iam.RouteGroup{
	iam.RouteAuth, iam.RouteRegistration, iam.RouteAccount, iam.RouteAdmin,
	iam.RoutePermissionGroups, iam.RouteDeviceKeys, iam.RouteDelegated,
}

// MountedRouteGroups is the explicit list of AuthKit route groups this control
// plane mounts (a nil list would mount AuthKit's default surface plus browser
// OIDC).
func (c *ControlPlane) MountedRouteGroups() []iam.RouteGroup {
	if c.SelfHostedPosture() {
		return append([]iam.RouteGroup(nil), IntentionalRouteGroups...)
	}
	return append([]iam.RouteGroup(nil), hostedRouteGroups...)
}

// AuthKit's routes live beneath the issuer's path, AuthKit's base path, or
// beneath /auth when the issuer is an origin. Its JSON API is that prefix plus
// authAPIVersion. JWKS is served at the issuer plus /.well-known/jwks.json.
func authPrefix(issuer string) string {
	if path := issuerPath(issuer); path != "" {
		return path
	}
	return "/auth"
}

// authAPIVersion is the HTTP API version AuthKit appends to its APIPath.
const authAPIVersion = "/v1"

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

// AuthAPIBase is the path AuthKit's JSON API is served at.
func (c *ControlPlane) AuthAPIBase() string { return c.AuthPrefix() + authAPIVersion }

// AuthRoutes is the mounted AuthKit route catalog, each served by AuthHandler.
// A GET route's pattern also serves HEAD.
func (c *ControlPlane) AuthRoutes() []iam.Route {
	if c == nil || c.client == nil {
		return nil
	}
	var out []iam.Route
	for _, route := range c.client.Routes() {
		if route.Method != http.MethodHead {
			out = append(out, route)
		}
	}
	return out
}

// AuthHandler serves AuthKit's mounted HTTP surface.
func (c *ControlPlane) AuthHandler() http.Handler {
	if c == nil || c.client == nil {
		return nil
	}
	return c.client.Handler()
}

// RiverJobs contributes AuthKit's jobs to OpenRails' River fleet.
func (c *ControlPlane) RiverJobs() riverhelpers.Contribution { return c.client.RiverJobs() }
