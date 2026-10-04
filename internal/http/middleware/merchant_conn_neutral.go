package middleware

import (
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
)

// MerchantDBConnMW is the framework-neutral analogue of MerchantDBConn (issue #282).
// It pins a merchant-scoped DB connection for the request, sets the
// `app.merchant_id` GUC, runs the rest of the chain, then releases the connection
// (resetting the GUC) on the way out. See db.WithMerchantConn for what the
// pin does and does not scope.
func MerchantDBConnMW(database *db.DB) router.Middleware {
	return func(next router.Handler) router.Handler {
		return func(r *request.Request) {
			if database == nil {
				next(r)
				return
			}
			ctx, release, err := database.WithMerchantConn(r.Request.Context())
			if err != nil {
				log.WithError(err).Error("merchant db connection setup failed")
				r.AbortCode(billing.CodeInternalError, "")
				return
			}
			defer release()
			r.Request = r.Request.WithContext(ctx)
			next(r)
		}
	}
}
