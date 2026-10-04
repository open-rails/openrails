package handlers

import (
	"net/http"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
)

// PublicPriceListQuery filters the public price list.
type PublicPriceListQuery struct {
	ProductID billing.ProductID `form:"product_id"`
	Currency  string            `form:"currency"`
	AutoRenew *bool             `form:"auto_renew"`
}

// ListPublicProducts lists the products on sale, each with its current
// prices, as a buyer sees them.
func ListPublicProducts(r *httprequest.Request) {
	page, ok := r.Page()
	if !ok {
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	onSale := false
	out, err := svc.ListProducts(r.Request.Context(), billing.ProductListParams{PageRequest: page, Archived: &onSale})
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

// ListPublicPrices lists the prices on sale, as a buyer sees them.
func ListPublicPrices(r *httprequest.Request) {
	page, ok := r.Page()
	if !ok {
		return
	}
	var query PublicPriceListQuery
	if !r.BindQuery(&query) {
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	onSale := false
	out, err := svc.ListPrices(r.Request.Context(), billing.PriceListParams{PageRequest: page, ProductID: query.ProductID, Currency: query.Currency, AutoRenew: query.AutoRenew, Archived: &onSale})
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	for i := range out.Items {
		out.Items[i] = models.PublicPrice(out.Items[i])
	}
	r.JSON(http.StatusOK, out)
}
