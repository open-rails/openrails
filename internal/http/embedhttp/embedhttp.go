// Package embedhttp assembles the embedded billing HTTP surface as a gin-free
// net/http handler (issue #282/#285). It is the keystone that lets pkg/embedded
// expose NewHTTPHandler without importing gin: route groups are registered via
// the neutral router.NewMux (request.NewHTTP backend) and the captcha discovery
// routes are plain net/http handlers, all wrapped with the net/http base
// middleware stack (security headers, CORS, body limit, merchant resolution).
//
// The gin Server (internal/http) and standalone cmd/ keep gin; this package is
// the gin-free analogue of the embedded assembly that used to live there.
package embedhttp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	redis "github.com/redis/go-redis/v9"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	authpolicy "github.com/open-rails/openrails/internal/auth/policy"
	"github.com/open-rails/openrails/internal/captcha"
	captchaembed "github.com/open-rails/openrails/internal/captcha/embed"
	httphandlers "github.com/open-rails/openrails/internal/http/handlers"
	"github.com/open-rails/openrails/internal/http/middleware"
	"github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/internal/http/routesurface"
	"github.com/open-rails/openrails/internal/shared/iputil"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
)

// EmbeddedV1Prefix is the canonical API prefix for embedded mode handlers.
//
// Embedded hosts typically mount billing under "/billing", so the stable
// contract becomes "/billing/v1/*".
const EmbeddedV1Prefix = "/billing/v1"

// Options controls which billing HTTP route groups are included in the returned
// handler. A zero RouteSets slice uses EmbeddedDefaultRouteSets.
type Options struct {
	RouteSets []RouteSet
	// AdvertiseRouteSets is the full selection reported by GET /v1/capabilities,
	// independent of which subset THIS handler actually mounts. The embedded
	// combined mount strips `customer` from RouteSets (it is served by the
	// separate self handler) but still advertises it here so discovery is honest.
	// Empty → falls back to RouteSets.
	AdvertiseRouteSets []RouteSet
	ProviderRoutes     *routesurface.ProviderRoutes
}

// Assembler builds the gin-free embedded billing surface from the gin-free
// application graph. Every field is gin-free, so importing this package pulls no
// gin onto the embedded request path.
type Assembler struct {
	Cfg     *config.Config
	Runtime *app.Runtime
	// AdminChecker is the live admin permission checker (#312), held as the neutral
	// authpolicy.AdminPermissionChecker interface so this core package imports
	// neither internal/controlplane nor AuthKit (#284). nil for embedded hosts
	// without a control plane (admin routes then fail closed). The concrete
	// *controlplane.ControlPlane satisfies it.
	AdminChecker              authpolicy.AdminPermissionChecker
	ServiceCredentialResolver httproutes.ServiceCredentialResolver
	// APIKeys is the #757 merchant self-serve API-key manager (the attached
	// control plane). nil for hosts without a control plane — the /api-keys
	// routes then answer 501.
	APIKeys      httphandlers.MerchantAPIKeyManager
	CaptchaStore *captcha.ChallengeStore
	// RDB is the Redis/Garnet client backing the rate-limit counters + captcha
	// challenge store. nil falls back to per-process in-memory rate-limit windows.
	RDB           *redis.Client
	Authenticator billingauth.Authenticator
	Gate          billingauth.Gate
	// DelegatedAuthenticator is the in-process host identity seam (#565). When set
	// (embedded hosts), the merchant routes accept the host's trusted principal and
	// gate on its permissions — the gin-free counterpart of the self surface's
	// DelegatedPrincipalRequired. nil for standalone (control-plane resolvers).
	DelegatedAuthenticator billingauth.DelegatedAuthenticator
	// HostResolve is the #734 Host->merchant mechanism, derived from the
	// attached control plane (HostMerchantResolverFrom) exactly like
	// AdminChecker/ServiceCredentialResolver above — nil for embedded hosts
	// without a control plane, in which case Host-based merchant resolution
	// behaves exactly as before this issue (no behavior change without opting
	// in). Unrelated to CORS since #765: browser CORS is now a static
	// per-route-tier policy (PermissiveCORSHTTP), not sourced from the control
	// plane or any Host lookup.
	HostResolve merchant.HostResolver
}

