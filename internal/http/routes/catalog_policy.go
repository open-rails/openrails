package routes

import (
	"net/http"

	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/catalogpolicy"
	"github.com/open-rails/openrails/internal/config"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
)

// catalogPolicyRouter excludes disabled mutation capabilities from every
// catalog group, including nested resource routes. The in-process surface
// registers them regardless: the guard admits its host principal only.
type catalogPolicyRouter struct {
	router.Router
	cfg       *config.Config
	inProcess bool
}

func withCatalogWritePolicy(rr router.Router, rt *app.Runtime, opts Options) router.Router {
	var cfg *config.Config
	if rt != nil {
		cfg = rt.Config
	}
	return catalogPolicyRouter{Router: rr, cfg: cfg, inProcess: opts.InProcess}
}

func (r catalogPolicyRouter) Handle(method, path string, handler router.Handler, mw ...router.Middleware) {
	if method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions {
		if !r.inProcess && !catalogpolicy.Enabled(r.cfg) {
			return
		}
		mw = append([]router.Middleware{catalogWriteGuardMW(r.cfg)}, mw...)
	}
	r.Router.Handle(method, path, handler, mw...)
}

func (r catalogPolicyRouter) Group(prefix string, mw ...router.Middleware) router.Router {
	return catalogPolicyRouter{Router: r.Router.Group(prefix, mw...), cfg: r.cfg, inProcess: r.inProcess}
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
