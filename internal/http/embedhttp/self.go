package embedhttp

import (
	"net/http"

	redis "github.com/redis/go-redis/v9"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/captcha"
	"github.com/open-rails/openrails/internal/http/middleware"
	"github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/internal/http/routesurface"
	"github.com/open-rails/openrails/internal/shared/iputil"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
)

// NewSelfHandler assembles the embedded browser-direct SELF-SERVICE surface
// (#339/#467) as a gin-free net/http handler:
//
//	/billing/v1/me/*                      (RegisterSelfServiceRoutes)
//	/billing/v1/customers/:customer_id/*  (RegisterCustomerTreasuryRoutes)
//
// authenticated by the host-supplied billingauth.DelegatedAuthenticator. The
// same neutral base middleware stack wraps it (recovery, security headers,
// CORS, body limit, merchant resolution, rate-limit + captcha), keeping it
// behaviorally consistent with the base embedded handler. Host-supplied
// delegated authenticators own their own browser-origin policy.
//
// hostResolve is the #734 Host->merchant mechanism (nil for hosts with no
// control plane attached — HostMerchantResolverFrom derives it, and
// mount.go's selfHandler is the sole caller). This handler's ENTIRE mounted
// surface (self-service + customer-treasury) is browser tier, so it always
// gets the #765 static permissive CORS policy unconditionally — no
// control-plane/source dependency, unlike hostResolve.
func NewSelfHandler(rt *app.Runtime, authn billingauth.DelegatedAuthenticator, providerRouteOverride *routesurface.ProviderRoutes, hostResolve merchant.HostResolver) http.Handler {
	mux := http.NewServeMux()
	delegatedMW := middleware.DelegatedPrincipalRequired(authn)
	providerRoutes := ProviderRoutesForRuntime(rt, providerRouteOverride)
	httproutes.RegisterSelfServiceRoutes(router.NewMux(mux, EmbeddedV1Prefix+httproutes.SelfRoutePrefix, rt), rt, delegatedMW, providerRoutes)
	httproutes.RegisterCustomerTreasuryRoutes(router.NewMux(mux, EmbeddedV1Prefix+httproutes.CustomerRoutePrefix, rt), rt, delegatedMW, providerRoutes)

	// OpenRails-native rate-limiting + captcha, matching the base NewHTTPHandler
	// chain. IP-keyed: the delegated principal is pinned per-route inside the mux,
	// after this outer chain — exactly like the standalone self surface.
	var rateLimits *config.RateLimitsConfig
	var captchaCfg *config.CaptchaConfig
	var rdb *redis.Client
	var resolver *iputil.TrustedProxies
	if rt != nil {
		rdb = rt.RedisClient
		resolver = rt.TrustedProxies
		if rt.Config != nil {
			rateLimits = rt.Config.RateLimits
			captchaCfg = rt.Config.Captcha
		}
	}
	return middleware.ChainHTTP(mux,
		middleware.RecoverHTTP(),
		middleware.SecurityHeadersHTTP(),
		// #765: this handler's entire surface is browser tier — always the
		// static permissive `*` grant, no per-request source.
		middleware.PermissiveCORSHTTP(middleware.AllRequests),
		middleware.BodyLimitHTTP(middleware.DefaultMaxBodyBytes),
		// Resolved PER REQUEST off the Runtime (#744) — never a value snapshotted
		// here at construction time, so a mount that races UpsertMerchantConfig's
		// post-boot bind still resolves correctly on every request.
		middleware.ResolveMerchantHTTP(rt.ConfiguredMerchant),
		// #734: a no-op when hostResolve is nil (no control plane attached).
		middleware.ResolveMerchantFromHostHTTP(hostResolve),
		middleware.RateLimitHTTP(rateLimits, captchaCfg, rdb, captcha.NewChallengeStore(rdb), resolver),
	)
}

// ProviderRoutesForRuntime derives provider-specific route gating from the
// runtime's configured rails + probed capabilities (#661); an explicit override
// wins; no runtime information means the permissive default.
func ProviderRoutesForRuntime(rt *app.Runtime, override *routesurface.ProviderRoutes) routesurface.ProviderRoutes {
	if override != nil {
		return *override
	}
	if rt != nil {
		if len(rt.Rails) > 0 || !rt.ConfiguredMerchant().IsZero() {
			if caps := rt.RouteCapabilities; caps != nil {
				return routesurface.ProviderRoutesFromRailsWithCapabilities(rt.Rails, *caps)
			}
			return routesurface.ProviderRoutesFromRails(rt.Rails)
		}
	}
	return routesurface.AllProviderRoutes()
}
