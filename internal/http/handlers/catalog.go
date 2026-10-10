package handlers

import (
	"net/http"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
)

// ListPublicProducts lists what a buyer may buy: the products on sale granting
// any ?entitlement= and named by ?keys= (one is required; there is no
// unfiltered listing), each with its live prices. A product no live price sells is
// granted only and is not listed.
func ListPublicProducts(r *httprequest.Request) {
	page, ok := r.Page()
	if !ok {
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	archived, forSale := false, true
	values := r.Request.URL.Query()
	if len(values["entitlement"]) == 0 && len(values["keys"]) == 0 {
		r.APIError(api.Coded(billing.CodeInvalidParam, "entitlement or keys is required").WithParam("entitlement"))
		return
	}
	out, err := svc.ListProducts(r.Request.Context(), billing.ProductListParams{PageRequest: page, Archived: &archived, ForSale: &forSale,
		Keys: values["keys"], Entitlements: values["entitlement"]})
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	if err := svc.HydratePrices(r.Request.Context(), out.Items); err != nil {
		writeCatalogError(r, err)
		return
	}
	for i := range out.Items {
		for j := range out.Items[i].Prices {
			out.Items[i].Prices[j] = models.PublicPrice(out.Items[i].Prices[j])
		}
	}
	r.JSON(http.StatusOK, out)
}
