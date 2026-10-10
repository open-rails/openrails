package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/http/embedhttp"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/internal/scim"
)

// registerUserRoutesAt mounts the buyer-facing checkout/catalog surface —
// browser tier (#765): every pattern registered here is also recorded into
// browserTierRoutes, so it's eligible for the static permissive CORS policy.
func (s *Server) registerUserRoutesAt(mux router.Registrar, apiPrefix string) {
	httproutes.RegisterUserRoutes(router.NewMuxRecorded(mux, apiPrefix, s.runtime, s.recordBrowserRoute), s.runtime, httproutes.Options{
		Auth:              s.customerAuth(),
		AuthBindsMerchant: true,
		External: httproutes.External{
			CaptchaStatus: embedhttp.CaptchaStatusHandler(s.cfg.Captcha, s.captchaStore, s.trustedProxies()),
			CaptchaScript: embedhttp.CaptchaClientScriptHandler(s.cfg.Captcha),
		},
	})
}

func (s *Server) registerUserRoutes(mux router.Registrar) {
	s.registerUserRoutesAt(mux, StandaloneV1Prefix)
}

// registerWebhookRoutes mounts the canonical account-specific callback surface.
// Embedded and standalone runtimes share account/environment resolution,
// configured merchant restrictions and provider verification.
func (s *Server) registerWebhookRoutes(mux router.Registrar) {
	httproutes.RegisterWebhookRoutes(router.NewMuxRecorded(mux, StandaloneV1Prefix+"/webhooks", s.runtime, s.recordRoute), s.runtime)
}

// registerProvisioningRoutes mounts SCIM 2.0 at /scim/v2: a merchant's
// provisioning token, or a client-credentials access token with scope scim
// from its trusted issuer, names the merchant.
func (s *Server) registerProvisioningRoutes(mux router.Registrar) {
	auth := scim.Authenticator{Tokens: scim.Tokens{DB: s.runtime.DB}}
	if s.controlPlane != nil {
		auth.Resource = s.controlPlane.ResolveProvisioningToken
	}
	httproutes.RegisterProvisioningRoutes(router.NewMuxRecorded(mux, "/scim/v2", s.runtime, s.recordRoute), s.runtime, httproutes.Options{Provisioning: auth.Authenticate})
}

// registerStandaloneMetaRoutes registers health, the standalone server's
// process surface, and the public configuration, which is browser tier.
func (s *Server) registerStandaloneMetaRoutes(mux router.Registrar) {
	httproutes.RegisterMetaRoutes(router.NewMuxRecorded(mux, "", s.runtime, s.recordRoute), httproutes.Options{External: httproutes.External{
		Live: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			httprequest.NewHTTP(w, r, nil).SuccessJSON(httproutes.Health{Status: httproutes.HealthOK})
		}),
		Ready: http.HandlerFunc(s.readyHandler),
	}})
	httproutes.RegisterMetaRoutes(router.NewMuxRecorded(mux, "", s.runtime, s.recordBrowserRoute), httproutes.Options{Capabilities: s.capabilities()})
}

// capabilities is what the standalone server serves: every bundle.
func (s *Server) capabilities() *billing.Capabilities {
	caps := embedhttp.CapabilitiesFor(s.runtime, staffPermissions, embedhttp.ProviderRoutesForRuntime(s.runtime, nil), nil)
	caps.RouteGroups[string(httproutes.Provisioning)] = true
	return &caps
}

// readyHandler serves /health/ready with the checks embedded Client.Ready
// runs (#748), and 503 once the process drains. Which dependency failed, and
// why, goes to the log: the route is public.
func (s *Server) readyHandler(w http.ResponseWriter, r *http.Request) {
	if s != nil && s.draining.Load() {
		httprequest.NewHTTP(w, r, nil).ErrorCode(billing.CodeServiceUnavailable, "draining")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	var runtime *app.Runtime
	if s != nil {
		runtime = s.runtime
	}
	_, err := runtime.Ready(ctx)
	if err == nil && (s == nil || s.authenticator == nil) {
		err = errors.New("readiness: authentication is not available")
	}
	req := httprequest.NewHTTP(w, r, nil)
	if err != nil {
		log.WithError(err).Warn("not ready")
		req.ErrorCode(billing.CodeServiceUnavailable, "not ready")
		return
	}
	req.SuccessJSON(httproutes.Health{Status: httproutes.HealthReady})
}
