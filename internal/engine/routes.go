package engine

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/billing"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/http/embedhttp"
	"github.com/open-rails/openrails/internal/http/routebundle"
	"github.com/open-rails/openrails/internal/http/router"
)

// Routes materializes the HTTP surface sel selects for the host's root
// router, and fails before anything mounts without the Auth it needs:
// nothing is ever mounted open. It is the public, customer and webhook routes
// and the route groups sel.RouteGroups turns on, under sel.Prefix. A
// standalone server's engine refuses: its surface is the server's.
func (e *Engine) Routes(sel config.Routes) (routes []routebundle.Route, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, fmt.Errorf("openrails: engine is closed")
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

// PublicConfig is the document GET /v1/config serves on the mount sel
// selects, read without a request.
func (e *Engine) PublicConfig(sel config.Routes) (billing.PublicConfig, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return billing.PublicConfig{}, fmt.Errorf("openrails: engine is closed")
	}
	if e.App.Standalone {
		return billing.PublicConfig{}, fmt.Errorf("openrails: this engine is a standalone server's; read the server's GET /v1/config")
	}
	return embedhttp.ConfiguredPublicConfig(e.App, sel)
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
	if err := embedhttp.ValidateRouteTable(table); err != nil {
		return nil, err
	}
	logMount(sel, len(table.Entries))
	return routebundle.FromTable(table), nil
}

// logMount names the Auth the mounted route groups answer to: the admin API
// at WARN, since it moves money.
func logMount(sel config.Routes, routes int) {
	g := sel.RouteGroups
	fields := log.Fields{
		"prefix": sel.Prefix + "/v1", "routes": routes, "auth": fmt.Sprintf("%T", sel.Auth),
		"admin": g.Admin, "catalog": g.Catalog, "merchant_config": g.MerchantConfig, "metrics": g.Metrics, "programmatic": g.Programmatic,
	}
	if httproutes.StaffGroups(g) {
		log.WithFields(fields).Warn("openrails: staff routes mounted; each asks the caller's Can for its permission at the merchant or in the root scope")
		return
	}
	log.WithFields(fields).Info("openrails: routes mounted")
}

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
