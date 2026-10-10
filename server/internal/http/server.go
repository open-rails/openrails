package server

import (
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
	"sync/atomic"

	httproutes "github.com/open-rails/openrails/internal/http/routes"

	"github.com/open-rails/openrails/internal/http/router"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/abusestate"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/captcha"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/http/middleware"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/merchanttarget"
	"github.com/open-rails/openrails/internal/shared/iputil"
	"github.com/open-rails/openrails/server/internal/controlplane"
	"github.com/open-rails/openrails/server/internal/hostconfig"
)

type Dependencies struct {
	Config  *config.Config
	Runtime *app.Runtime
	// Authenticator is the framework-neutral auth boundary; billingauth.Optional
	// wraps it as the best-effort global middleware (#282/#670).
	Authenticator billingauth.SessionAuthenticator
	// ControlPlane is the standalone server's control plane over its own
	// AuthKit. Required: the server selectively mounts the intentional AuthKit
	// route groups (never DefaultAPI in locked-down mode).
	ControlPlane *controlplane.ControlPlane
	// Issuer is the control plane's issuer: its users' and API keys'.
	Issuer string
	// ResourceServer is the trusted issuers the admin API accepts, and
	// ConsoleIssuer the one of them the admin console signs staff in at.
	ResourceServer *hostconfig.ResourceServerConfig
	ConsoleIssuer  *hostconfig.ConsoleIssuer
	// ConsoleAssets is the built admin console SPA (#754: the engine ships no
	// frontend bytes; whoever builds the binary owns the embed). nil = absent.
	// The console mounts only when this is present AND admin_console.enabled;
	// enabled without assets is a boot error.
	ConsoleAssets fs.FS
	// RouteGroups turns the merchant route groups on.
	RouteGroups config.RouteGroups
	// AdminConsole serves the merchant admin console; nil serves none.
	AdminConsole *config.ConsoleMount
}

type Server struct {
	cfg     *config.Config
	runtime *app.Runtime
	// draining fails readiness while the process stops (Drain).
	draining atomic.Bool
	// authenticator is the framework-neutral auth boundary (issue #282/#670 —
	// there is no gin auth provider any more; every surface uses this directly).
	authenticator billingauth.SessionAuthenticator
	controlPlane  *controlplane.ControlPlane
	issuer        string
	// resourceServer and consoleIssuer are Dependencies'.
	resourceServer *hostconfig.ResourceServerConfig
	consoleIssuer  *hostconfig.ConsoleIssuer
	// customerResolver replaces the control plane's openrails:self token
	// verification (tests).
	customerResolver httproutes.ResourceCustomerResolver
	captchaStore     *captcha.ChallengeStore
	adminLimiter     *middleware.AdminOperationLimiter
	// consoleAssets is the host/binary-supplied admin console build (#754).
	consoleAssets fs.FS
	adminConsole  *config.ConsoleMount
	// groups are the route groups the server mounts; permissions their
	// staff permissions.
	groups      config.RouteGroups
	permissions httproutes.Permissions

	// merchants is the merchant provisioning + lifecycle + per-merchant secret service
	// (issue #225). It reuses the control plane's pgx pool (the openrails.*
	// control-plane DB) and permission-group provisioner, and is always built
	// (#469: the control plane is mandatory on this surface).
	merchants *merchants.Service

	// browserTierRoutes tracks which registered patterns belong to the
	// permissive-CORS browser tier (#765: checkout + self-service)
	// — populated as registerUserRoutesAt/
	// registerSelfServiceRoutes mount their routes, consulted by
	// wrapPublicHandler's PermissiveCORSHTTP. Never nil once New() has run.
	browserTierRoutes *middleware.BrowserTierRoutes
	// merchantTierRoutes are the admin API's patterns, which trusted
	// issuers' origins may call cross-origin (#1140).
	merchantTierRoutes *middleware.BrowserTierRoutes

	// publicHandler is the single "full surface" HTTP handler: health + user +
	// self/customer + merchant + control-plane auth + webhook routes AND the
	// API-key-authenticated server-to-server service routes (issue #222). It is
	// the framework-neutral net/http mux wrapped in the neutral middleware chain
	// (#670 — the standalone gin stack is gone; one HTTP stack everywhere).
	publicHandler http.Handler

	// routeTable records every registered route pattern (ServeMux syntax), the
	// standalone surface's introspectable route table for the #670 parity test.
	routeTable      []string
	nativeRoutes    *router.Table
	nativeBrowser   map[string]bool
	sharedRateLimit middleware.HTTPMiddleware
}

