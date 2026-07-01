package gin

import (
	"fmt"
	"net/http"

	gingonic "github.com/gin-gonic/gin"
	redis "github.com/redis/go-redis/v9"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/captcha"
	"github.com/open-rails/openrails/internal/http/embedhttp"
	"github.com/open-rails/openrails/internal/http/middleware"
	ginmw "github.com/open-rails/openrails/internal/http/middleware/ginmw"
	"github.com/open-rails/openrails/internal/http/routes/ginroutes"
	"github.com/open-rails/openrails/internal/http/routesurface"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/embedded"
	"github.com/open-rails/openrails/pkg/merchant"
)

// SelfHandler returns the mountable browser-direct SELF-SERVICE
// surface for an embedded host (issues #339/#467). A host-supplied
// billingauth.DelegatedAuthenticator is supplied by the host at mount time.
//
// Routes are served at the CANONICAL embedded paths, alongside the
// NewHTTPHandler surface:
//
//	/billing/v1/me/*                      (RegisterSelfServiceRoutes)
//	/billing/v1/customers/:customer_id/*  (RegisterCustomerTreasuryRoutes)
//
// so a host that mounts NewHTTPHandler under /billing without prefix stripping
// can route this subtree to this handler and everything else to the
// gin-free handler. The same neutral base middleware stack wraps it (security
// headers, CORS, body limit, default merchant resolution — the delegated
// middleware then pins the principal's merchant), keeping the two embedded
// handlers behaviorally consistent. Host-supplied delegated authenticators own
// their own browser-origin policy before returning a principal.
//
// This lives in the gin-coupled pkg/embedded/gin subpackage (#285) because the
// self surface registration is gin (ginroutes); the core pkg/embedded handler
// stays gin-free. Errors when the app graph is not initialized or no identity
// source can be derived — the surface is never mounted without authentication.
func SelfHandler(e *embedded.Embedded, authn billingauth.DelegatedAuthenticator) (http.Handler, error) {
	return selfHandler(e, authn, nil)
}

func selfHandler(e *embedded.Embedded, authn billingauth.DelegatedAuthenticator, providerRouteOverride *routesurface.ProviderRoutes) (http.Handler, error) {
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
	var configured merchant.ID
	if a.Runtime != nil {
		configured = a.Runtime.ConfiguredMerchant
	}
	return newSelfHandler(a.Runtime, authn, configured, providerRouteOverride), nil
}

// newSelfHandler assembles the self-service gin engine and wraps it in
// the neutral net/http base middleware stack (the gin-free analogue embedhttp
// uses). Split from SelfHandler so the routing/auth behavior is unit-testable
// without a live app graph.
func newSelfHandler(rt *app.Runtime, authn billingauth.DelegatedAuthenticator, configured merchant.ID, providerRouteOverride *routesurface.ProviderRoutes) http.Handler {
	engine := gingonic.New()
	engine.Use(gingonic.Recovery())

	delegatedMW := ginmw.DelegatedPrincipalRequired(authn)
	base := engine.Group(embedhttp.EmbeddedV1Prefix)
	providerRoutes := providerRoutesForRuntime(rt, providerRouteOverride)
	ginroutes.RegisterSelfServiceRoutesWithProviderRoutes(base.Group(ginroutes.SelfRoutePrefix), rt, delegatedMW, providerRoutes)
	ginroutes.RegisterCustomerTreasuryRoutesWithProviderRoutes(base.Group(ginroutes.CustomerRoutePrefix), rt, delegatedMW, providerRoutes)

	// OpenRails-native rate-limiting + captcha on the embedded self-service
	// surface, matching the base NewHTTPHandler chain so an embedded
	// host does not have to front it with its own gateway. It is IP-keyed: the
	// delegated principal is pinned per-route (inside the gin engine, after this
	// outer chain), exactly like the standalone self surface, whose global limiter
	// also runs before the per-route delegated middleware. Config + Redis are read
	// off the runtime; nil (e.g. unit tests) makes the limiter a pass-through.
	var rateLimits *config.RateLimitsConfig
	var captchaCfg *config.CaptchaConfig
	var rdb *redis.Client
	if rt != nil {
		rdb = rt.RedisClient
		if rt.Config != nil {
			rateLimits = rt.Config.RateLimits
			captchaCfg = rt.Config.Captcha
		}
	}
	return middleware.ChainHTTP(engine,
		middleware.SecurityHeadersHTTP(),
		middleware.CORSHTTP(nil),
		middleware.BodyLimitHTTP(middleware.DefaultMaxBodyBytes),
		middleware.ResolveMerchantHTTP(configured),
		middleware.RateLimitHTTP(rateLimits, captchaCfg, rdb, captcha.NewChallengeStore(rdb)),
	)
}

func providerRoutesForRuntime(rt *app.Runtime, override *routesurface.ProviderRoutes) routesurface.ProviderRoutes {
	if override != nil {
		return *override
	}
	if rt != nil {
		if len(rt.Rails) > 0 || !rt.ConfiguredMerchant.IsZero() {
			return routesurface.ProviderRoutesFromRails(rt.Rails)
		}
	}
	return routesurface.AllProviderRoutes()
}
