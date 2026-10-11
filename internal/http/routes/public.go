package routes

import (
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
)

// A public route takes no credential, so the request itself names the
// merchant whose rows it reads: the public catalog by ?merchant=, a wallet's
// Solana Pay request by its order. That merchant replaces any pinned before
// the route, and opens nothing but the route's own answer.

// publicMerchant finds a public route's merchant, ahead of its connection.
func (e *Env) publicMerchant(route Route) []router.Middleware {
	switch route.Merchant {
	case MerchantQuery:
		return []router.Middleware{e.merchantByQuery(route)}
	case MerchantOrder:
		return []router.Middleware{e.merchantByOrder(route)}
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

// merchantByQuery pins the live merchant ?merchant= names by id: 400
// invalid_query unless it is one canonical id, 404 when no live merchant has
// it.
func (e *Env) merchantByQuery(route Route) router.Middleware {
	return func(next router.Handler) router.Handler {
		return func(r *httprequest.Request) {
			values := r.Request.URL.Query()["merchant"]
			var id billing.MerchantID
			if len(values) == 1 {
				id, _ = billing.ParseMerchantID(values[0])
			}
			if id.IsZero() || id.String() != values[0] {
				r.AbortAPIError(api.Coded(billing.CodeInvalidQuery, "merchant is one merchant id").WithParam("merchant"))
				return
			}
			row, err := e.directory().GetMerchantDirectoryByID(r.Request.Context(), id.UUID())
			switch {
			case errors.Is(err, pgx.ErrNoRows), err == nil && row.Status != string(merchants.StatusActive):
				r.AbortCode(billing.CodeResourceNotFound, "merchant not found")
			case err != nil:
				log.WithError(err).WithField("route", route.Key()).Error("openrails: merchant lookup failed")
				r.AbortCode(billing.CodeInternalError, "")
			default:
				pinPublic(r, id)
				next(r)
			}
		}
	}
}

// merchantByOrder pins the live merchant of the order {id} names: 404 for an
// id no one order of a live merchant has.
func (e *Env) merchantByOrder(route Route) router.Middleware {
	return func(next router.Handler) router.Handler {
		return func(r *httprequest.Request) {
			id, err := billing.ParseOrderID(strings.TrimSpace(r.Param("id")))
			if err != nil || id.IsZero() {
				r.AbortCode(billing.CodeResourceNotFound, "order not found")
				return
			}
			owners, err := e.directory().GetOrderMerchant(r.Request.Context(), id.UUID())
			switch {
			case err != nil:
				log.WithError(err).WithField("route", route.Key()).Error("openrails: order merchant lookup failed")
				r.AbortCode(billing.CodeInternalError, "")
			case len(owners) != 1:
				r.AbortCode(billing.CodeResourceNotFound, "order not found")
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
	r.Set("openrails.merchant_id", id)
}
