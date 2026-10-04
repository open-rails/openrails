package routes

import (
	"net/http"

	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/catalogpolicy"
	"github.com/open-rails/openrails/internal/config"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
)

// catalogWriteGuardMW refuses a catalog write the deployment does not allow.
// The in-process surface mounts the writes regardless: the guard admits its
// host principal only.
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
