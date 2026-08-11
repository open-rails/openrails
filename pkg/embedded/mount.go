package embedded

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/http/embedhttp"
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
	mountPrefix, err := normalizeMountPrefix(opts.MountPrefix)
	if err != nil {
		return nil, err
	}

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
	return combinedMount(mountPrefix, base, self, user), nil
}

// normalizeMountPrefix trims trailing slashes (so "/billing/" and "/billing//"
// both mean the canonical "/billing") and requires a leading "/" when
// non-empty — a prefix silently missing its leading slash is far more likely
// a caller bug than an intentional relative mount, so it fails loudly here at
// boot time instead of silently 404ing every request.
func normalizeMountPrefix(prefix string) (string, error) {
	for strings.HasSuffix(prefix, "/") {
		prefix = strings.TrimSuffix(prefix, "/")
	}
	if prefix != "" && !strings.HasPrefix(prefix, "/") {
		return "", fmt.Errorf("embedded billing: MountPrefix %q must start with \"/\"", prefix)
	}
	return prefix, nil
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
	// #913: checked first (static misconfiguration beats runtime state), and
	// the error names the standard bridge — every observed host either
	// hand-rolled this wrong (th#1765) or never wired it and shipped a
	// 404ing self surface (ca#269).
	if authn == nil {
		return nil, fmt.Errorf("embedded billing: RouteSetCustomer (self surface) requires MountOptions.DelegatedAuthenticator — hosts with their own AuthKit verifier use pkg/embedded/authkit.NewDelegatedAuthenticator(verifier, boundMerchantID, opts...); remote-issuer hosts use NewVerifierDelegatedAuthenticator(issuers, audience, boundMerchantID, opts...)")
	}
	if e == nil {
		return nil, fmt.Errorf("embedded billing: not initialized")
	}
	a := e.App()
	if a == nil {
		return nil, fmt.Errorf("embedded billing: not initialized")
	}
	// #734: derive the SAME Host->merchant resolver the base handler uses
	// (embedhttp.FromApp) from whatever control plane is attached — nil when
	// none is, in which case this surface's Host resolution behaves exactly
	// as before this issue. CORS is unrelated since #765: this surface's
	// entire mounted range is browser tier, so it always gets the static
	// permissive policy regardless of control-plane attachment.
	hostResolve := embedhttp.HostMerchantResolverFrom(a.ControlPlane)
	return newSelfHandler(a.Runtime, authn, providerRouteOverride, hostResolve), nil
}

// newSelfHandler delegates to the neutral assembly; kept as the unit-testable
// seam (no live app graph required).
func newSelfHandler(rt *app.Runtime, authn billingauth.DelegatedAuthenticator, providerRouteOverride *routesurface.ProviderRoutes, hostResolve merchant.HostResolver) http.Handler {
	return embedhttp.NewSelfHandler(rt, authn, providerRouteOverride, hostResolve)
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
// handler. Paths not under mountPrefix (including prefix-boundary near-misses
// like "/billingfoo" against "/billing") get a clean 404 instead of a garbage
// rewrite.
func combinedMount(mountPrefix, base string, self, user http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest, ok := stripMountPrefix(r.URL.Path, mountPrefix)
		if !ok {
			http.NotFound(w, r)
			return
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = base + rest
		if r2.URL.RawPath != "" {
			rawRest, ok := stripMountPrefix(r.URL.RawPath, mountPrefix)
			if !ok {
				http.NotFound(w, r)
				return
			}
			r2.URL.RawPath = base + rawRest
		}
		if self != nil && (rest == "/v1/me" || rest == "/v1/customers" ||
			strings.HasPrefix(rest, "/v1/me/") || strings.HasPrefix(rest, "/v1/customers/")) {
			self.ServeHTTP(w, r2)
			return
		}
		user.ServeHTTP(w, r2)
	})
}

// stripMountPrefix removes mountPrefix from path at a "/" boundary (path
// equals mountPrefix exactly, or continues with "/"). Returns ok=false when
// path is not under mountPrefix at all.
func stripMountPrefix(path, mountPrefix string) (rest string, ok bool) {
	if mountPrefix == "" {
		if path == "" {
			return "/", true
		}
		return path, true
	}
	if path == mountPrefix {
		return "/", true
	}
	if strings.HasPrefix(path, mountPrefix+"/") {
		return path[len(mountPrefix):], true
	}
	return "", false
}