// hostMerchantResolver is the #734 neutral capability a *controlplane.ControlPlane
// satisfies, asserted off App.ControlPlane (held as `any`) so this package stays
// free of AuthKit (#284) — matching the AdminChecker/ServiceCredentialResolver
// pattern above.
type hostMerchantResolver interface {
	ResolveMerchantByHost(ctx context.Context, host string) (merchant.ID, error)
}

// HostMerchantResolverFrom derives the #734 Host->merchant resolver from an
// app's ControlPlane field (an `any` on app.App). Returns nil when no control
// plane is attached, or it doesn't implement the capability — callers treat
// that identically to never having called this at all (single-merchant
// self-hosters see no behavior change without opting in). Exported so
// pkg/embedded/mount.go (a sibling package, composing the self-service
// handler) can derive the SAME resolver this package's own FromApp uses,
// without duplicating the type assertion.
func HostMerchantResolverFrom(controlPlane any) merchant.HostResolver {
	c, ok := controlPlane.(hostMerchantResolver)
	if !ok || c == nil {
		return nil
	}
	return c.ResolveMerchantByHost
}

// FromApp builds an Assembler from the gin-free application graph (the same
// inputs the gin Server derives its embedded surface from). The control plane,
// when present, is read off app.App.ControlPlane (held as `any`) via an interface
// type assertion to the neutral AdminPermissionChecker — no controlplane import
// on the embedded request path (#284).
func FromApp(a *app.App) *Assembler {
	if a == nil {
		return nil
	}
	var checker authpolicy.AdminPermissionChecker
	if c, ok := a.ControlPlane.(authpolicy.AdminPermissionChecker); ok {
		checker = c
	}
	var resolver httproutes.ServiceCredentialResolver
	if c, ok := a.ControlPlane.(httproutes.ServiceCredentialResolver); ok {
		resolver = c
	}
	var apiKeys httphandlers.MerchantAPIKeyManager
	if c, ok := a.ControlPlane.(httphandlers.MerchantAPIKeyManager); ok {
		apiKeys = c
	}
	hostResolve := HostMerchantResolverFrom(a.ControlPlane)
	asm := &Assembler{
		Cfg:                       a.Config,
		Runtime:                   a.Runtime,
		AdminChecker:              checker,
		ServiceCredentialResolver: resolver,
		APIKeys:                   apiKeys,
		CaptchaStore:              captcha.NewChallengeStore(a.RedisClient),
		RDB:                       a.RedisClient,
		HostResolve:               hostResolve,
	}
	if checker != nil || resolver != nil {
		asm.Gate = httproutes.NewGate(httproutes.GateOptions{
			AdminPermissionChecker:    checker,
			ServiceCredentialResolver: resolver,
		})
	}
	return asm
}

