package server

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/internal/adminconsole"
	"github.com/open-rails/openrails/internal/http/router"
)

// registerAdminConsoleRoutes mounts the merchant admin console SPA (#740) at
// admin_console.path (#1127) per the #754 rule: routes exist ONLY when assets
// are present AND admin_console.enabled. Enabled without assets is a boot error
// (explicit operator intent we cannot satisfy — fail closed); disabled means
// the path 404s like any unknown path, assets or not. The console owns its GET
// subtree, so it registers last and refuses a path over another route.
func (s *Server) registerAdminConsoleRoutes(mux *router.Table) error {
	if s.cfg == nil || !s.cfg.AdminConsole.IsEnabled() {
		return nil
	}
	if !adminconsole.Present(s.consoleAssets) {
		return fmt.Errorf("admin_console.enabled is set but web/admin holds no console build: " +
			"build it (`task admin-build`) before go build — or unset admin_console.enabled")
	}
	path := s.cfg.AdminConsole.MountPath()
	for _, entry := range mux.Entries {
		if entry.Path == path || strings.HasPrefix(entry.Path, path+"/") {
			return fmt.Errorf("admin_console.path %q overlaps the OpenRails route %s %s; choose another path", path, entry.Method, entry.Path)
		}
	}
	cfg := adminconsole.Config{
		AuthBaseURL:            s.cfg.AdminConsole.AuthBaseURL,
		APIBaseURL:             s.cfg.AdminConsole.APIBaseURL,
		NLWidgetsEnabled:       s.cfg.LLM.IsConfigured(),
		AskEnabled:             s.cfg.LLM.AskConfigured(),
		CatalogCopilotEnabled:  s.cfg.LLM.CatalogCopilotConfigured(),
		CatalogDraftingEnabled: s.cfg.LLM.CatalogDraftingConfigured(),
	}
	if cfg.AuthBaseURL == "" {
		cfg.AuthBaseURL = s.controlPlane.AuthAPIBase()
	}
	if cfg.APIBaseURL == "" {
		cfg.APIBaseURL = StandaloneV1Prefix
	}
	console, err := adminconsole.Handler(path, cfg, s.consoleAssets)
	if err != nil {
		return err
	}
	s.handle(mux, http.MethodGet+" "+path, console)
	s.handle(mux, http.MethodGet+" "+path+"/{asset...}", console)
	return nil
}
