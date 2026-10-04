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

// registerStandaloneMetaRoutes registers banner/health endpoints that are appropriate for the
// standalone billing service, but should not be forced onto embedded hosts.
func (s *Server) registerStandaloneMetaRoutes(mux router.Registrar) {
	live := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "service": "billing"})
	})
	httproutes.RegisterMetaRoutes(router.NewMuxRecorded(mux, "", s.runtime, s.recordRoute), httproutes.Options{External: httproutes.External{
		// A simple JSON banner for API servers.
		Banner: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, map[string]any{
				"service":   "billing",
				"status":    "ok",
				"endpoints": []string{"/health/live", "/health/ready", StandaloneV1Prefix},
			})
		}),
		Live:         live,
		Ready:        http.HandlerFunc(s.readyHandler),
		Metrics:      http.HandlerFunc(s.metricsHandler),
		Capabilities: embedhttp.CapabilitiesHandler(embedhttp.AllRouteSets, embedhttp.ProviderRoutesForRuntime(s.runtime, nil)),
	}})
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
		if d.Optional {
			entry["degraded"] = true
		}
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
