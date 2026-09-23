package handlers

import (
	"net/http"
	"strconv"
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

func ListOffersForEntitlement(r *httprequest.Request) {
	params := openrails.OfferListParams{Kind: openrails.OfferKind(r.Query("kind")), PreferredCurrency: r.Query("preferred_currency"), Cursor: r.Query("cursor")}
	if raw := r.Query("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil {
			r.ErrorJSON(http.StatusBadRequest, "invalid limit")
			return
		}
		params.Limit = value
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	result, err := svc.ListOffersForEntitlement(r.Request.Context(), r.Query("entitlement"), params)
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	r.SuccessJSON(result)
}
