package handlers

import (
	"net/http"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
)

// ListPublicProducts lists the products on sale, each with its current
// prices, as a buyer sees them. A product no live price sells is granted
// only and is not listed.
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
	out, err := svc.ListProducts(r.Request.Context(), billing.ProductListParams{PageRequest: page, Archived: &archived, ForSale: &forSale})
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
