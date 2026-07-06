package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/http/embedhttp"
	"github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
)

// registerUserRoutesAt mounts the buyer-facing checkout/catalog surface —
// browser tier (#765): every pattern registered here is also recorded into
// browserTierRoutes, so it's eligible for the static permissive CORS policy.
func (s *Server) registerUserRoutesAt(mux *http.ServeMux, apiPrefix string) {
	s.handleBrowser(mux, http.MethodGet+" "+apiPrefix+"/captcha/status",
		embedhttp.CaptchaStatusHandler(s.cfg.Captcha, s.captchaStore, s.trustedProxies()))
	s.handleBrowser(mux, http.MethodGet+" "+apiPrefix+"/captcha/client.js",
		embedhttp.CaptchaClientScriptHandler(s.cfg.Captcha))
	httproutes.RegisterUserRoutes(router.NewMuxRecorded(mux, apiPrefix, s.runtime, s.recordBrowserRoute), s.runtime, httproutes.Options{
		Authenticator: s.authenticator,
	})
}

func (s *Server) registerUserRoutes(mux *http.ServeMux) {
	s.registerUserRoutesAt(mux, StandaloneV1Prefix)
}

// registerWebhookRoutes mounts the canonical provider-only webhook surface (#650):
// /webhooks/:provider (NMI/CCBill, merchant derived from payload account identity) and
// /webhooks/:provider/:account_id (direct Stripe). Standalone mounts this; embedded hosts
// use the merchant-scoped surface because they pin one merchant in context.
func (s *Server) registerWebhookRoutes(mux *http.ServeMux) {
	httproutes.RegisterWebhookRoutes(router.NewMuxRecorded(mux, StandaloneV1Prefix+"/webhooks", s.runtime, s.recordRoute), s.runtime)
}

// registerStandaloneMetaRoutes registers banner/health endpoints that are appropriate for the
// standalone billing service, but should not be forced onto embedded hosts.
func (s *Server) registerStandaloneMetaRoutes(mux *http.ServeMux) {
	// Root: simple JSON banner for API servers. "GET /{$}" pins the exact root
	// (a bare "/" ServeMux pattern would swallow every unmatched path).
	s.handle(mux, http.MethodGet+" /{$}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"service":   "billing",
			"status":    "ok",
			"endpoints": []string{"/health/live", "/health/ready", StandaloneV1Prefix},
		})
	}))

	live := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "service": "billing"})
	})
	s.handle(mux, http.MethodGet+" /health/live", live)
	s.handle(mux, http.MethodGet+" /health/ready", http.HandlerFunc(s.readyHandler))

	// Capability discovery (#623): which route groups this deployment serves.
	// Standalone mounts the full surface, so it advertises StandaloneDefaultRouteSets.
	// Same hand-written handler the embedded surface serves at /billing/v1/capabilities.
	s.handle(mux, http.MethodGet+" "+StandaloneV1Prefix+"/capabilities",
		embedhttp.CapabilitiesHandler(embedhttp.StandaloneDefaultRouteSets))

	// Kubernetes-style health check endpoints (aliases)
	s.handle(mux, http.MethodGet+" /healthz", live)
	s.handle(mux, http.MethodGet+" /readyz", http.HandlerFunc(s.readyHandler))
}

// readyHandler serves /health/ready and /readyz. Dependency checks are the
// SAME ones pkg/embedded.Embedded.Ready runs (#748, internal/app.Runtime.Ready)
// — standalone and embedded report one shared posture, never two.
func (s *Server) readyHandler(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	var runtime *app.Runtime
	if s != nil {
		runtime = s.runtime
	}
	deps, err := runtime.Ready(ctx)
	authReady := s != nil && s.authenticator != nil
	verbose := r.URL.Query().Get("verbose") == "1" || strings.EqualFold(r.URL.Query().Get("verbose"), "true")

	if err != nil || !authReady {
		resp := map[string]any{
			"status":  "not_ready",
			"service": "billing",
			"auth":    map[string]any{"available": authReady},
		}
		if verbose {
			resp["dependencies"] = dependencyStatus(deps)
		}
		writeJSON(w, http.StatusServiceUnavailable, resp)
		return
	}

	resp := map[string]any{
		"status":  "ready",
		"service": "billing",
		"auth":    map[string]any{"available": true},
	}
	if verbose {
		resp["dependencies"] = dependencyStatus(deps)
	}
	writeJSON(w, http.StatusOK, resp)
}

// dependencyStatus renders Runtime.Ready's per-dependency detail for the
// verbose /readyz payload (#748).
func dependencyStatus(deps []app.ReadinessDependency) map[string]any {
	out := make(map[string]any, len(deps))
	for _, d := range deps {
		if d.Available {
			out[d.Name] = map[string]any{"available": true}
			continue
		}
		entry := map[string]any{"available": false}
		if d.Err != nil {
			entry["last_error"] = d.Err.Error()
		}
		out[d.Name] = entry
	}
	return out
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
