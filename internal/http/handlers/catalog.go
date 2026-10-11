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
// products on sale of the merchant ?merchant= names (the route's gate read
// it), named by ?keys= (1 to billing.MaxBatchItems), each with its live
// prices, in one page. It takes no other parameter: no entitlement filter,
// no listing. A product no live price sells is not on sale.
func ListPublicProducts(r *httprequest.Request) {
	query := r.Request.URL.Query()
	names := make([]string, 0, len(query))
	for name := range query {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if name != "keys" && name != "merchant" {
			r.APIError(api.Coded(billing.CodeInvalidQuery, "the public catalog takes only merchant and keys, not "+name).WithParam(name))
			return
		}
	}
	keys := query["keys"]
	if len(keys) == 0 || len(keys) > billing.MaxBatchItems || slices.Contains(keys, "") {
		r.APIError(api.Coded(billing.CodeInvalidQuery, fmt.Sprintf("keys names 1 to %d products", billing.MaxBatchItems)).WithParam("keys"))
		return
	}
	writePublicProducts(r, billing.ProductListParams{PageRequest: billing.PageRequest{Limit: billing.MaxBatchItems}, Keys: keys})
}

// writePublicProducts answers the products on sale params selects, each with its
// live prices as a buyer sees them.
func writePublicProducts(r *httprequest.Request, params billing.ProductListParams) {
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
