package engine

import (
	"fmt"
	"net/http"
	"strings"

	httproutes "github.com/open-rails/openrails/internal/http/routes"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/adminconsole"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/http/embedhttp"
	"github.com/open-rails/openrails/internal/http/routebundle"
	"github.com/open-rails/openrails/internal/http/router"
)

// Routes materializes the HTTP surface sel selects for the host's root
// router, and fails before anything mounts when a selected group lacks the
// Auth it needs: nothing is ever mounted open. It is sel's groups under
// sel.Prefix, and the admin console at its own path. Every mount of the
// merchant API in one process must agree on MerchantConfig. A standalone
// server's engine refuses: its surface is the server's.
func (e *Engine) Routes(sel config.Routes) (routes []routebundle.Route, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, fmt.Errorf("openrails: client is closed")
	}
	if e.App.Standalone {
		return nil, fmt.Errorf("openrails: this engine is a standalone server's; mount server.Routes")
	}
	sel.CustomerProfiles = append([]config.CustomerRoutes(nil), sel.CustomerProfiles...)
	// Mounting one selection again reuses its handlers, so in-memory rate
	// limits are shared rather than reset per mount.
	key := fmt.Sprintf("%#v", sel)
	if routes, ok := e.routes[key]; ok {
		return append([]routebundle.Route(nil), routes...), nil
	}
	var admit func(http.Handler) http.Handler
	if sel.CookieOrigin != "" {
		var err error
		if admit, err = billingauth.CookieAuthentication(sel.CookieOrigin); err != nil {
			return nil, fmt.Errorf("openrails: Routes.CookieOrigin: %w", err)
		}
	}
	routes, err = e.buildRoutes(sel)
	if err != nil {
		return nil, err
	}
	if admit != nil {
		for i := range routes {
			routes[i].Handler = admit(routes[i].Handler)
		}
	}
	if e.routes == nil {
		e.routes = map[string][]routebundle.Route{}
	}
	e.routes[key] = routes
	return append([]routebundle.Route(nil), routes...), nil
}

func (e *Engine) buildRoutes(sel config.Routes) (routes []routebundle.Route, err error) {
	defer func() {
		if v := recover(); v != nil {
			mount, ok := v.(httproutes.MountError)
			if !ok {
				panic(v)
			}
			routes, err = nil, mount
		}
	}()
	a := e.App
	if sel.Prefix != "" {
		if err := config.ValidateMountPath("Routes.Prefix", sel.Prefix); err != nil {
			return nil, fmt.Errorf("openrails: %w", err)
		}
	}
	table, err := embedhttp.ConfiguredRoutes(a, sel)
	if err != nil {
		return nil, err
	}
	for i := range table.Entries {
		table.Entries[i].Path = sel.Prefix + strings.TrimPrefix(table.Entries[i].Path, "/billing")
	}
	if sel.AdminConsole != nil {
		console, err := e.adminConsoleRoutes(sel)
		if err != nil {
			return nil, err
		}
		path := console[0].Path
		for _, entry := range table.Entries {
			if entry.Path == path || strings.HasPrefix(entry.Path, path+"/") || strings.HasPrefix(path, sel.Prefix+"/v1/") {
				return nil, fmt.Errorf("openrails: Routes.AdminConsole.Path %q overlaps the billing route %s %s; choose another path", path, entry.Method, entry.Path)
			}
		}
		table.Entries = append(table.Entries, console...)
	}
	if err := embedhttp.ValidateRouteTable(table); err != nil {
		return nil, err
	}
	if sel.Merchant || sel.MerchantConfig {
		// Catalog edits reach staff when the configuration routes are mounted
		// and the host's catalog document is not the truth.
		if err := a.Runtime.CatalogEdits.Decide(sel.MerchantConfig && a.Config.Catalog == nil); err != nil {
			return nil, err
		}
	}
	logMount(sel, len(table.Entries))
	return routebundle.FromTable(table), nil
}

// logMount names the Auth each mounted group answers to: the merchant API at
// WARN, since it moves money.
func logMount(sel config.Routes, routes int) {
	fields := log.Fields{"prefix": sel.Prefix + "/v1", "routes": routes, "auth": fmt.Sprintf("%T", sel.Auth), "storefront": sel.Storefront, "customers": sel.Customers != config.CustomersNone, "customer_profiles": len(sel.CustomerProfiles)}
	if sel.Merchant || sel.MerchantConfig {
		fields["merchant_config"] = sel.MerchantConfig
		log.WithFields(fields).Warn("openrails: merchant API mounted; Auth.RequirePermission gates each route with its guard")
		return
	}
	log.WithFields(fields).Info("openrails: routes mounted")
}

// adminConsoleRoutes serves the console sel selects at its path, against the
// merchant API at sel.Prefix and the host's AuthKit.
func (e *Engine) adminConsoleRoutes(sel config.Routes) ([]router.Entry, error) {
	a := e.App
	if !sel.Merchant {
		return nil, fmt.Errorf("openrails: Routes.AdminConsole drives the merchant API; set Routes.Merchant")
	}
	path := config.AdminConsolePath(sel.AdminConsole)
	if err := config.ValidateMountPath("Routes.AdminConsole.Path", path); err != nil {
		return nil, fmt.Errorf("openrails: %w", err)
	}
	if err := config.ValidateConsoleExtensions("Routes.AdminConsole.Extensions", sel.AdminConsole.Extensions); err != nil {
		return nil, fmt.Errorf("openrails: %w", err)
	}
	if !adminconsole.Present(a.ConsoleAssets) {
		return nil, fmt.Errorf("openrails: Routes.AdminConsole needs a console build: supply Deps.ConsoleAssets (scripts/build-admin-console.sh)")
	}
	cfg := a.Config
	authBase := sel.AdminConsole.AuthBaseURL
	if authBase == "" {
		return nil, fmt.Errorf("openrails: Routes.AdminConsole has no sign-in method: set AdminConsole.AuthBaseURL")
	}
	handler, err := adminconsole.Handler(path, adminconsole.Config{
		AuthBaseURL:            authBase,
		APIBaseURL:             sel.Prefix + "/v1",
		NLWidgetsEnabled:       config.LLMConfigured(cfg.LLM),
		AskEnabled:             config.LLMAskConfigured(cfg.LLM),
		CatalogCopilotEnabled:  config.LLMCatalogCopilotConfigured(cfg.LLM),
		CatalogDraftingEnabled: config.LLMCatalogDraftingConfigured(cfg.LLM),
		Extensions:             sel.AdminConsole.Extensions,
	}, a.ConsoleAssets)
	if err != nil {
		return nil, fmt.Errorf("openrails: %w", err)
	}
	return []router.Entry{
		{Method: http.MethodGet, Path: path, Handler: handler},
		{Method: http.MethodGet, Path: path + "/{asset...}", Handler: handler},
	}, nil
}