// NewHTTPHandler assembles the embedded billing surface as a gin-free
// *http.ServeMux (issue #282). Route groups are registered via router.NewMux
// (request.NewHTTP backend), and the captcha routes are plain net/http handlers.
// The mux is wrapped with the net/http base middleware stack (security headers,
// CORS, body limit, merchant resolution) — the gin-free analogue of the global
// engine middleware. The returned handler imports zero gin on the request path.
//
// Rate-limiting + the captcha challenge flow ARE enforced here BY DEFAULT
// (#742): embedded.New seeds config.Config.RateLimits/Captcha with the same
// curated defaults config.Load applies whenever the host leaves them nil, so
// an embedded host does not need to front billing with its own gateway unless
// it explicitly opts out (config.Config.RateLimitsDisabled) — RateLimitHTTP
// runs as a passthrough only then. billingauth.Optional runs ahead of the
// limiter so an authenticated caller is keyed per-user, not only per-IP
// (mirroring the standalone engine's authProvider.Optional() → RateLimit
// order).
func (s *Assembler) NewHTTPHandler(opts Options) http.Handler {
	routeSets := routeSetMap(opts.RouteSets)
	if err := s.validateAuthBoundary(routeSets); err != nil {
		panic(err)
	}
	providerRoutes := s.providerRoutes(opts.ProviderRoutes)
	if !providerRoutes.Webhooks {
		delete(routeSets, RouteSetWebhooks)
	}
	mux := http.NewServeMux()

	// Capability discovery (#623): always-on, public, independent of selection so
	// even a minimal deployment is discoverable. Reports the full advertised set
	// (incl. `customer`, which the combined mount serves via the self handler).
	advertise := opts.AdvertiseRouteSets
	if len(advertise) == 0 {
		advertise = ResolveRouteSets(opts.RouteSets)
	}
	if !providerRoutes.Webhooks {
		advertise = withoutRouteSet(advertise, RouteSetWebhooks)
	}
	mux.Handle(http.MethodGet+" "+EmbeddedV1Prefix+"/capabilities", CapabilitiesHandler(advertise, providerRoutes))

	// browserTier tracks the checkout patterns mounted below (#765): the ONLY
	// route set on this combined handler that belongs to the permissive-CORS
	// browser tier. Empty (matches nothing) when RouteSetCheckout isn't
	// selected, so PermissiveCORSHTTP is a pure no-op for a merchant-admin/
	// catalog/payment-providers/merchant-API/webhooks-only mount.
	browserTier := middleware.NewBrowserTierRoutes()
	if routeSets[RouteSetCheckout] {
		// Captcha discovery routes (net/http), mirroring registerUserRoutesAt.
		mux.HandleFunc(http.MethodGet+" "+EmbeddedV1Prefix+"/captcha/status", s.captchaStatusHandler)
		browserTier.Add(http.MethodGet + " " + EmbeddedV1Prefix + "/captcha/status")
		mux.HandleFunc(http.MethodGet+" "+EmbeddedV1Prefix+"/captcha/client.js", s.captchaClientScriptHandler)
		browserTier.Add(http.MethodGet + " " + EmbeddedV1Prefix + "/captcha/client.js")
		httproutes.RegisterUserRoutes(router.NewMuxRecorded(mux, EmbeddedV1Prefix, s.Runtime, browserTier.Add), s.Runtime, httproutes.Options{
			Authenticator:  s.Authenticator,
			ProviderRoutes: &providerRoutes,
		})
	}
	if routeSets[RouteSetMerchantAdmin] {
		adminOpts := httproutes.Options{
			Gate:    s.Gate,
			APIKeys: s.APIKeys,
		}
		// #528: per-user `/admin` retired; the delegated admin surface is mounted
		// via embgin.SelfHandler (issuer→owner), not the base handler.
		httproutes.RegisterMerchantActionRoutes(router.NewMux(mux, EmbeddedV1Prefix+"/merchant", s.Runtime), s.Runtime, adminOpts)
		// #737: DeclaredBilling import — same route the standalone surface serves.
		httproutes.RegisterImportRoutes(router.NewMux(mux, EmbeddedV1Prefix+"/import", s.Runtime), s.Runtime, adminOpts)
	}
	if routeSets[RouteSetCatalog] {
		adminOpts := httproutes.Options{
			Gate: s.Gate,
		}
		httproutes.RegisterCatalogRoutes(router.NewMux(mux, EmbeddedV1Prefix+"/merchant/catalog", s.Runtime), s.Runtime, adminOpts)
	}
	if routeSets[RouteSetPaymentProviders] {
		adminOpts := httproutes.Options{
			Gate: s.Gate,
		}
		httproutes.RegisterPaymentProviderRoutes(router.NewMux(mux, EmbeddedV1Prefix+"/merchant/payment-providers", s.Runtime), s.Runtime, adminOpts)
	}
	if routeSets[RouteSetMerchantAPI] {
		httproutes.RegisterServiceRoutes(router.NewMux(mux, EmbeddedV1Prefix+"/merchant", s.Runtime), s.Runtime, httproutes.Options{Gate: s.Gate})
	}
	if routeSets[RouteSetWebhooks] {
		httproutes.RegisterMerchantWebhookRoutes(router.NewMux(mux, EmbeddedV1Prefix, s.Runtime), s.Runtime)
		// #734: the Host-routed webhook mount (saas #15's "api.<slug>.<domain>"
		// scheme) is additive — it mounts at the canonical no-merchant-segment
		// path ("/billing/v1/webhooks/:provider"), distinct from the merchant-scoped
		// path above, and only when a control plane is attached (HostResolve != nil).
		// A host with no control plane, or one that never configures any
		// merchant's api_host, is unaffected: the path simply isn't mounted / never
		// resolves.
		if s.HostResolve != nil {
			httproutes.RegisterHostWebhookRoutes(router.NewMux(mux, EmbeddedV1Prefix, s.Runtime), s.Runtime, s.HostResolve)
		}
	}

	// Merchant resolution (#223/#336) pins Runtime.ConfiguredMerchant() onto
	// the request context before any merchant-owned DB access. It is resolved
	// PER REQUEST, not cached here at handler-construction time (#744): an
	// embedded host's UpsertMerchantConfig call can bind the merchant AFTER
	// this handler is already mounted and serving traffic, and
	// Runtime.ConfiguredMerchant() always reflects the latest bind. Zero
	// (unbound) is a legal boot state — merchant-owned operations then hard-fail
	// downstream instead of silently defaulting.
	var rateLimits *config.RateLimitsConfig
	var captchaCfg *config.CaptchaConfig
	var resolver *iputil.TrustedProxies
	if s.Cfg != nil {
		rateLimits = s.Cfg.RateLimits
		captchaCfg = s.Cfg.Captcha
	}
	if s.Runtime != nil {
		resolver = s.Runtime.TrustedProxies
	}
	return middleware.ChainHTTP(mux,
		middleware.SecurityHeadersHTTP(),
		// #765: static permissive CORS on exactly the checkout patterns
		// registered into browserTier above — `*` from any origin, no
		// credentials, nothing on merchant-admin/catalog/payment-providers/
		// merchant-API/webhooks.
		middleware.PermissiveCORSHTTP(browserTier.Match),
		middleware.BodyLimitHTTP(middleware.DefaultMaxBodyBytes),
		middleware.ResolveMerchantHTTP(s.Runtime.ConfiguredMerchant),
		// #734: Host-based multi-merchant resolution (a no-op when HostResolve is
		// nil), unrelated to CORS since #765.
		middleware.ResolveMerchantFromHostHTTP(s.HostResolve),
		// Best-effort auth so the rate limiter can key by user, not only IP.
		middleware.HTTPMiddleware(billingauth.Optional(s.Authenticator)),
		// OpenRails-native rate-limiting + captcha for embedded hosts.
		middleware.RateLimitHTTP(rateLimits, captchaCfg, s.RDB, s.CaptchaStore, resolver),
	)
}

