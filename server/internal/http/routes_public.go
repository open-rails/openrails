package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/buildinfo"
	"github.com/open-rails/openrails/internal/http/embedhttp"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/internal/logx"
	"github.com/open-rails/openrails/server/internal/adminconsole"
)

// registerUserRoutesAt mounts the buyer-facing purchase/catalog surface —
// browser tier (#765): every pattern registered here is also recorded into
// browserTierRoutes, so it's eligible for the static permissive CORS policy.
func (s *Server) registerUserRoutesAt(mux router.Registrar, apiPrefix string) {
	httproutes.RegisterUserRoutes(router.NewMuxRecorded(mux, apiPrefix, s.runtime, s.recordBrowserRoute), s.runtime, httproutes.Options{Auth: s.auth})
}

func (s *Server) registerUserRoutes(mux router.Registrar) {
	s.registerUserRoutesAt(mux, StandaloneV1Prefix)
}

// registerWebhookRoutes mounts the canonical account-specific callback surface.
// Embedded and standalone runtimes share account/environment resolution and
// provider verification.
func (s *Server) registerWebhookRoutes(mux router.Registrar) {
	httproutes.RegisterWebhookRoutes(router.NewMuxRecorded(mux, StandaloneV1Prefix+"/webhooks", s.runtime, s.recordRoute), s.runtime)
}

// registerStandaloneMetaRoutes registers the summary and health, the
// standalone server's process surface, and the public configuration, which is
// browser tier.
func (s *Server) registerStandaloneMetaRoutes(mux router.Registrar) {
	httproutes.RegisterMetaRoutes(s.Log, router.NewMuxRecorded(mux, "", s.runtime, s.recordRoute), httproutes.Options{External: httproutes.External{
		Summary: http.HandlerFunc(s.summaryHandler),
		Live: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			httprequest.New(s.Log, w, r, nil).SuccessJSON(httproutes.Health{Status: httproutes.HealthOK})
		}),
		Ready: http.HandlerFunc(s.readyHandler),
	}})
	httproutes.RegisterMetaRoutes(s.Log, router.NewMuxRecorded(mux, "", s.runtime, s.recordBrowserRoute), httproutes.Options{Capabilities: s.capabilities()})
}

// capabilities is what the standalone server serves: every bundle.
func (s *Server) capabilities() *billing.Capabilities {
	caps := embedhttp.CapabilitiesFor(s.runtime, s.groups, embedhttp.ProviderRoutesForRuntime(s.runtime, nil), nil)
	return &caps
}

// summaryHandler serves GET /: the service, its version, whether
// /health/ready answers ready, and where its surfaces are. It is public, so it
// says no more than the health routes.
func (s *Server) summaryHandler(w http.ResponseWriter, r *http.Request) {
	status := httproutes.ServiceReady
	if s.ready(r.Context()) != nil {
		status = httproutes.ServiceNotReady
	}
	links := httproutes.ServiceLinks{API: StandaloneV1Prefix, Config: StandaloneV1Prefix + "/config", Live: "/health/live", Ready: "/health/ready"}
	if !s.consoleOff {
		console := adminconsole.Path + "/"
		links.Console = &console
	}
	httprequest.New(s.Log, w, r, nil).SuccessJSON(httproutes.ServiceSummary{Service: "openrails", Version: buildinfo.Get().Version, Status: status, Links: links})
}

// errDraining is readiness once the process drains.
var errDraining = errors.New("readiness: draining")

// readyHandler serves /health/ready: 200 when ready, else 503. Which
// dependency failed, and why, goes to the log: the route is public.
func (s *Server) readyHandler(w http.ResponseWriter, r *http.Request) {
	req := httprequest.New(s.Log, w, r, nil)
	switch err := s.ready(r.Context()); {
	case errors.Is(err, errDraining):
		req.ErrorCode(billing.CodeServiceUnavailable, "draining")
	case err != nil:
		s.Log.WarnContext(r.Context(), "not ready", logx.Err(err))
		req.ErrorCode(billing.CodeServiceUnavailable, "not ready")
	default:
		req.SuccessJSON(httproutes.Health{Status: httproutes.HealthReady})
	}
}

// ready runs the checks Engine.Ready runs and the staff second factor
// (ControlPlane.SecondFactor); errDraining once the process drains.
func (s *Server) ready(ctx context.Context) error {
	if s != nil && s.draining.Load() {
		return errDraining
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	var runtime *app.Runtime
	if s != nil {
		runtime = s.runtime
	}
	_, err := runtime.Ready(ctx)
	if err == nil && (s == nil || s.auth == nil) {
		err = errors.New("readiness: authentication is not available")
	}
	// Staff sign-ins need a second factor: one none can enroll fails
	// readiness, not just a log line at boot.
	if err == nil {
		err = s.controlPlane.SecondFactor(ctx)
	}
	return err
}
