package embedhttp

import (
	"context"
	"fmt"
	"net/http"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/abusestate"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/captcha"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/http/middleware"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/http/routesurface"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/shared/iputil"
)

// wrapCustomerRoutes applies the browser-tier base chain to the customer
// routes: permissive CORS, body limits, credential admission, merchant
// resolution and the rate limiter. Each route's own gate runs inside it.
func wrapCustomerRoutes(rt *app.Runtime, mux *router.Table, hostResolve merchant.HostResolver) *router.Table {
	// OpenRails-native rate-limiting + captcha, matching the base NewHTTPHandler
	// chain. IP-keyed: the delegated principal is pinned per-route inside the mux,
	// after this outer chain — exactly like the standalone self surface.
	var rateLimits *config.RateLimitsConfig
	var captchaCfg *config.CaptchaConfig
	var resolver *iputil.TrustedProxies
	var store *captcha.ChallengeStore
	var state *abusestate.Store
	if rt != nil {
		store = rt.CaptchaStore
		state = rt.AbuseState
		resolver = rt.TrustedProxies
		if rt.Config != nil {
			rateLimits = rt.Config.RateLimits
			captchaCfg = rt.Config.Captcha
		}
	}
	for i := range mux.Entries {
		mux.Entries[i].Browser = true
	}
	limiter := middleware.RateLimitHTTP(rateLimits, captchaCfg, state, store, resolver)
	mux.Wrap(func(entry router.Entry) http.Handler {
		return middleware.ChainHTTP(entry.Handler,
			middleware.WithRoutePath(embeddedMount+entry.Path),
			middleware.RecoverHTTP(),
			middleware.SecurityHeadersHTTP(),
			// #765: this handler's entire surface is browser tier — always the
			// static permissive `*` grant, no per-request source.
			middleware.PermissiveCORSHTTP(middleware.AllRequests),
			middleware.RequestLimitsHTTP(middleware.DefaultMaxBodyBytes),
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
		if config.SecretStoreBackend(rt.Config) == config.SecretBackendSnapshot {
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
func armedProviderRoutes(ctx context.Context, rt *app.Runtime, mid billing.MerchantID) routesurface.ProviderRoutes {
	env := config.ExpectedProviderEnvironment(rt.Config != nil && config.IsTestMode(rt.Config))
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

// ConfiguredProviderRoutes is the provider routes a mount serves, whatever
// custody its credentials are in. Each request checks its selected account
// and current credential readiness; mounting a route never arms an account or
// grants secret access.
func ConfiguredProviderRoutes(ctx context.Context, rt *app.Runtime) (routesurface.ProviderRoutes, error) {
	if rt == nil || rt.Config == nil {
		return routesurface.ProviderRoutes{}, fmt.Errorf("openrails HTTP: runtime configuration is missing")
	}
	selected := routesurface.AllProviderRoutes()
	return ProviderRoutesForRuntime(rt, &selected), nil
}
