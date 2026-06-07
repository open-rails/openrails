// Package embedhttp assembles the embedded billing HTTP surface as a gin-free
// net/http handler (issue #282/#285). It is the keystone that lets pkg/embedded
// expose NewHTTPHandler without importing gin: route groups are registered via
// the neutral router.NewMux (request.NewHTTP backend) and the captcha discovery
// routes are plain net/http handlers, all wrapped with the net/http base
// middleware stack (security headers, CORS, body limit, tenant resolution).
//
// The gin Server (internal/http) and standalone cmd/ keep gin; this package is
// the gin-free analogue of the embedded assembly that used to live there.
package embedhttp

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	authpolicy "github.com/open-rails/openrails/internal/auth/policy"
	"github.com/open-rails/openrails/internal/captcha"
	captchaembed "github.com/open-rails/openrails/internal/captcha/embed"
	"github.com/open-rails/openrails/internal/http/middleware"
	"github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/pkg/billingauth"
)

// EmbeddedV1Prefix is the canonical API prefix for embedded mode handlers.
//
// Embedded hosts typically mount billing under "/billing", so the stable
// contract becomes "/billing/v1/*".
const EmbeddedV1Prefix = "/billing/v1"

// Options controls which billing HTTP route groups are included in the returned
// handler. If all fields are false (zero value), it defaults to user + admin +
// webhooks.
type Options struct {
	IncludeUser     bool
	IncludeAdmin    bool
	IncludeWebhooks bool
}

func (o Options) isZero() bool {
	return !o.IncludeUser && !o.IncludeAdmin && !o.IncludeWebhooks
}

func (o Options) withDefaults() Options {
	if !o.isZero() {
		return o
	}
	return Options{IncludeUser: true, IncludeAdmin: true, IncludeWebhooks: true}
}

// Assembler builds the gin-free embedded billing surface from the gin-free
// application graph. Every field is gin-free, so importing this package pulls no
// gin onto the embedded request path.
type Assembler struct {
	Cfg     *config.Config
	Runtime *app.Runtime
	// OperatorChecker is the live operator-tenant permission checker (#224), held as
	// the neutral authpolicy.OperatorPermissionChecker interface so this core
	// package imports neither internal/controlplane nor AuthKit (#284). nil in
	// verifier-only mode. The concrete *controlplane.ControlPlane satisfies it.
	OperatorChecker authpolicy.OperatorPermissionChecker
	CaptchaStore    *captcha.ChallengeStore
	Authenticator   billingauth.Authenticator
}

// FromApp builds an Assembler from the gin-free application graph (the same
// inputs the gin Server derives its embedded surface from). The control plane,
// when present, is read off app.App.ControlPlane (held as `any`) via an interface
// type assertion to the neutral OperatorPermissionChecker — no controlplane
// import on the embedded request path (#284).
func FromApp(a *app.App) *Assembler {
	if a == nil {
		return nil
	}
	var checker authpolicy.OperatorPermissionChecker
	if c, ok := a.ControlPlane.(authpolicy.OperatorPermissionChecker); ok {
		checker = c
	}
	return &Assembler{
		Cfg:             a.Config,
		Runtime:         a.Runtime,
		OperatorChecker: checker,
		CaptchaStore:    captcha.NewChallengeStore(a.RedisClient),
		Authenticator:   a.Authenticator,
	}
}

// NewHTTPHandler assembles the embedded billing surface as a gin-free
// *http.ServeMux (issue #282). Route groups are registered via router.NewMux
// (request.NewHTTP backend), and the captcha routes are plain net/http handlers.
// The mux is wrapped with the net/http base middleware stack (security headers,
// CORS, body limit, tenant resolution) — the gin-free analogue of the global
// engine middleware. The returned handler imports zero gin on the request path.
//
// NOTE: rate-limiting + the captcha challenge flow are NOT applied here (they are
// gin/captcha-flow-coupled; the standalone gin surface keeps them, and embedded
// hosts front billing with their own gateway). See middleware/http_base.go.
func (s *Assembler) NewHTTPHandler(opts Options) http.Handler {
	opts = opts.withDefaults()
	mux := http.NewServeMux()

	if opts.IncludeUser {
		// Captcha discovery routes (net/http), mirroring registerUserRoutesAt.
		mux.HandleFunc(http.MethodGet+" "+EmbeddedV1Prefix+"/captcha/status", s.captchaStatusHandler)
		mux.HandleFunc(http.MethodGet+" "+EmbeddedV1Prefix+"/captcha/client.js", s.captchaClientScriptHandler)
		httproutes.RegisterUserRoutes(router.NewMux(mux, EmbeddedV1Prefix, s.Runtime), s.Runtime, httproutes.Options{
			Authenticator: s.Authenticator,
		})
	}
	if opts.IncludeAdmin {
		adminOpts := httproutes.Options{
			Authenticator: s.Authenticator,
		}
		if s.OperatorChecker != nil {
			adminOpts.OperatorPermissionChecker = s.OperatorChecker
		}
		httproutes.RegisterAdminRoutes(router.NewMux(mux, EmbeddedV1Prefix+"/admin", s.Runtime), s.Runtime, adminOpts)
	}
	if opts.IncludeWebhooks {
		httproutes.RegisterWebhookRoutes(router.NewMux(mux, EmbeddedV1Prefix+"/webhooks", s.Runtime), s.Runtime)
	}

	var origins []string
	if s.Cfg != nil {
		origins = s.Cfg.AllowedCORSOrigins()
	}
	return middleware.ChainHTTP(mux,
		middleware.SecurityHeadersHTTP(),
		middleware.CORSHTTP(origins),
		middleware.BodyLimitHTTP(middleware.DefaultMaxBodyBytes),
		middleware.ResolveTenantHTTP(),
	)
}

// captchaStatusHandler is the gin-free captcha status endpoint (issue #282).
func (s *Assembler) captchaStatusHandler(w http.ResponseWriter, r *http.Request) {
	var cfg *config.CaptchaConfig
	if s != nil && s.Cfg != nil {
		cfg = s.Cfg.Captcha
	}
	resp := map[string]any{
		"enabled":           cfg != nil && cfg.Enabled,
		"required":          false,
		"token_header":      captcha.TokenHeader,
		"client_script_url": captchaClientScriptURL(r),
	}
	if cfg != nil {
		resp["provider"] = cfg.EffectiveProvider()
	}
	if cfg != nil && cfg.Enabled && s.CaptchaStore != nil {
		for _, subjectKey := range middleware.RateLimitSubjectKeysHTTP(r) {
			challenged, err := s.CaptchaStore.IsChallenged(r.Context(), subjectKey)
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

// captchaClientScriptHandler is the gin-free captcha client-script endpoint.
func (s *Assembler) captchaClientScriptHandler(w http.ResponseWriter, r *http.Request) {
	var cfg *config.CaptchaConfig
	if s != nil && s.Cfg != nil {
		cfg = s.Cfg.Captcha
	}
	script := buildCaptchaClientScript(cfg)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(script))
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
	enabled := cfg != nil && cfg.Enabled
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
