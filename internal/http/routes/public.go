package routes

import (
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/logx"
	"github.com/open-rails/openrails/internal/merchant"
)

// A public route takes no credential, so the request itself names the
// merchant whose rows it reads: a wallet's Solana Pay request by its invoice.
// That merchant replaces any pinned before the route, and opens nothing but
// the route's own answer.

// publicMerchant finds a public route's merchant, ahead of its connection.
func (e *Env) publicMerchant(route Route) []router.Middleware {
	if route.Merchant == MerchantInvoice {
		return []router.Middleware{e.merchantByInvoice(route)}
	}
	return nil
}

// directory reads the merchant directory, outside any merchant's connection.
func (e *Env) directory() *gen.Queries {
	var d *db.DB
	if e.Runtime != nil {
		d = e.Runtime.DB
	}
	return d.GenDirectory()
}

// merchantByInvoice pins the live merchant of the invoice {id} names: 404 for an
// id no one invoice of a live merchant has.
func (e *Env) merchantByInvoice(route Route) router.Middleware {
	return func(next router.Handler) router.Handler {
		return func(r *httprequest.Request) {
			id, err := billing.ParseInvoiceID(strings.TrimSpace(r.Param("id")))
			if err != nil || id.IsZero() {
				r.AbortCode(billing.CodeResourceNotFound, "invoice not found")
				return
			}
			owners, err := e.directory().GetInvoiceMerchant(r.Request.Context(), id.UUID())
			switch {
			case err != nil:
				r.Log.ErrorContext(r.Request.Context(), "openrails: invoice merchant lookup failed", logx.Err(err), "route", route.Key())
				r.AbortCode(billing.CodeInternalError, "")
			case len(owners) != 1:
				r.AbortCode(billing.CodeResourceNotFound, "invoice not found")
			default:
				pinPublic(r, billing.MerchantID(owners[0]))
				next(r)
			}
		}
	}
}

// pinPublic binds the merchant a public request named as the one it reads.
func pinPublic(r *httprequest.Request, id billing.MerchantID) {
	r.Request = r.Request.WithContext(merchant.WithID(r.Request.Context(), id))
}