// recordRoute appends a registered pattern to the server's route table.
func (s *Server) recordRoute(pattern string) {
	s.routeTable = append(s.routeTable, pattern)
}

// recordBrowserRoute is recordRoute plus browser-tier CORS registration
// (#765): used ONLY by the checkout + self-service
// registration call sites, so PermissiveCORSHTTP grants the static `*` policy
// on exactly those routes and nothing else. Lazily initializes
// browserTierRoutes so hand-built *Server{} unit tests that skip New() still
// behave correctly.
func (s *Server) recordBrowserRoute(pattern string) {
	if s.nativeBrowser == nil {
		s.nativeBrowser = map[string]bool{}
	}
	s.nativeBrowser[pattern] = true
	s.recordRoute(pattern)
	if s.browserTierRoutes == nil {
		s.browserTierRoutes = middleware.NewBrowserTierRoutes()
	}
	s.browserTierRoutes.Add(pattern)
}

// recordMerchantRoute is recordRoute for a admin API route: trusted
// issuers' origins may call it cross-origin (#1140).
func (s *Server) recordMerchantRoute(pattern string) {
	s.recordRoute(pattern)
	if s.merchantTierRoutes == nil {
		s.merchantTierRoutes = middleware.NewBrowserTierRoutes()
	}
	s.merchantTierRoutes.Add(pattern)
}

// allowedIssuerOrigin reports whether a trusted issuer declared origin.
func (s *Server) allowedIssuerOrigin(origin string) bool {
	return s != nil && s.controlPlane != nil && s.controlPlane.AllowedOrigin(origin)
}

// RouteTable returns the registered route surface (guard tests).
func (s *Server) RouteTable() []string {
	out := make([]string, len(s.routeTable))
	copy(out, s.routeTable)
	return out
}

// handle registers a plain net/http handler on the mux and records it.
func (s *Server) handle(mux router.Registrar, pattern string, h http.Handler) {
	s.recordRoute(pattern)
	mux.Handle(pattern, h)
}

// handleBrowser is handle plus browser-tier CORS registration (#765): use for
// raw net/http handlers (not routed through router.Router) that belong to the
// checkout/self-service surface, e.g. the captcha discovery endpoints
// registered alongside RegisterUserRoutes.
func (s *Server) handleBrowser(mux router.Registrar, pattern string, h http.Handler) {
	if s.nativeBrowser == nil {
		s.nativeBrowser = map[string]bool{}
	}
	s.nativeBrowser[pattern] = true
	s.handle(mux, pattern, h)
	if s.browserTierRoutes == nil {
		s.browserTierRoutes = middleware.NewBrowserTierRoutes()
	}
	s.browserTierRoutes.Add(pattern)
}

