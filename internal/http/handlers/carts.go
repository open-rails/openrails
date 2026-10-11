package handlers

import (
	"net/http"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	httprequest "github.com/open-rails/openrails/internal/http/request"
)

// GetMyCart handles GET /v1/me/cart.
func GetMyCart(r *httprequest.Request) {
	actor := orderActor(r, false)
	svc := orderService(r)
	if actor == nil || svc == nil {
		return
	}
	cart, err := svc.GetCart(r.Request.Context(), *actor)
	if err != nil {
		writeOrderError(r, err)
		return
	}
	r.SetHeader("Cache-Control", "no-store")
	r.JSON(http.StatusOK, cart)
}

// SetMyCartLine handles PUT /v1/me/cart/lines/{price_id}.
func SetMyCartLine(r *httprequest.Request) {
	var body billing.SetCartLineParams
	if !r.BindJSON(&body) {
		return
	}
	price, err := billing.ParsePriceID(r.Param("price_id"))
	if err != nil || price.IsZero() {
		r.APIError(api.Coded(billing.CodeResourceNotFound, "price not found"))
		return
	}
	actor := orderActor(r, false)
	svc := orderService(r)
	if actor == nil || svc == nil {
		return
	}
	cart, err := svc.SetCartLine(r.Request.Context(), *actor, price, body)
	if err != nil {
		writeOrderError(r, err)
		return
	}
	r.SetHeader("Cache-Control", "no-store")
	r.JSON(http.StatusOK, cart)
}

// ClearMyCart handles DELETE /v1/me/cart.
func ClearMyCart(r *httprequest.Request) {
	actor := orderActor(r, false)
	svc := orderService(r)
	if actor == nil || svc == nil {
		return
	}
	if err := svc.ClearCart(r.Request.Context(), *actor); err != nil {
		writeOrderError(r, err)
		return
	}
	r.NoContent()
}
