package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/http/embedhttp"
	"github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
)

func (s *Server) registerUserRoutesAt(mux *http.ServeMux, apiPrefix string) {
	s.handle(mux, http.MethodGet+" "+apiPrefix+"/captcha/status",
		embedhttp.CaptchaStatusHandler(s.cfg.Captcha, s.captchaStore))
	s.handle(mux, http.MethodGet+" "+apiPrefix+"/captcha/client.js",
		embedhttp.CaptchaClientScriptHandler(s.cfg.Captcha))
	httproutes.RegisterUserRoutes(router.NewMuxRecorded(mux, apiPrefix, s.runtime, s.recordRoute), s.runtime, httproutes.Options{
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

func (s *Server) readyHandler(w http.ResponseWriter, r *http.Request) {
	services, servicesReady := s.requiredServiceHealth(r.Context())
	authReady := s.authenticator != nil
	verbose := r.URL.Query().Get("verbose") == "1" || strings.EqualFold(r.URL.Query().Get("verbose"), "true")
	if !servicesReady || !authReady {
		resp := map[string]any{
			"status":  "not_ready",
			"service": "billing",
			"auth":    map[string]any{"available": authReady},
		}
		if verbose {
			resp["dependencies"] = services
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
		resp["dependencies"] = services
	}
	writeJSON(w, http.StatusOK, resp)
}

// requiredServiceHealth pings the required dependencies (postgres, garnet) on
// demand. /ready is the only consumer of dependency health, so these direct
// pings replaced the background circuit-breaker manager (#666).
func (s *Server) requiredServiceHealth(ctx context.Context) (map[string]any, bool) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	var pgPing, redisPing func(context.Context) error
	if s != nil && s.runtime != nil {
		if s.runtime.DB != nil {
			if pool := s.runtime.DB.Pool(); pool != nil {
				pgPing = pool.Ping
			}
		}
		if rdb := s.runtime.RedisClient; rdb != nil {
			redisPing = func(c context.Context) error { return rdb.Ping(c).Err() }
		}
	}

	services := map[string]any{}
	allReady := true
	for _, dep := range []struct {
		name string
		ping func(context.Context) error
	}{{"postgres", pgPing}, {"garnet", redisPing}} {
		switch {
		case dep.ping == nil:
			services[dep.name] = map[string]any{"available": false, "reason": "not_configured"}
			allReady = false
		default:
			if err := dep.ping(ctx); err != nil {
				services[dep.name] = map[string]any{"available": false, "last_error": err.Error()}
				allReady = false
			} else {
				services[dep.name] = map[string]any{"available": true}
			}
		}
	}
	return services, allReady
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
