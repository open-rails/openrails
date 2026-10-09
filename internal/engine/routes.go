package engine

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/internal/adminconsole"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/http/embedhttp"
	"github.com/open-rails/openrails/internal/http/routebundle"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/merchanttarget"
	"github.com/open-rails/openrails/internal/operator"
)

// Routes materializes the HTTP surface sel selects for the host's root
// router, and fails before anything mounts when the engine lacks what a group
// needs. Without a control plane that is sel's groups under sel.Prefix (and
// the admin console at its own path); with one it is the standalone surface at
// the root, to which sel adds only CatalogEdits, AdminConsole, Delegated
// CustomerProfiles and CookieOrigin. Every mount of the merchant API in one
// process must agree on CatalogEdits.
func (e *Engine) Routes(sel config.Routes) ([]routebundle.Route, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, fmt.Errorf("openrails: client is closed")
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
	routes, err := e.buildRoutes(sel)
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

func (e *Engine) buildRoutes(sel config.Routes) ([]routebundle.Route, error) {
	a := e.App
	if sel.Prefix != "" {
		if err := config.ValidateMountPath("Routes.Prefix", sel.Prefix); err != nil {
			return nil, fmt.Errorf("openrails: %w", err)
		}
	}
	if operator.Get(a) != nil {
		if sel.Prefix != "" || sel.Storefront || sel.Merchant || sel.Customers != config.CustomersNone {
			return nil, fmt.Errorf("openrails: with Config.ControlPlane, Routes is the standalone surface at the root; set only CatalogEdits, AdminConsole, Delegated CustomerProfiles and CookieOrigin")
		}
		for _, profile := range sel.CustomerProfiles {
			if !profile.Delegated {
				return nil, fmt.Errorf("openrails: with Config.ControlPlane, CustomerProfiles must be Delegated (Deps.AuthenticateCustomer)")
			}
		}
		profiles := embedhttp.CustomerProfiles(sel, "")
		extra, err := embedhttp.BuildCustomerRoutes(a, profiles, a.Runtime.Auth)
		if err != nil {
			return nil, err
		}
		// The server resolved selectors on its own routes.
		router.ResolveMerchantSelectors(extra, "", func(ctx context.Context, r *http.Request) (billingauth.Target, error) {
			return merchanttarget.Resolve(ctx, r, a.Runtime.Merchants, a.Runtime.ConfiguredMerchant(), "")
		}, embedhttp.CustomerPrefixes("", profiles)...)
		table, err := operator.StandaloneRoutes(a, sel)
		if err != nil {
			return nil, err
		}
		table.Entries = append(table.Entries, extra.Entries...)
		if err := embedhttp.ValidateRouteTable(table); err != nil {
			return nil, err
		}
		return routebundle.FromTable(table), nil
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
	if sel.Merchant {
		if err := a.Runtime.CatalogEdits.Decide(sel.CatalogEdits); err != nil {
			return nil, err
		}
	}
	return routebundle.FromTable(table), nil
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
	if err := config.ValidateNewMerchantURL("Routes.AdminConsole.NewMerchantURL", sel.AdminConsole.NewMerchantURL); err != nil {
		return nil, fmt.Errorf("openrails: %w", err)
	}
	if !adminconsole.Present(a.ConsoleAssets) {
		return nil, fmt.Errorf("openrails: Routes.AdminConsole needs a console build: supply Deps.ConsoleAssets (scripts/build-admin-console.sh)")
	}
	authBase := sel.AdminConsole.AuthBaseURL
	if authBase == "" {
		authBase = e.authAPIBase
	}
	if authBase == "" {
		return nil, fmt.Errorf("openrails: set Routes.AdminConsole.AuthBaseURL, the AuthKit JSON API staff sign in through")
	}
	cfg := a.Config
	handler, err := adminconsole.Handler(path, adminconsole.Config{
		AuthBaseURL:            authBase,
		APIBaseURL:             sel.Prefix + "/v1",
		NLWidgetsEnabled:       config.LLMConfigured(cfg.LLM),
		AskEnabled:             config.LLMAskConfigured(cfg.LLM),
		CatalogCopilotEnabled:  config.LLMCatalogCopilotConfigured(cfg.LLM),
		CatalogDraftingEnabled: config.LLMCatalogDraftingConfigured(cfg.LLM),
		NewMerchantURL:         sel.AdminConsole.NewMerchantURL,
	}, a.ConsoleAssets)
	if err != nil {
		return nil, fmt.Errorf("openrails: %w", err)
	}
	return []router.Entry{
		{Method: http.MethodGet, Path: path, Handler: handler},
		{Method: http.MethodGet, Path: path + "/{asset...}", Handler: handler},
	}, nil
}
