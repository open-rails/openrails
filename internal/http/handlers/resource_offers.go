package handlers

import (
	"time"

	"github.com/open-rails/openrails"
	httprequest "github.com/open-rails/openrails/internal/http/request"
)

func ServiceCheckEntitlements(r *httprequest.Request) {
	customer, ok := commerceCustomer(r, customerIDParam(r.Param("user_id")))
	if !ok {
		return
	}
	var body struct {
		Entitlements []string  `json:"entitlements"`
		At           time.Time `json:"at"`
	}
	if !r.BindJSON(&body) {
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	result, err := svc.CheckEntitlements(r.Request.Context(), customer.String(), body.Entitlements, body.At)
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	r.SuccessJSON(result)
}

func ListOffersForEntitlements(r *httprequest.Request) {
	var body openrails.OfferLookupRequest
	if !r.BindJSON(&body) {
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	result, err := svc.ListOffersForEntitlements(r.Request.Context(), body.Entitlements, openrails.OfferListParams{Kind: body.Kind, PreferredCurrency: body.PreferredCurrency, Limit: body.PageSize, Cursors: body.Cursors})
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	r.SuccessJSON(result)
}
