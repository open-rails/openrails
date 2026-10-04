package handlers

import (
	"net/http"
	"time"

	"github.com/open-rails/openrails/billing"
	httprequest "github.com/open-rails/openrails/internal/http/request"
)

func ServiceCheckEntitlements(r *httprequest.Request) {
	customer, ok := commerceCustomer(r, customerIDParam(r.Param("customer_id")))
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

// ListOffers looks up the live offers that grant each requested
// entitlement: one page per entitlement.
func ListOffers(r *httprequest.Request) {
	var params billing.OfferListParams
	if !r.BindJSON(&params) {
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	out, err := svc.ListOffers(r.Request.Context(), params)
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	r.JSON(http.StatusOK, out)
}