func (s *Assembler) providerRoutes(override *routesurface.ProviderRoutes) routesurface.ProviderRoutes {
	if s == nil {
		return ProviderRoutesForRuntime(nil, override)
	}
	return ProviderRoutesForRuntime(s.Runtime, override)
}

func withoutRouteSet(routeSets []RouteSet, remove RouteSet) []RouteSet {
	if len(routeSets) == 0 {
		return routeSets
	}
	out := make([]RouteSet, 0, len(routeSets))
	for _, routeSet := range routeSets {
		if routeSet != remove {
			out = append(out, routeSet)
		}
	}
	return out
}

func (s *Assembler) validateAuthBoundary(routeSets map[RouteSet]bool) error {
	if (routeSets[RouteSetCheckout] || routeSets[RouteSetCustomer]) && (s == nil || s.Authenticator == nil) {
		return fmt.Errorf("embedded billing: user route groups require Options.Authenticator")
	}
	if (routeSets[RouteSetMerchantAdmin] || routeSets[RouteSetCatalog] || routeSets[RouteSetPaymentProviders] || routeSets[RouteSetMerchantAPI]) && (s == nil || s.Gate == nil) {
		return fmt.Errorf("embedded billing: merchant route groups require Options.Gate")
	}
	return nil
}

// captchaStatusHandler is the gin-free captcha status endpoint (issue #282).
func (s *Assembler) captchaStatusHandler(w http.ResponseWriter, r *http.Request) {
	var cfg *config.CaptchaConfig
	if s != nil && s.Cfg != nil {
		cfg = s.Cfg.Captcha
	}
	var store *captcha.ChallengeStore
	var resolver *iputil.TrustedProxies
	if s != nil {
		store = s.CaptchaStore
		if s.Runtime != nil {
			resolver = s.Runtime.TrustedProxies
		}
	}
	CaptchaStatusHandler(cfg, store, resolver)(w, r)
}

