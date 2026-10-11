package server

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/server/internal/adminconsole"
	"github.com/open-rails/openrails/server/internal/controlplane"
	"github.com/open-rails/openrails/server/internal/hostconfig"
)

// registerAdminConsoleRoutes mounts the merchant admin console SPA (#740)
// when the surface selects one (admin_console.enabled): without it no console
// route exists. Selected without a build is a boot error. The console owns its
// GET subtree, so it registers last and refuses a path over another route.
func (s *Server) registerAdminConsoleRoutes(mux *router.Table) error {
	if s.adminConsole == nil {
		return nil
	}
	if g := s.groups; !g.Admin && !g.Catalog && !g.MerchantConfig && !g.Metrics {
		return fmt.Errorf("admin_console.enabled drives the staff routes; turn on at least one of route_groups.admin, catalog, merchant_config or metrics")
	}
	if !adminconsole.Present(s.consoleAssets) {
		return fmt.Errorf("admin_console.enabled is set but server/web/console holds no console build: " +
			"build it (`task admin-build`) before go build — or unset admin_console.enabled")
	}
	path := hostconfig.AdminConsolePath(s.adminConsole)
	if err := config.ValidateMountPath("admin_console.path", path); err != nil {
		return err
	}
	for _, entry := range mux.Entries {
		if entry.Path == path || strings.HasPrefix(entry.Path, path+"/") {
			return fmt.Errorf("admin_console.path %q overlaps the OpenRails route %s %s; choose another path", path, entry.Method, entry.Path)
		}
	}
	cfg := adminconsole.Config{
		AuthBaseURL: s.adminConsole.AuthBaseURL,
		APIBaseURL:  StandaloneV1Prefix,
	}
	issuer, err := consoleIssuer(s.consoleIssuer, s.resource)
	if err != nil {
		return err
	}
	cfg.Issuer = issuer
	if issuer == nil && !s.controlPlane.LocalSignIn() {
		return fmt.Errorf("admin_console has no sign-in method: declare admin_console.issuer (a trusted issuer) or set local_sign_in")
	}
	if issuer == nil {
		// A session of the server's own accounts is bound to no merchant:
		// staff act on one through a token exchanged for it.
		if s.resource == "" {
			return fmt.Errorf("admin_console with local sign-in needs the server's resource: set public_billing_base_url (https), or auth.resource.id; staff act on a merchant through a token exchanged for it")
		}
		cfg.Exchange = &adminconsole.Exchange{TokenEndpoint: s.controlPlane.TokenEndpoint(), ClientID: controlplane.ConsoleClientID, Resource: s.resource, Scope: billing.ScopeMerchant}
	}
	if cfg.AuthBaseURL == "" && s.controlPlane.LocalSignIn() {
		cfg.AuthBaseURL = s.controlPlane.AuthAPIBase()
	}
	console, err := adminconsole.Handler(path, cfg, s.consoleAssets)
	if err != nil {
		return err
	}
	s.handle(mux, http.MethodGet+" "+path, console)
	s.handle(mux, http.MethodGet+" "+path+"/{asset...}", console)
	return nil
}

// consoleIssuer is the console's issuer, asked for tokens for resource:
// nil without one. AuthKit refuses its tokens unless it is trusted.
func consoleIssuer(console *hostconfig.ConsoleIssuer, resource string) (*adminconsole.Issuer, error) {
	if console == nil {
		return nil, nil
	}
	url, name, err := hostconfig.ResolveConsoleIssuer(console, resource)
	if err != nil {
		return nil, err
	}
	scope := strings.Join(strings.Fields(console.Scope), " ")
	if scope == "" {
		scope = hostconfig.ConsoleScope
	}
	return &adminconsole.Issuer{URL: url, ClientID: strings.TrimSpace(console.ClientID), Name: name, Resource: strings.TrimSpace(resource), Scope: scope}, nil
}
