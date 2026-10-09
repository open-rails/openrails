package routes

import (
	"net/http"

	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/catalogpolicy"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
)

// catalogWriteGuardMW refuses a catalog write the deployment does not allow.
// The in-process surface mounts the writes regardless: the guard admits its
// host principal only, unless a mount published catalog edits.
func catalogWriteGuardMW(rt *app.Runtime) router.Middleware {
	var exposure *catalogpolicy.Exposure
	if rt != nil {
		exposure = rt.CatalogEdits
	}
	return func(next router.Handler) router.Handler {
		return func(r *httprequest.Request) {
			if err := catalogpolicy.Check(r.Request.Context(), exposure); err != nil {
				r.APIError(api.NewAPIError(http.StatusForbidden, api.ErrorTypeInvalidRequest, "catalog_updates_disabled", err.Error()))
				return
			}
			next(r)
		}
	}
}
