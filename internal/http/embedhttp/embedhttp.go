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
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/abusestate"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/captcha"
	captchaembed "github.com/open-rails/openrails/internal/captcha/embed"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/http/middleware"
	"github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/internal/http/routesurface"
	"github.com/open-rails/openrails/internal/shared/iputil"
)

// EmbeddedV1Prefix is the canonical API prefix for embedded mode handlers.
//
// Embedded hosts typically mount billing under "/billing", so the stable
// contract becomes "/billing/v1/*".
const EmbeddedV1Prefix = embeddedMount + "/v1"

// embeddedMount is where an embedded host mounts the API root.
const embeddedMount = "/billing"

// Options is what the handler mounts beside the public and webhook routes:
// the staff route groups Permissions gives a permission.
type Options struct {
	Permissions    httproutes.Permissions
	ProviderRoutes *routesurface.ProviderRoutes
	// Capabilities is what GET /v1/config reports; nil derives it.
	Capabilities *Capabilities
	// Programmatic mounts the programmatic routes (/v1/app), SCIM included
	// unless the runtime reads the host's directory.
	Programmatic bool
}

// Assembler builds the embedded billing surface from the application graph.
type Assembler struct {
	Cfg          *config.Config
	Runtime      *app.Runtime
	CaptchaStore *captcha.ChallengeStore
	AdminLimiter *middleware.AdminOperationLimiter
	// Auth is the mount's Routes.Auth: who a staff, programmatic or
	// checkout-session request is. Scope is Routes.Scope.
	Auth  billingauth.Authenticator
	Scope billingauth.Scope
}

