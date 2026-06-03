package middleware

import (
	"net/http"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
)

// TenantDBConnMW is the framework-neutral analogue of TenantDBConn (issue #282).
// It pins a tenant-scoped DB connection for the request, sets the
// `app.tenant_id` GUC, runs the rest of the chain, then releases the connection
// (resetting the GUC) on the way out. See TenantDBConn for the full RLS
// rationale; the semantics are identical, only the transport differs.
func TenantDBConnMW(database *db.DB) router.Middleware {
	return func(next router.Handler) router.Handler {
		return func(r *request.Request) {
			if database == nil {
				next(r)
				return
			}
			ctx, release, err := database.WithTenantConn(r.Request.Context())
			if err != nil {
				log.WithError(err).Error("tenant db connection setup failed")
				r.AbortJSON(http.StatusInternalServerError, "internal_error")
				return
			}
			defer release()
			r.Request = r.Request.WithContext(ctx)
			next(r)
		}
	}
}
