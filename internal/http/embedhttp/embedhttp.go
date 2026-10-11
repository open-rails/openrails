// Package embedhttp assembles the embedded billing HTTP surface as a gin-free
// net/http handler (issue #282/#285). It is the keystone that lets pkg/embedded
// expose NewHTTPHandler without importing gin: route groups are registered via
// the neutral router.NewMux (request.NewHTTP backend), all wrapped with the
// net/http base middleware stack (security headers, CORS, body limit).
//
// The gin Server (internal/http) and standalone cmd/ keep gin; this package is
// the gin-free analogue of the embedded assembly that used to live there.
package embedhttp

import (
	"net/http"

	"github.com/open-rails/openrails/internal/abusestate"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/captcha"
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

// Options is what the handler mounts beside the public and webhook routes.
type Options struct {
	// Groups are the staff groups that are on and, with Programmatic, the
	// programmatic routes (/v1/app) and SCIM, unless the runtime reads the
	// host's directory.
	Groups         config.RouteGroups
	ProviderRoutes *routesurface.ProviderRoutes
	// Capabilities is what GET /v1/config reports; nil derives it.
	Capabilities *Capabilities
}

// Assembler builds the embedded billing surface from the application graph.
type Assembler struct {
	Cfg          *config.Config
	Runtime      *app.Runtime
	CaptchaStore *captcha.ChallengeStore
	AdminLimiter *middleware.AdminOperationLimiter
	// Auth is the mount's Routes.Auth: who a staff, programmatic or
	// checkout-session request is.
	Auth billingauth.Authenticator
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
// CORS, body limit) — the gin-free analogue of the global engine middleware. The returned handler imports zero gin on the request path.
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
	capabilities := CapabilitiesFor(s.Runtime, opts.Groups, providerRoutes, nil)
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
	})
	if httproutes.StaffGroups(opts.Groups) {
		httproutes.RegisterStaffRoutes(router.NewMux(mux, EmbeddedV1Prefix, s.Runtime), s.Runtime, httproutes.Options{
			Auth:         s.Auth,
			AdminLimiter: s.AdminLimiter,
			Groups:       opts.Groups,
			Capabilities: &capabilities,
		})
	}
	if providerRoutes.Webhooks {
		httproutes.RegisterWebhookRoutes(router.NewMux(mux, EmbeddedV1Prefix+"/webhooks", s.Runtime), s.Runtime)
	}
	if opts.Groups.Programmatic {
		httproutes.RegisterAppRoutes(router.NewMux(mux, EmbeddedV1Prefix, s.Runtime), s.Runtime, httproutes.Options{
			Auth:         s.Auth,
			Groups:       opts.Groups,
			Capabilities: &capabilities,
		})
	}

	// Nothing here knows a request's merchant: each route's gate binds it
	// from the credential, and limits ahead of the gate key on the address.
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
			middleware.PermissiveCORSHTTP(func(*http.Request) bool { return entry.Browser }, s.Auth),
			middleware.RequestLimitsHTTP(middleware.DefaultMaxBodyBytes),
			middleware.HTTPMiddleware(billingauth.ExplicitCredentials),
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
	if (httproutes.StaffGroups(opts.Groups) || opts.Groups.Programmatic) && (s == nil || httproutes.IsNilAuth(s.Auth)) {
		return httproutes.MountError{Route: "admin API", Reason: "needs Routes.Auth"}
	}
	return nil
}