// captchaClientScriptHandler is the gin-free captcha client-script endpoint.
func (s *Assembler) captchaClientScriptHandler(w http.ResponseWriter, r *http.Request) {
	var cfg *config.CaptchaConfig
	if s != nil && s.Cfg != nil {
		cfg = s.Cfg.Captcha
	}
	CaptchaClientScriptHandler(cfg)(w, r)
}

// CaptchaStatusHandler is the captcha discovery endpoint shared by the embedded
// and standalone surfaces (#670: one implementation, two mounts). resolver
// must be the SAME trusted-proxy resolver RateLimitHTTP enforces with (#746),
// or the subject key checked here can diverge from the one actually
// challenged.
func CaptchaStatusHandler(cfg *config.CaptchaConfig, store *captcha.ChallengeStore, resolver *iputil.TrustedProxies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"enabled":           cfg.IsEnabled(),
			"required":          false,
			"token_header":      captcha.TokenHeader,
			"client_script_url": captchaClientScriptURL(r),
		}
		if cfg != nil {
			resp["provider"] = cfg.EffectiveProvider()
		}
		if cfg.IsEnabled() && store != nil {
			for _, subjectKey := range middleware.RateLimitSubjectKeysHTTP(r, resolver) {
				challenged, err := store.IsChallenged(r.Context(), subjectKey)
				if err != nil {
					continue
				}
				if challenged {
					resp["required"] = true
					break
				}
			}
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// CaptchaClientScriptHandler serves the captcha client bootstrap script.
func CaptchaClientScriptHandler(cfg *config.CaptchaConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		script := buildCaptchaClientScript(cfg)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(script))
	}
}

func captchaClientScriptURL(r *http.Request) string {
	if r == nil || r.URL == nil {
		return "/v1/captcha/client.js"
	}
	path := r.URL.Path
	if strings.HasSuffix(path, "/status") {
		return strings.TrimSuffix(path, "/status") + "/client.js"
	}
	return "/v1/captcha/client.js"
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func buildCaptchaClientScript(cfg *config.CaptchaConfig) string {
	enabled := cfg.IsEnabled()
	provider := ""
	siteKey := ""
	scriptURL := ""
	action := ""
	if cfg != nil {
		provider = cfg.EffectiveProvider()
		action = cfg.EffectiveAction()
	}
	if enabled {
		siteKey = strings.TrimSpace(cfg.SiteKey)
		scriptURL = cfg.EffectiveScriptURL()
	}

	return strings.NewReplacer(
		"__OPENRAILS_CAPTCHA_ENABLED__", strconv.FormatBool(enabled),
		"__OPENRAILS_CAPTCHA_PROVIDER__", jsonLiteral(provider),
		"__OPENRAILS_CAPTCHA_SITE_KEY__", jsonLiteral(siteKey),
		"__OPENRAILS_CAPTCHA_SCRIPT_URL__", jsonLiteral(scriptURL),
		"__OPENRAILS_CAPTCHA_ACTION__", jsonLiteral(action),
	).Replace(captchaembed.ClientScriptTemplate)
}

func jsonLiteral(value string) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return "\"\""
	}
	return string(raw)
}
