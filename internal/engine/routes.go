package engine

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/http/embedhttp"
	"github.com/open-rails/openrails/internal/http/routebundle"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/merchanttarget"
	"github.com/open-rails/openrails/internal/operator"
)

// Routes materializes the configured HTTP surface once, relative to the
// host's mount. Without a control plane that is Config.HTTP's route groups
// (mount under /billing for /billing/v1/*); with one it is the standalone
// surface (see RoutesRequireRoot).
func (e *Engine) Routes() ([]routebundle.Route, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, fmt.Errorf("openrails: client is closed")
	}
	if e.routes == nil {
		routes, err := e.buildRoutes()
		if err != nil {
			return nil, err
		}
		if e.http != nil && e.http.CookieOrigin != "" {
			admit, err := billingauth.CookieAuthentication(e.http.CookieOrigin)
			if err != nil {
				return nil, err
			}
			for i := range routes {
				routes[i].Handler = admit(routes[i].Handler)
			}
		}
		e.routes = routes
	}
	return append([]routebundle.Route(nil), e.routes...), nil
}

// RoutesRequireRoot reports whether Routes is the standalone surface, whose
// issuer-anchored URLs must be mounted at the router's root.
func (e *Engine) RoutesRequireRoot() bool { return operator.Get(e.App) != nil }

func (e *Engine) buildRoutes() ([]routebundle.Route, error) {
	a := e.App
	if operator.Get(a) != nil {
		table, err := operator.StandaloneRoutes(a)
		if err != nil {
			return nil, err
		}
		if e.http != nil {
			extra, err := embedhttp.BuildCustomerRoutes(a, e.http.CustomerRoutes, a.Runtime.Auth)
			if err != nil {
				return nil, err
			}
			table.Entries = append(table.Entries, extra.Entries...)
		}
		router.AddMerchantSelectorRoutes(table, "", func(ctx context.Context, r *http.Request) (billingauth.Target, error) {
			return merchanttarget.Resolve(ctx, r, a.Runtime.Merchants, a.Runtime.ConfiguredMerchant(), "")
		})
		if err := embedhttp.ValidateRouteTable(table); err != nil {
			return nil, err
		}
		return routebundle.FromTable(table), nil
	}
	if e.http == nil {
		return nil, fmt.Errorf("openrails: HTTP is disabled; set Config.HTTP")
	}
	table, err := embedhttp.ConfiguredRoutes(a, e.http)
	if err != nil {
		return nil, err
	}
	for i := range table.Entries {
		table.Entries[i].Path = strings.TrimPrefix(table.Entries[i].Path, "/billing")
	}
	if err := embedhttp.ValidateRouteTable(table); err != nil {
		return nil, err
	}
	return routebundle.FromTable(table), nil
}
