package server

import (
	"net/http"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/pkg/adminconsole"
)

// registerAdminConsoleRoutes mounts the merchant admin console SPA (#740) at
// /admin/ when admin_console.enabled is set. Default OFF: nothing is mounted
// and /admin/* 404s like any unknown path.
func (s *Server) registerAdminConsoleRoutes(mux *http.ServeMux) {
	if s.cfg == nil || !s.cfg.AdminConsole.IsEnabled() {
		return
	}
	cfg := adminconsole.Config{
		AuthBaseURL:      s.cfg.AdminConsole.AuthBaseURL,
		APIBaseURL:       s.cfg.AdminConsole.APIBaseURL,
		NLWidgetsEnabled: s.cfg.LLM.IsConfigured(),
	}
	if cfg.AuthBaseURL == "" {
		cfg.AuthBaseURL = ControlPlaneAuthPrefix
	}
	if cfg.APIBaseURL == "" {
		cfg.APIBaseURL = StandaloneV1Prefix
	}
	if !adminconsole.BuiltAssets() {
		// Fail loud but keep serving: config gate is on with only the
		// placeholder embedded; every /admin/* request answers 503.
		log.Error("admin console enabled but web/dist holds no built assets — run `task admin-build` and rebuild")
	}
	s.handle(mux, http.MethodGet+" /admin", http.RedirectHandler("/admin/", http.StatusMovedPermanently))
	s.handle(mux, http.MethodGet+" /admin/", adminconsole.Handler(cfg))
}
