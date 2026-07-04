package server

import (
	"fmt"
	"net/http"

	"github.com/open-rails/openrails/pkg/adminconsole"
)

// registerAdminConsoleRoutes mounts the merchant admin console SPA (#740) at
// /admin/ per the #754 rule: routes exist ONLY when assets are present AND
// admin_console.enabled. Enabled without assets is a boot error (explicit
// operator intent we cannot satisfy — fail closed); disabled means /admin/*
// 404s like any unknown path, assets or not.
func (s *Server) registerAdminConsoleRoutes(mux *http.ServeMux) error {
	if s.cfg == nil || !s.cfg.AdminConsole.IsEnabled() {
		return nil
	}
	if !adminconsole.Present(s.consoleAssets) {
		return fmt.Errorf("admin_console.enabled is set but no console assets were provided: " +
			"build the SPA (scripts/build-admin-console.sh; in-repo `task admin-build`) and rebuild " +
			"with `-tags console_assets`, or pass the built assets via embed.WithAdminConsole — " +
			"or unset admin_console.enabled")
	}
	cfg := adminconsole.Config{
		AuthBaseURL:      s.cfg.AdminConsole.AuthBaseURL,
		APIBaseURL:       s.cfg.AdminConsole.APIBaseURL,
		NLWidgetsEnabled: s.cfg.LLM.IsConfigured(),
		AskEnabled:       s.cfg.LLM.AskConfigured(),
	}
	if cfg.AuthBaseURL == "" {
		cfg.AuthBaseURL = ControlPlaneAuthPrefix
	}
	if cfg.APIBaseURL == "" {
		cfg.APIBaseURL = StandaloneV1Prefix
	}
	s.handle(mux, http.MethodGet+" /admin", http.RedirectHandler("/admin/", http.StatusMovedPermanently))
	s.handle(mux, http.MethodGet+" /admin/", adminconsole.Handler(cfg, s.consoleAssets))
	return nil
}
