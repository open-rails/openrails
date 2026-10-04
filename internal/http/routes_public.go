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
)

// registerUserRoutesAt mounts the buyer-facing checkout/catalog surface —
// browser tier (#765): every pattern registered here is also recorded into
// browserTierRoutes, so it's eligible for the static permissive CORS policy.
func (s *Server) registerUserRoutesAt(mux router.Registrar, apiPrefix string) {
	httproutes.RegisterUserRoutes(router.NewMuxRecorded(mux, apiPrefix, s.runtime, s.recordBrowserRoute), s.runtime, httproutes.Options{
		Authenticator: s.authenticator,
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

// registerStandaloneMetaRoutes registers health, metrics and capability
// discovery: the standalone server's process surface, which embedded hosts
// supply themselves.
func (s *Server) registerStandaloneMetaRoutes(mux router.Registrar) {
	httproutes.RegisterMetaRoutes(router.NewMuxRecorded(mux, "", s.runtime, s.recordRoute), httproutes.Options{External: httproutes.External{
		Live: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			httprequest.NewHTTP(w, r, nil).SuccessJSON(httproutes.Health{Status: httproutes.HealthOK})
		}),
		Ready:   http.HandlerFunc(s.readyHandler),
		Metrics: http.HandlerFunc(s.metricsHandler),
		Capabilities: embedhttp.CapabilitiesHandler(s.runtime, embedhttp.AllRouteSets, embedhttp.ProviderRoutesForRuntime(s.runtime, nil),
			// Team invitations mint register-and-join links when the control
			// plane's posture allows them.
			map[string]bool{"team_invites": s.controlPlane != nil && s.controlPlane.InvitesEnabled()}),
	}})
}

// readyHandler serves /health/ready with the checks embedded Client.Ready
// runs (#748). Which dependency failed, and why, goes to the log: the route
// is public.
func (s *Server) readyHandler(w http.ResponseWriter, r *http.Request) {
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