// New assembles the standalone surface over the engine's initialized runtime
// and the server's control plane.
func New(deps Dependencies) (*Server, error) {
	if deps.Config == nil {
		return nil, fmt.Errorf("server config is required")
	}
	if deps.Runtime == nil {
		return nil, fmt.Errorf("server runtime is required")
	}
	if deps.Runtime.DB == nil {
		return nil, fmt.Errorf("server runtime DB is required")
	}
	if deps.Runtime.Clock == nil {
		return nil, fmt.Errorf("server runtime clock is required")
	}
	if deps.Runtime.PaymentService == nil {
		return nil, fmt.Errorf("server runtime payment service is required")
	}
	if deps.Runtime.CheckoutService == nil {
		return nil, fmt.Errorf("server runtime checkout service is required")
	}
	if deps.Runtime.CheckoutAttemptService == nil {
		return nil, fmt.Errorf("server runtime checkout attempt service is required")
	}
	if deps.Runtime.SubscriptionService == nil {
		return nil, fmt.Errorf("server runtime subscription service is required")
	}
	if deps.Runtime.UserSubscriptionService == nil {
		return nil, fmt.Errorf("server runtime user subscription service is required")
	}
	if deps.Runtime.AdminSubscriptionService == nil {
		return nil, fmt.Errorf("server runtime admin subscription service is required")
	}
	if deps.Runtime.PaymentMethodService == nil {
		return nil, fmt.Errorf("server runtime payment method service is required")
	}
	if deps.Runtime.RailPaymentMethodService == nil {
		return nil, fmt.Errorf("server runtime payment method service is required")
	}
	if deps.Runtime.RailCustomerService == nil {
		return nil, fmt.Errorf("server runtime rail customer service is required")
	}
	if deps.Runtime.RiverProducer == nil {
		return nil, fmt.Errorf("server runtime river producer is required")
	}
	if deps.Authenticator == nil {
		return nil, fmt.Errorf("authenticator is required")
	}
	if deps.ControlPlane == nil {
		return nil, fmt.Errorf("control plane is required")
	}
	if deps.ControlPlane.Pool() == nil {
		return nil, fmt.Errorf("control plane pool is required")
	}

	s := &Server{
		cfg:                deps.Config,
		runtime:            deps.Runtime,
		authenticator:      deps.Authenticator,
		controlPlane:       deps.ControlPlane,
		issuer:             deps.Issuer,
		resourceServer:     deps.ResourceServer,
		consoleIssuer:      deps.ConsoleIssuer,
		captchaStore:       deps.Runtime.CaptchaStore,
		adminLimiter:       middleware.NewAdminOperationLimiter(deps.Runtime.AbuseState),
		consoleAssets:      deps.ConsoleAssets,
		adminConsole:       deps.AdminConsole,
		groups:             deps.RouteGroups,
		permissions:        staffPermissionsFor(deps.RouteGroups),
		browserTierRoutes:  middleware.NewBrowserTierRoutes(),
		merchantTierRoutes: middleware.NewBrowserTierRoutes(),
	}

	// Build the merchant provisioning/lifecycle/secret service (issue #225). It
	// uses the runtime data pool for request-time secret custody, matching the
	// merchant pin, and the control plane for provisioning. MODE 1 (#723,
	// merchant_config_source=manifest) serves read-only provider credentials from the
	// manifest and operator webhook URLs from managed encrypted storage. Provider
	// write routes retain their manifest_driven 405. MODE 2 uses managed storage.
	// The Runtime owns credential construction and lifetime; HTTP assembly
	// reuses its service instead of opening another store.
	if err := deps.Runtime.EnsureMerchantsService(context.Background()); err != nil {
		return nil, err
	}
	s.merchants = deps.Runtime.Merchants

	// Single (standalone-friendly) HTTP surface on the framework-neutral
	// net/http mux (#670): the same stack the embedded surface serves.
	mux := &router.Table{}
	// Standalone mode owns service-level health/meta routes.
	s.registerStandaloneMetaRoutes(mux)
	// Canonical: /v1/*
	s.registerUserRoutes(mux)
	// #555/#561: merchant/support routes live only under `/v1/admin/*`.
	s.registerMerchantActionRoutes(mux)

	// Selective AuthKit route mounting (#224, authhttp.MountHandler since
	// authkit #250). In locked-down mode this mounts ONLY the intentional
	// AuthKit route groups (login/session/user) under /auth — never AuthKit
	// DefaultAPI.
	if err := s.registerControlPlaneAuthRoutes(mux); err != nil {
		return nil, err
	}

	// Browser-direct self-service API: delegated-access-token-authenticated, on
	// the SAME public surface (issue #222 browser tier). Always mounted (#469);
	// a host-supplied DelegatedAuthenticator overrides the control plane's
	// delegated-token verifier (#339).
	s.registerSelfServiceRoutes(mux)

	// Canonical account-specific webhook surface: /v1/webhooks/:provider/:account_id for
	// direct Stripe. The handler resolves the PSP from the payload/route, derives
	// the owning merchant from that globally-unique account row, and verifies the signature
	// with THAT account's secret. This is the canonical multi-merchant shape.
	s.registerWebhookRoutes(mux)
	// or#893: the merchant-scoped alias (/v1/merchants/:merchant/webhooks/...,
	// #529) is NOT mounted here. It was a transition alias beside the canonical
	// surface above; standalone resolves the merchant from PSP
	// identity, so a URL slug is a second way to say the same thing. Embedded
	// hosts still mount it — a pinned merchant has no payload-derived identity
	// to resolve — via internal/http/embedhttp.

	// Merchant admin console SPA (#740/#754): mounted only when assets are
	// present AND admin_console.enabled; enabled without assets refuses boot.
	// Last, so it can refuse a path that overlaps any route above (#1127).
	if err := s.registerAdminConsoleRoutes(mux); err != nil {
		return nil, err
	}

	// A request that names its merchant has it resolved before its route
	// authorizes, on the served handler and on the exported table alike.
	router.ResolveMerchantSelectors(mux, "", func(ctx context.Context, r *http.Request) (billingauth.Target, error) {
		return merchanttarget.Resolve(ctx, r, s.runtime.Merchants, s.runtime.ConfiguredMerchant(), "")
	})

	s.sharedRateLimit = middleware.RateLimitHTTP(s.cfg.RateLimits, s.cfg.Captcha, s.abuseState(), s.captchaStore, s.trustedProxies())
	s.publicHandler = s.wrapPublicHandler(mux.Handler())
	s.nativeRoutes = &router.Table{}
	for _, entry := range mux.Entries {
		switch entry.Path {
		case "/health/live", "/health/ready":
			continue
		}
		entry.Browser = s.nativeBrowser[entry.Method+" "+entry.Path]
		s.nativeRoutes.Entries = append(s.nativeRoutes.Entries, entry)
	}
	s.nativeRoutes.Wrap(func(entry router.Entry) http.Handler {
		return middleware.WithRoutePath(entry.Path)(s.wrapHandler(entry.Handler, func(*http.Request) bool { return entry.Browser }))
	})

	log.Info("Billing service initialized successfully")
	return s, nil
}