// FromApp builds an Assembler from the application graph.
func FromApp(a *app.App) *Assembler {
	if a == nil {
		return nil
	}
	return &Assembler{
		Cfg:          a.Config,
		Runtime:      a.Runtime,
		CaptchaStore: a.Runtime.CaptchaStore,
		AdminLimiter: middleware.NewAdminOperationLimiter(a.Runtime.AbuseState),
	}
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
// it explicitly opts out (config.Config.RateLimitsDisabled). It keys by
// client IP: no route is authenticated before its own gate runs.
func (s *Assembler) NewHTTPHandler(opts Options) http.Handler {
	return s.NewRoutes(opts).Handler()
}

// NewRoutes records actual registrations, retaining each route's security chain.
func (s *Assembler) NewRoutes(opts Options) *router.Table {
	if err := s.validateAuthBoundary(opts); err != nil {
		panic(err)
	}
	providerRoutes := s.providerRoutes(opts.ProviderRoutes)
	mux := &router.Table{}

	// The public configuration (GET /v1/config) is always on, so even a
	// minimal deployment is discoverable.
	capabilities := CapabilitiesFor(s.Runtime, opts.Permissions, opts.Programmatic, providerRoutes, nil)
	if opts.Capabilities != nil {
		capabilities = *opts.Capabilities
	}
	// browserTier tracks the configuration and checkout patterns mounted
	// below (#765): the ONLY routes on this combined handler that belong to
	// the permissive-CORS browser tier.
	browserRoutes := make(map[string]bool)
	recordBrowser := func(pattern string) { browserRoutes[pattern] = true }
	httproutes.RegisterMetaRoutes(router.NewMuxRecorded(mux, embeddedMount, s.Runtime, recordBrowser), httproutes.Options{Capabilities: &capabilities})

	httproutes.RegisterUserRoutes(router.NewMuxRecorded(mux, EmbeddedV1Prefix, s.Runtime, recordBrowser), s.Runtime, httproutes.Options{
		Auth:           s.Auth,
		ProviderRoutes: &providerRoutes,
		External: httproutes.External{
			CaptchaStatus: http.HandlerFunc(s.captchaStatusHandler),
			CaptchaScript: http.HandlerFunc(s.captchaClientScriptHandler),
		},
	})
	if opts.Permissions != (httproutes.Permissions{}) {
		httproutes.RegisterStaffRoutes(router.NewMux(mux, EmbeddedV1Prefix, s.Runtime), s.Runtime, httproutes.Options{
			Auth:         s.Auth,
			Scope:        httproutes.FixedScope(s.Scope),
			AdminLimiter: s.AdminLimiter,
			Permissions:  opts.Permissions,
			Capabilities: &capabilities,
		})
	}
	if providerRoutes.Webhooks {
		httproutes.RegisterWebhookRoutes(router.NewMux(mux, EmbeddedV1Prefix+"/webhooks", s.Runtime), s.Runtime)
	}
	if opts.Programmatic {
		httproutes.RegisterAppRoutes(router.NewMux(mux, EmbeddedV1Prefix, s.Runtime), s.Runtime, httproutes.Options{
			Auth:         s.Auth,
			Capabilities: &capabilities,
		})
	}

	// Resolve the configured merchant on each request before merchant-owned
	// database access. An unbound runtime requires explicit merchant authority;
	// it never silently selects a default merchant.
	var rateLimits *config.RateLimitsConfig
	var captchaCfg *config.CaptchaConfig
	var resolver *iputil.TrustedProxies
	var state *abusestate.Store
	if s.Cfg != nil {
		rateLimits = s.Cfg.RateLimits
		captchaCfg = s.Cfg.Captcha
	}
	if s.Runtime != nil {
		resolver = s.Runtime.TrustedProxies
		state = s.Runtime.AbuseState
	}
	for i := range mux.Entries {
		entry := &mux.Entries[i]
		entry.Browser = browserRoutes[entry.Method+" "+entry.Path]
	}
	limiter := middleware.RateLimitHTTP(rateLimits, captchaCfg, state, s.CaptchaStore, resolver)
	mux.Wrap(func(entry router.Entry) http.Handler {
		return middleware.ChainHTTP(entry.Handler,
			middleware.WithRoutePath(entry.Path),
			middleware.SecurityHeadersHTTP(),
			// #765: static permissive CORS on exactly the checkout patterns
			// registered into browserTier above — `*` from any origin, no
			// credentials, nothing on merchant-admin/catalog/psps/
			// merchant-API/webhooks.
			middleware.PermissiveCORSHTTP(func(*http.Request) bool { return entry.Browser }),
			middleware.RequestLimitsHTTP(middleware.DefaultMaxBodyBytes),
			middleware.HTTPMiddleware(billingauth.ExplicitCredentials),
			middleware.ResolveMerchantHTTP(s.Runtime.ConfiguredMerchant),
			// OpenRails-native rate-limiting + captcha for embedded hosts.
			limiter,
		)
	})
	return mux
}

func (s *Assembler) providerRoutes(override *routesurface.ProviderRoutes) routesurface.ProviderRoutes {
	if s == nil {
		return ProviderRoutesForRuntime(nil, override)
	}
	return ProviderRoutesForRuntime(s.Runtime, override)
}

func (s *Assembler) validateAuthBoundary(opts Options) error {
	if (opts.Permissions != (httproutes.Permissions{}) || opts.Programmatic) && (s == nil || httproutes.IsNilAuth(s.Auth)) {
		return httproutes.MountError{Route: "admin API", Reason: "needs Routes.Auth"}
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
		resp := billing.CaptchaStatus{Enabled: config.CaptchaEnabled(cfg), TokenHeader: captcha.TokenHeader, ClientScriptURL: captchaClientScriptURL(r)}
		if cfg != nil {
			provider := config.CaptchaProvider(cfg)
			resp.Provider = &provider
		}
		if config.CaptchaEnabled(cfg) && store != nil {
			for _, subjectKey := range middleware.RateLimitSubjectKeysHTTP(r, resolver) {
				if store.IsChallenged(r.Context(), subjectKey) {
					resp.Required = true
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
	enabled := config.CaptchaEnabled(cfg)
	provider := ""
	siteKey := ""
	scriptURL := ""
	action := ""
	if cfg != nil {
		provider = config.CaptchaProvider(cfg)
		action = config.CaptchaAction
	}
	if enabled {
		siteKey = strings.TrimSpace(cfg.SiteKey)
		scriptURL = config.CaptchaScriptURL(cfg)
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
