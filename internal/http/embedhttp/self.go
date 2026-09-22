package embedhttp

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	redis "github.com/redis/go-redis/v9"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/captcha"
	"github.com/open-rails/openrails/internal/db/models"
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
	return NewSelfRoutes(rt, authn, providerRouteOverride, hostResolve).Handler()
}

func NewSelfRoutes(rt *app.Runtime, authn billingauth.DelegatedAuthenticator, providerRouteOverride *routesurface.ProviderRoutes, hostResolve merchant.HostResolver) *router.Table {
	mux := &router.Table{}
	delegatedMW := middleware.DelegatedPrincipalRequired(authn)
	providerRoutes := ProviderRoutesForRuntime(rt, providerRouteOverride)
	httproutes.RegisterSelfServiceRoutes(router.NewMux(mux, EmbeddedV1Prefix+httproutes.SelfRoutePrefix, rt), rt, delegatedMW, providerRoutes)
	httproutes.RegisterCustomerTreasuryRoutes(router.NewMux(mux, EmbeddedV1Prefix+httproutes.CustomerRoutePrefix, rt), rt, delegatedMW, providerRoutes)

	return wrapCustomerRoutes(rt, mux, hostResolve, "")
}

func wrapCustomerRoutes(rt *app.Runtime, mux *router.Table, hostResolve merchant.HostResolver, selfPrefix string) *router.Table {
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
	for i := range mux.Entries {
		mux.Entries[i].Browser = true
	}
	limiter := middleware.RateLimitHTTP(rateLimits, captchaCfg, rdb, captcha.NewChallengeStore(rdb), resolver)
	mux.Wrap(func(entry router.Entry) http.Handler {
		canonical := entry.Path
		if selfPrefix != "" {
			canonical = EmbeddedV1Prefix + "/me" + strings.TrimPrefix(entry.Path, selfPrefix)
		}
		return middleware.ChainHTTP(entry.Handler,
			middleware.WithRoutePath(canonical),
			middleware.RecoverHTTP(),
			middleware.SecurityHeadersHTTP(),
			// #765: this handler's entire surface is browser tier — always the
			// static permissive `*` grant, no per-request source.
			middleware.PermissiveCORSHTTP(middleware.AllRequests),
			middleware.BodyLimitHTTP(middleware.DefaultMaxBodyBytes),
			middleware.HTTPMiddleware(billingauth.ExplicitCredentials),
			// Resolve current authority on each request, including privileged
			// restore/bootstrap integrations that bind after graph construction.
			middleware.ResolveMerchantHTTP(rt.ConfiguredMerchant),
			// #734: a no-op when hostResolve is nil (no control plane attached).
			middleware.ResolveMerchantFromHostHTTP(hostResolve),
			limiter,
		)
	})
	return mux
}

// ProviderRoutesForRuntime derives provider-specific route gating from the
// bound merchant's DB-armed rail accounts and probed capabilities. Explicit
// selections override rail discovery, but cannot enable host-owned credential
// writes or writes unsupported by the secret backend.
func ProviderRoutesForRuntime(rt *app.Runtime, override *routesurface.ProviderRoutes) routesurface.ProviderRoutes {
	r := routesurface.AllProviderRoutes()
	if override != nil {
		r = *override
	} else if rt != nil {
		if mid := rt.ConfiguredMerchant(); !mid.IsZero() && rt.Merchants != nil {
			r = armedProviderRoutes(context.Background(), rt, mid)
			r.SolanaSigning = r.Solana
			r.SecretWrite = true
			if caps := rt.RouteCapabilities; caps != nil {
				r.SolanaSigning = r.Solana && caps.SolanaCanSign
			}
		}
	}
	if rt != nil {
		if rt.Config.IsManifestMerchantConfigSource() {
			r.SecretWrite = false
		}
		if caps := rt.RouteCapabilities; caps != nil {
			r.SecretWrite = r.SecretWrite && caps.SecretWrite
			r.SolanaSigning = r.SolanaSigning && caps.SolanaCanSign
		}
	}
	return r
}

// armedProviderRoutes derives the route surface from mid's DB-armed rail
// accounts (#775) — the mount-time analogue of the per-request DB fallback
// checkoutRailConfigured / effectiveSolanaRailConfig use. This runs ONCE at
// handler-assembly time (not per request), so one query per rail is not a hot
// path concern.
func armedProviderRoutes(ctx context.Context, rt *app.Runtime, mid merchant.ID) routesurface.ProviderRoutes {
	env := config.ExpectedProviderEnvironment(rt.Config != nil && rt.Config.IsTestMode())
	armed := func(rail string) bool {
		_, ok, err := rt.Merchants.ActivePSPScope(ctx, mid, rail, env)
		return err == nil && ok
	}
	stripe := armed(string(models.RailStripe))
	solana := armed(string(models.RailSolana))
	return routesurface.ProviderRoutes{
		StripePortal: stripe,
		Solana:       solana,
		Webhooks:     stripe || armed(string(models.RailNMI)) || armed(string(models.RailCCBill)),
	}
}

// ConfiguredProviderRoutes resolves optional buyer routes without hiding store
// failures. API-owned configurations retain routes as providers are added; each
// request still enforces actual account readiness. Manifest-owned hosts must
// finish provider declaration before materializing their buyer HTTP surface.
func ConfiguredProviderRoutes(ctx context.Context, rt *app.Runtime, buyer bool) (routesurface.ProviderRoutes, error) {
	if rt == nil || rt.Config == nil {
		return routesurface.ProviderRoutes{}, fmt.Errorf("openrails HTTP: runtime configuration is missing")
	}
	selected := routesurface.AllProviderRoutes()
	if !buyer {
		selected.StripePortal = false
		selected.Solana = false
		selected.SolanaSigning = false
	} else if rt.Config.IsManifestMerchantConfigSource() {
		mid := rt.ConfiguredMerchant()
		if mid.IsZero() || rt.Merchants == nil {
			return selected, fmt.Errorf("openrails HTTP: declare the manifest merchant before mounting buyer routes")
		}
		environment := config.ExpectedProviderEnvironment(rt.Config.IsTestMode())
		_, stripe, err := rt.Merchants.ActivePSPScope(ctx, mid, string(models.RailStripe), environment)
		if err != nil {
			return selected, fmt.Errorf("openrails HTTP: resolve Stripe routes: %w", err)
		}
		_, solana, err := rt.Merchants.ActivePSPScope(ctx, mid, string(models.RailSolana), environment)
		if err != nil {
			return selected, fmt.Errorf("openrails HTTP: resolve Solana routes: %w", err)
		}
		selected.StripePortal = stripe
		selected.Solana = solana
		selected.SolanaSigning = solana
	}
	return ProviderRoutesForRuntime(rt, &selected), nil
}
