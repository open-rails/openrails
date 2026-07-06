package embedded

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/http/embedhttp"
	"github.com/open-rails/openrails/internal/http/middleware"
	"github.com/open-rails/openrails/internal/http/routesurface"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
)

// MountOptions configures the combined embedded surface (MountHandler).
type MountOptions struct {
	// RouteSets selects the mounted billing surfaces. A zero slice uses the
	// embedded defaults.
	RouteSets []RouteSet
	// Authenticator protects checkout/user routes.
	Authenticator billingauth.Authenticator
	// Gate protects merchant routes.
	Gate billingauth.Gate
	// DelegatedAuthenticator protects /v1/me and /v1/customers routes.
	DelegatedAuthenticator billingauth.DelegatedAuthenticator
	// MountPrefix is the host path the whole surface is mounted under (e.g.
	// "/billing"); incoming paths are MountPrefix + "/v1/...". Empty means
	// paths already arrive canonical ("/v1/...").
	MountPrefix string
	// ProviderRoutes selects provider-specific public routes. Nil derives from
	// the embedded runtime where possible.
	ProviderRoutes *ProviderRoutes
}

// MountHandler returns the selected embedded billing surfaces as ONE
// framework-neutral net/http handler. The host mounts this once (a gin host
// uses gin.WrapH) and rewrites nothing.
func MountHandler(e *Embedded, opts MountOptions) (http.Handler, error) {
	// Resolve + record the full selection (incl. customer); the combined mount
	// serves customer via the self handler, so it is part of the advertised set.
	active := e.MountRouteSets(opts.RouteSets)
	var self http.Handler
	providerRoutes := providerRoutesFromMountOptions(opts.ProviderRoutes)
	if routeSetSelected(active, RouteSetCustomer) {
		var err error
		self, err = selfHandler(e, opts.DelegatedAuthenticator, providerRoutes)
		if err != nil {
			return nil, err
		}
	}
	asm := embedhttp.FromApp(e.App())
	asm.Authenticator = opts.Authenticator
	asm.Gate = opts.Gate
	userRouteSets := routeSetsWithoutCustomer(active)
	user := http.NotFoundHandler()
	if len(userRouteSets) > 0 {
		user = asm.NewHTTPHandler(embedhttp.Options{
			RouteSets:          userRouteSets,
			AdvertiseRouteSets: active,
			ProviderRoutes:     providerRoutes,
		})
	}

	// base is the canonical embedded mount ("/billing"); both handlers serve at
	// base + "/v1/...".
	base := strings.TrimSuffix(embedhttp.EmbeddedV1Prefix, "/v1")
	return combinedMount(opts.MountPrefix, base, self, user), nil
}

// SelfHandler returns the mountable browser-direct SELF-SERVICE surface for an
// embedded host (issues #339/#467), authenticated by a host-supplied
// billingauth.DelegatedAuthenticator.
//
// Routes are served at the CANONICAL embedded paths, alongside the rest of
// the embedded billing surface:
//
//	/billing/v1/me/*                      (self-service)
//	/billing/v1/customers/:customer_id/*  (customer treasury)
//
// so a host that mounts the base embedded surface under /billing without
// prefix stripping can route this subtree to this handler and everything else
// to the base handler. Most hosts should use MountHandler, which does that
// routing itself.
// Errors when the app graph is not initialized or no identity source can be
// derived — the surface is never mounted without authentication.
func SelfHandler(e *Embedded, authn billingauth.DelegatedAuthenticator) (http.Handler, error) {
	return selfHandler(e, authn, nil)
}

func selfHandler(e *Embedded, authn billingauth.DelegatedAuthenticator, providerRouteOverride *routesurface.ProviderRoutes) (http.Handler, error) {
	if e == nil {
		return nil, fmt.Errorf("embedded billing: not initialized")
	}
	a := e.App()
	if a == nil {
		return nil, fmt.Errorf("embedded billing: not initialized")
	}
	if authn == nil {
		return nil, fmt.Errorf("embedded billing: self surface requires MountOptions.DelegatedAuthenticator")
	}
	// #734: derive the SAME Host->merchant resolver + CORS source the base
	// handler uses (embedhttp.FromApp) from whatever control plane is attached
	// — nil/nil when none is, in which case this surface behaves exactly as
	// before this issue.
	hostResolve, corsSource := embedhttp.HostMerchantResolverFrom(a.ControlPlane)
	return newSelfHandler(a.Runtime, authn, providerRouteOverride, hostResolve, corsSource), nil
}

// newSelfHandler delegates to the neutral assembly; kept as the unit-testable
// seam (no live app graph required).
func newSelfHandler(rt *app.Runtime, authn billingauth.DelegatedAuthenticator, providerRouteOverride *routesurface.ProviderRoutes, hostResolve merchant.HostResolver, corsSource middleware.CORSOriginSource) http.Handler {
	return embedhttp.NewSelfHandler(rt, authn, providerRouteOverride, hostResolve, corsSource)
}

func providerRoutesFromMountOptions(opt *ProviderRoutes) *routesurface.ProviderRoutes {
	if opt == nil {
		return nil
	}
	v := opt.internal()
	return &v
}

func routeSetSelected(routeSets []RouteSet, want RouteSet) bool {
	if len(routeSets) == 0 {
		routeSets = EmbeddedDefaultRouteSets
	}
	for _, routeSet := range routeSets {
		if routeSet == want {
			return true
		}
	}
	return false
}

func routeSetsWithoutCustomer(routeSets []RouteSet) []RouteSet {
	if len(routeSets) == 0 {
		routeSets = EmbeddedDefaultRouteSets
	}
	out := make([]RouteSet, 0, len(routeSets))
	for _, routeSet := range routeSets {
		if routeSet != RouteSetCustomer {
			out = append(out, routeSet)
		}
	}
	return out
}

// combinedMount strips mountPrefix, rewrites to the canonical base, and routes
// selected customer subtrees to the self handler; everything else to the user
// handler.
func combinedMount(mountPrefix, base string, self, user http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, mountPrefix)
		if rest == "" {
			rest = "/"
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = base + rest
		if r2.URL.RawPath != "" {
			r2.URL.RawPath = base + strings.TrimPrefix(r.URL.RawPath, mountPrefix)
		}
		if self != nil && (rest == "/v1/me" || rest == "/v1/customers" ||
			strings.HasPrefix(rest, "/v1/me/") || strings.HasPrefix(rest, "/v1/customers/")) {
			self.ServeHTTP(w, r2)
			return
		}
		user.ServeHTTP(w, r2)
	})
}