// abuseState is the runtime's abuse state, nil-safe like trustedProxies.
func (s *Server) abuseState() *abusestate.Store {
	if s == nil || s.runtime == nil {
		return nil
	}
	return s.runtime.AbuseState
}

// trustedProxies returns the #746 client-IP resolver, nil-safe against a
// Server built without New() (some unit tests construct &Server{} directly).
func (s *Server) trustedProxies() *iputil.TrustedProxies {
	if s == nil || s.runtime == nil {
		return nil
	}
	return s.runtime.TrustedProxies
}

// wrapPublicHandler applies the global middleware chain — the neutral analogue
// (and successor, #670) of the old gin engine's global middleware, same order.
func (s *Server) wrapPublicHandler(mux http.Handler) http.Handler {
	return s.wrapHandler(mux, s.browserTierRoutes.Match)
}

func (s *Server) wrapHandler(next http.Handler, browser func(*http.Request) bool) http.Handler {
	limiter := s.sharedRateLimit
	if limiter == nil {
		limiter = middleware.RateLimitHTTP(s.cfg.RateLimits, s.cfg.Captcha, s.abuseState(), s.captchaStore, s.trustedProxies())
	}
	return middleware.ChainHTTP(next,
		middleware.RecoverHTTP(),
		middleware.RequestLogHTTP("/health/live", "/health/ready"),
		middleware.SecurityHeadersHTTP(),
		// CORS is browser transport policy, not API authorization; real request
		// security is always JWT signature/issuer/audience/permissions plus
		// merchant ownership, which a browser preflight can't even carry (no
		// JWT on OPTIONS). #765: since every accepted request is a bearer JWT
		// (never an ambient cookie), an origin allow-list protects nothing —
		// a stolen token is replayed from curl, where CORS doesn't exist — so
		// the policy is a static, non-configurable `*` grant on exactly the
		// browser-tier routes (checkout + self-service,
		// tracked in browserTierRoutes as they register) and NO CORS headers
		// anywhere else (admin/platform/merchant-API/webhooks/auth), so a
		// browser refuses cross-origin script access to those by default.
		middleware.PermissiveCORSHTTP(browser),
		middleware.IssuerOriginCORSHTTP(func(r *http.Request) bool { return s.merchantTierRoutes.Match(r) }, s.allowedIssuerOrigin),
		middleware.RequestLimitsHTTP(middleware.DefaultMaxBodyBytes),
		s.billingCredentialsHTTP,
		// Resolve the merchant / billing namespace before authorization and before any
		// merchant-owned DB access (issue #223). Resolved PER REQUEST off the Runtime
		// (#744), never a value snapshotted here at construction time; zero when none
		// is configured, in which case merchant-owned operations hard-fail (there is
		// no default merchant).
		middleware.ResolveMerchantHTTP(s.runtime.ConfiguredMerchant),
		// #734: Host-based multi-merchant resolution, sharing the SAME control-plane
		// directory lookup as CORS above and as the JWT-issuer resolution
		// (internal/controlplane.merchantForIssuer checks the marker this pins).
		// Runs after the static ResolveMerchantHTTP so a live per-request Host match
		// is authoritative over any process-wide configured merchant. A Host with no
		// api_host configured for any merchant is a no-op — behavior is unchanged.
		middleware.ResolveMerchantFromHostHTTP(s.hostMerchantResolver),
		// Best-effort auth so the rate limiter can key by user, not only IP.
		middleware.HTTPMiddleware(billingauth.Optional(s.authenticator)),
		limiter,
	)
}

