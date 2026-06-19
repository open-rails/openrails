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
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/embedded"
	"github.com/open-rails/openrails/pkg/merchant"
)

// SelfHandler returns the mountable browser-direct SELF-SERVICE + MERCHANT-ADMIN
// surface for an embedded host (issues #339/#467), authenticated by the
// host-supplied billingauth.DelegatedAuthenticator from Options — one system,
// one credential: the host verifies its own token and returns the explicitly
// mapped {tenant, subject, permissions} principal, exactly like the standalone
// server's host-pluggable mode (internal/http registerSelfServiceRoutes).
//
// Routes are served at the CANONICAL embedded paths, alongside the
// NewHTTPHandler surface:
//
//	/billing/v1/self/*          (RegisterSelfServiceRoutes — openrails:self:*)
//	/billing/v1/merchant-admin/*  (RegisterMerchantAdminRoutes — openrails:merchant:*)
//
// so a host that mounts NewHTTPHandler under /billing without prefix stripping
// can route these two subtrees to this handler and everything else to the
// gin-free handler. The same neutral base middleware stack wraps it (security
// headers, CORS, body limit, default-tenant resolution — the delegated
// middleware then pins the principal's tenant), keeping the two embedded
// handlers behaviorally consistent. Host-supplied delegated authenticators own
// their own browser-origin policy before returning a principal.
//
// This lives in the gin-coupled pkg/embedded/gin subpackage (#285) because the
// self surface registration is gin (ginroutes); the core pkg/embedded handler
// stays gin-free. Errors when the app graph is not initialized or no
// DelegatedAuthenticator was supplied — the surface is identity-gated and is
// never mounted without an identity source (fail closed, mirroring the
// standalone server's "surface not mounted" behavior).
func SelfHandler(e *embedded.Embedded) (http.Handler, error) {
	if e == nil {
		return nil, fmt.Errorf("embedded billing: not initialized")
	}
	a := e.App()
	if a == nil {
		return nil, fmt.Errorf("embedded billing: not initialized")
	}
	if a.DelegatedAuthenticator == nil {
		return nil, fmt.Errorf("embedded billing: self surface requires Options.DelegatedAuthenticator (#339)")
	}
	var configured merchant.ID
	if a.Runtime != nil {
		configured = a.Runtime.ConfiguredMerchant
	}
	return newSelfHandler(a.Runtime, a.DelegatedAuthenticator, configured), nil
}

// newSelfHandler assembles the self + merchant-admin gin engine and wraps it in
// the neutral net/http base middleware stack (the gin-free analogue embedhttp
// uses). Split from SelfHandler so the routing/auth behavior is unit-testable
// without a live app graph.
func newSelfHandler(rt *app.Runtime, authn billingauth.DelegatedAuthenticator, configured merchant.ID) http.Handler {
	engine := gingonic.New()
	engine.Use(gingonic.Recovery())

	delegatedMW := ginmw.DelegatedPrincipalRequired(authn)
	base := engine.Group(embedhttp.EmbeddedV1Prefix)
	ginroutes.RegisterSelfServiceRoutes(base.Group(ginroutes.SelfRoutePrefix), rt, delegatedMW)
	ginroutes.RegisterAdminRoutes(base.Group(ginroutes.AdminRoutePrefix), rt, delegatedMW)

	// OpenRails-native rate-limiting + captcha on the embedded self-service +
	// merchant-admin surface, matching the base NewHTTPHandler chain so an embedded
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
