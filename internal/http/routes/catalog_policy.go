package routes

import (
	"net/http"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/catalogpolicy"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/pkg/api"
)

// catalogPolicyRouter excludes disabled mutation capabilities from every
// catalog group, including nested resource routes.
type catalogPolicyRouter struct {
	router.Router
	cfg *config.Config
}

func withCatalogWritePolicy(rr router.Router, rt *app.Runtime) router.Router {
	var cfg *config.Config
	if rt != nil {
		cfg = rt.Config
	}
	return catalogPolicyRouter{Router: rr, cfg: cfg}
}

func (r catalogPolicyRouter) Handle(method, path string, handler router.Handler, mw ...router.Middleware) {
	if method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions {
		if !catalogpolicy.Enabled(r.cfg) {
			return
		}
		mw = append([]router.Middleware{catalogWriteGuardMW(r.cfg)}, mw...)
	}
	r.Router.Handle(method, path, handler, mw...)
}

func (r catalogPolicyRouter) Group(prefix string, mw ...router.Middleware) router.Router {
	return catalogPolicyRouter{Router: r.Router.Group(prefix, mw...), cfg: r.cfg}
}

func catalogWriteGuardMW(cfg *config.Config) router.Middleware {
	return func(next router.Handler) router.Handler {
		return func(r *httprequest.Request) {
			if err := catalogpolicy.Check(r.Request.Context(), cfg); err != nil {
				r.APIError(api.NewAPIError(http.StatusForbidden, api.ErrorTypeInvalidRequest, "catalog_updates_disabled", err.Error()))
				return
			}
			next(r)
		}
	}
}