// staffAuth is the standalone server's Authenticator for the admin API and
// the programmatic routes: its API keys, trusted issuers' access tokens and
// control-plane users' sessions (AuthKit's Authenticator).
func (s *Server) staffAuth() *httproutes.StandaloneAuth {
	auth := &httproutes.StandaloneAuth{Issuer: s.issuer}
	if s.controlPlane != nil {
		auth.ResourceTokenResolver, auth.ServiceCredentialResolver, auth.Directory = s.controlPlane, s.controlPlane, s.controlPlane
		if core := s.controlPlane.Core(); core != nil {
			auth.Sessions = core.Authenticator()
		}
	}
	return auth
}

// customerAuth is the standalone server's Authenticator for /v1/me: trusted
// issuers' openrails:self access tokens.
func (s *Server) customerAuth() httproutes.StandaloneCustomers {
	if s.customerResolver != nil {
		return httproutes.StandaloneCustomers{Resolver: s.customerResolver}
	}
	if s.controlPlane == nil {
		return httproutes.StandaloneCustomers{}
	}
	return httproutes.StandaloneCustomers{Resolver: s.controlPlane}
}

// hostMerchantResolver is the standalone #734 Host->merchant resolver: the
// control plane's; nil-safe for hand-built *Server{} unit tests that skip
// New(). Unrelated to CORS since #765 (CORS is a static per-route
// policy, not sourced from Host/merchant resolution).
func (s *Server) hostMerchantResolver(ctx context.Context, host string) (billing.MerchantID, error) {
	if s == nil || s.controlPlane == nil {
		return billing.MerchantID{}, nil
	}
	return s.controlPlane.ResolveMerchantByHost(ctx, host)
}

// Handler returns the full public HTTP surface: health + user + self/customer
// + merchant + webhooks + API-key-authenticated server-to-server service routes
// (issue #222). There is no separate private/service handler — embedded hosts use
// the in-process internal/service facade (Embedded.Service()) or this same public
// surface. It is designed to be mounted at a path prefix via http.StripPrefix.
func (s *Server) Handler() http.Handler { return s.publicHandler }

// AuthKit owns its refresh/CSRF cookie protocol. Apply the billing credential
// policy only to the billing surface, never to the mounted AuthKit transport.
func (s *Server) billingCredentialsHTTP(next http.Handler) http.Handler {
	billing := billingauth.ExplicitCredentials(next)
	prefix := s.controlPlane.AuthPrefix()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == prefix || strings.HasPrefix(r.URL.Path, prefix+"/") {
			next.ServeHTTP(w, r)
			return
		}
		billing.ServeHTTP(w, r)
	})
}

// HTTPRoutes returns the native hosted surface without process health routes.
func (s *Server) HTTPRoutes() *router.Table {
	return &router.Table{Entries: append([]router.Entry(nil), s.nativeRoutes.Entries...)}
}

// Drain fails /health/ready from now on, so load balancers stop routing here
// while the process finishes what it serves.
func (s *Server) Drain() { s.draining.Store(true) }
