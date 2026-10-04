package handlers

import (
	"net/http"

	"github.com/open-rails/openrails/billing"
	httprequest "github.com/open-rails/openrails/internal/http/request"
)

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
