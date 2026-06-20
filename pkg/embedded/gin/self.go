package gin

import (
	"context"
	"fmt"
	"net/http"

	gingonic "github.com/gin-gonic/gin"
	redis "github.com/redis/go-redis/v9"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/captcha"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/http/embedhttp"
	"github.com/open-rails/openrails/internal/http/middleware"
	ginmw "github.com/open-rails/openrails/internal/http/middleware/ginmw"
	"github.com/open-rails/openrails/internal/http/routes/ginroutes"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/embedded"
	"github.com/open-rails/openrails/pkg/merchant"
)

// SelfHandler returns the mountable browser-direct SELF-SERVICE + MERCHANT-ADMIN
// surface for an embedded host (issues #339/#467). A host-supplied
// billingauth.DelegatedAuthenticator wins when present. Otherwise OpenRails
// adapts the embedded billingauth.Authenticator into a self-service principal
// pinned to the configured embedded merchant and carrying the full
// openrails:self:* permission catalog.
//
// Routes are served at the CANONICAL embedded paths, alongside the
// NewHTTPHandler surface:
//
//	/billing/v1/me/*            (RegisterSelfServiceRoutes — openrails:self:*)
//	/billing/v1/admin/*         (RegisterAdminRoutes — openrails:merchant:*)
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
// stays gin-free. Errors when the app graph is not initialized or no identity
// source can be derived — the surface is never mounted without authentication.
func SelfHandler(e *embedded.Embedded) (http.Handler, error) {
	if e == nil {
		return nil, fmt.Errorf("embedded billing: not initialized")
	}
	a := e.App()
	if a == nil {
		return nil, fmt.Errorf("embedded billing: not initialized")
	}
	var configured merchant.ID
	if a.Runtime != nil {
		configured = a.Runtime.ConfiguredMerchant
	}
	authn := a.DelegatedAuthenticator
	if authn == nil {
		authn = delegatedAuthenticatorFromUserAuthenticator(a.Authenticator, configured)
	}
	if authn == nil {
		return nil, fmt.Errorf("embedded billing: self surface requires Options.Authenticator plus configured merchant, or Options.DelegatedAuthenticator")
	}
	return newSelfHandler(a.Runtime, authn, configured), nil
}

func delegatedAuthenticatorFromUserAuthenticator(authn billingauth.Authenticator, configured merchant.ID) billingauth.DelegatedAuthenticator {
	if authn == nil || configured.IsZero() {
		return nil
	}
	return billingauth.DelegatedAuthenticatorFunc(func(ctx context.Context, r *http.Request) (*billingauth.DelegatedPrincipal, error) {
		uc, err := authn.Authenticate(ctx, r)
		if err != nil {
			return nil, err
		}
		if err := uc.ValidateSubject(); err != nil {
			return nil, err
		}
		return &billingauth.DelegatedPrincipal{
			MerchantID:    configured.String(),
			SubjectID:     uc.UserID,
			Issuer:        "embedded",
			Permissions:   controlplane.SelfCatalogNames(),
			Email:         uc.Email,
			EmailVerified: uc.EmailVerified,
			Username:      uc.Username,
		}, nil
	})
}

// newSelfHandler assembles the self + delegated-admin gin engine and wraps it in
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
	// delegated-admin surface, matching the base NewHTTPHandler chain so an embedded
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
