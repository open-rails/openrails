package middleware

import (
	"errors"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchanttarget"
	"github.com/open-rails/openrails/internal/modules/checkoutsession"
)

// CheckoutSessionMerchant resolves the native capability before a tenant DB
// connection is pinned. Request selectors can restrict its book, never replace it.
func CheckoutSessionMerchant(rt *app.Runtime) router.Middleware {
	return func(next router.Handler) router.Handler {
		return func(r *request.Request) {
			r.SetHeader("Cache-Control", "no-store")
			if !checkoutsession.ValidID(r.Param("id")) {
				r.AbortCode(checkoutsession.ErrNotFound.Code, "")
				return
			}
			if rt == nil || rt.CheckoutSessions == nil {
				r.AbortCode(billing.CodeServiceUnavailable, "")
				return
			}
			ctx := r.Request.Context()
			mid, err := rt.CheckoutSessions.Merchant(ctx, r.Param("id"), r.Clock.Now())
			if err != nil {
				if errors.Is(err, checkoutsession.ErrNotFound) {
					r.AbortCode(checkoutsession.ErrNotFound.Code, "")
				} else {
					log.WithError(err).Error("checkout capability lookup unavailable")
					r.AbortCode(billing.CodeServiceUnavailable, "")
				}
				return
			}
			// A configured deployment, resolved Host, or earlier authorized selector
			// cannot be widened by handing it a capability belonging to another book.
			pinned, _ := merchant.FromContext(ctx)
			host, _ := merchant.HostMerchant(ctx)
			target, _ := merchanttarget.FromContext(ctx)
			for _, bound := range []billing.MerchantID{rt.ConfiguredMerchant(), pinned, host, target.MerchantID} {
				if !bound.IsZero() && bound != mid {
					r.AbortCode(checkoutsession.ErrNotFound.Code, "")
					return
				}
			}
			_, present, err := merchant.ParseSelector(r.Request.Header)
			if err == nil && present {
				_, err = merchanttarget.Resolve(ctx, r.Request, rt.Merchants, mid, "")
			}
			if err != nil {
				var refusal billingauth.GateError
				if errors.As(err, &refusal) && refusal.Code == billing.CodeMerchantDirectoryUnavailable {
					r.AbortCode(billing.CodeServiceUnavailable, "")
				} else {
					r.AbortCode(checkoutsession.ErrNotFound.Code, "")
				}
				return
			}
			r.Request = r.Request.WithContext(merchant.WithID(r.Request.Context(), mid))
			next(r)
		}
	}
}
