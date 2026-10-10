package handlers

import (
	"fmt"
	"net/http"
	"slices"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
)

// ListPublicProducts is the public catalog, a lookup by product key: the
// products on sale named by ?keys= (1 to billing.MaxBatchItems), each with its
// live prices, in one page. It takes no other parameter: no entitlement
// filter, no listing. A product no live price sells is not on sale.
func ListPublicProducts(r *httprequest.Request) {
	query := r.Request.URL.Query()
	names := make([]string, 0, len(query))
	for name := range query {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if name != "keys" {
			r.APIError(api.Coded(billing.CodeInvalidQuery, "the public catalog takes only keys, not "+name).WithParam(name))
			return
		}
	}
	keys := query["keys"]
	if len(keys) == 0 || len(keys) > billing.MaxBatchItems || slices.Contains(keys, "") {
		r.APIError(api.Coded(billing.CodeInvalidQuery, fmt.Sprintf("keys names 1 to %d products", billing.MaxBatchItems)).WithParam("keys"))
		return
	}
	writeOffers(r, billing.ProductListParams{PageRequest: billing.PageRequest{Limit: billing.MaxBatchItems}, Keys: keys})
}

// AppListOffers is the host backend's read of what a customer may buy: the
// products on sale granting any ?entitlement= and named by ?keys= (one is
// required, each at most billing.MaxBatchItems), each with its live prices.
func AppListOffers(r *httprequest.Request) {
	page, ok := r.Page()
	if !ok {
		return
	}
	query := r.Request.URL.Query()
	keys, entitlements := query["keys"], query["entitlement"]
	if len(keys) == 0 && len(entitlements) == 0 {
		r.APIError(api.Coded(billing.CodeInvalidQuery, "entitlement or keys is required").WithParam("entitlement"))
		return
	}
	writeOffers(r, billing.ProductListParams{PageRequest: page, Keys: keys, Entitlements: entitlements})
}

// writeOffers answers the products on sale params selects, each with its
// live prices as a buyer sees them.
func writeOffers(r *httprequest.Request, params billing.ProductListParams) {
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	archived, forSale := false, true
	params.Archived, params.ForSale = &archived, &forSale
	out, err := svc.ListProducts(r.Request.Context(), params)
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
