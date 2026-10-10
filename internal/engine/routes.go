package engine

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/billing"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/adminconsole"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/http/embedhttp"
	"github.com/open-rails/openrails/internal/http/routebundle"
	"github.com/open-rails/openrails/internal/http/router"
)

// Routes materializes the HTTP surface sel selects for the host's root
// router, and fails before anything mounts without the Auth it needs:
// nothing is ever mounted open. It is the public, customer and webhook routes
// and the route groups sel.RouteGroups turns on, under sel.Prefix, and the
// admin console at its /admin. A standalone server's engine refuses: its
// surface is the server's.
func (e *Engine) Routes(sel config.Routes) (routes []routebundle.Route, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, fmt.Errorf("openrails: client is closed")
	}
	if e.App.Standalone {
		return nil, fmt.Errorf("openrails: this engine is a standalone server's; mount server.Routes")
	}
	// Mounting one selection again reuses its handlers, so in-memory rate
	// limits are shared rather than reset per mount.
	key := fmt.Sprintf("%#v", sel)
	if routes, ok := e.routes[key]; ok {
		return append([]routebundle.Route(nil), routes...), nil
	}
	routes, err = e.buildRoutes(sel)
	if err != nil {
		return nil, err
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
	if sel.AdminConsole {
		console, err := e.adminConsoleRoutes(sel)
		if err != nil {
			return nil, err
		}
		table.Entries = append(table.Entries, console...)
	}
	if err := embedhttp.ValidateRouteTable(table); err != nil {
		return nil, err
	}
	perms, err := embedhttp.RoutePermissions(sel)
	if err != nil {
		return nil, err
	}
	logMount(sel, perms, len(table.Entries))
	return routebundle.FromTable(table), nil
}

// logMount names the Auth each mounted route group answers to: the admin API at
// WARN, since it moves money.
func logMount(sel config.Routes, perms httproutes.Permissions, routes int) {
	fields := log.Fields{"prefix": sel.Prefix + "/v1", "routes": routes, "auth": fmt.Sprintf("%T", sel.Auth), "programmatic": sel.RouteGroups.Programmatic}
	if perms != (httproutes.Permissions{}) {
		fields["customer_read"], fields["customer_update"], fields["catalog"], fields["merchant_config"], fields["metrics"] = perms.AdminRead, perms.AdminUpdate, perms.Catalog, perms.MerchantConfig, perms.Metrics
		fields["scope_authority"], fields["scope_id"] = sel.Scope.Authority, sel.Scope.ID
		log.WithFields(fields).Warn("openrails: staff routes mounted; each asks the caller's Can for its group's permission in Routes.Scope")
		return
	}
	log.WithFields(fields).Info("openrails: routes mounted")
}

// adminConsoleRoutes serves the console at sel.Prefix's /admin, against the
// admin API there and the host's AuthKit (/api/v1) on the same origin.
func (e *Engine) adminConsoleRoutes(sel config.Routes) ([]router.Entry, error) {
	a := e.App
	if b := sel.RouteGroups; !b.Admin && !b.Catalog && !b.MerchantConfig && !b.Metrics {
		return nil, fmt.Errorf("openrails: AdminConsole drives the staff routes; turn on at least one of RouteGroups.Admin, Catalog, MerchantConfig or Metrics")
	}
	if !adminconsole.Present(a.ConsoleAssets) {
		return nil, fmt.Errorf("openrails: Routes.AdminConsole needs a console build: supply Deps.ConsoleAssets (scripts/build-admin-console.sh)")
	}
	path := sel.Prefix + "/admin"
	console := adminconsole.Config{AuthBaseURL: ConsoleAuthBaseURL, APIBaseURL: sel.Prefix + "/v1"}
	console.Assistants(a.Config.LLM, sel.RouteGroups.Catalog, sel.RouteGroups.Metrics)
	// The console acts for the merchant the engine serves.
	if mid := a.Runtime.ConfiguredMerchant(); !mid.IsZero() && a.Config.Merchant.Slug != "" {
		console.Merchant = &adminconsole.Merchant{ID: mid.String(), Slug: a.Config.Merchant.Slug, DisplayName: a.Config.Merchant.DisplayName}
	}
	handler, err := adminconsole.Handler(path, console, a.ConsoleAssets)
	if err != nil {
		return nil, fmt.Errorf("openrails: %w", err)
	}
	return []router.Entry{
		{Method: http.MethodGet, Path: path, Handler: handler},
		{Method: http.MethodGet, Path: path + "/{asset...}", Handler: handler},
	}, nil
}

// ConsoleAuthBaseURL is where an embedded console signs staff in: AuthKit's
// JSON API on the host's origin.
const ConsoleAuthBaseURL = "/api/v1"

// SCIMHandler is the SCIM service provider for mid, rooted at the SCIM root
// (/Users, /Bulk): a directory in the same process pushes to it, and holding
// the engine is the authority. An engine reading Deps.UserInfo keeps no copy
// and refuses.
func (e *Engine) SCIMHandler(mid billing.MerchantID) (http.Handler, error) {
	rt := e.App.Runtime
	if rt.HostUserInfo {
		return nil, fmt.Errorf("openrails: Deps.UserInfo reads your directory; there is no copy to provision")
	}
	if mid.IsZero() {
		return nil, fmt.Errorf("openrails: SCIMHandler needs the Client's merchant")
	}
	table := &router.Table{}
	httproutes.RegisterSCIMRoutes(router.NewMux(table, "", rt), rt, mid)
	return table.Handler(), nil
}
