package handlers

import (
	"net/http"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	billingservice "github.com/open-rails/openrails/internal/service"
)

// Catalog handlers serve both the merchant's catalog routes
// (/v1/catalog) and a creator's own (/v1/catalog); the owner scope on
// the request selects which catalog they act on.

func newAdminBillingService(r *httprequest.Request) (*billingservice.Service, bool) {
	svc, err := billingservice.New(r.Log, r.State)
	if err != nil {
		r.ErrorCode(billing.CodeInternalError, "billing service unavailable")
		return nil, false
	}
	return svc, true
}

func writeCatalogError(r *httprequest.Request, err error) {
	writeRefusal(r, err, "catalog operation failed")
}

// -- Products ----------------------------------------------------------------

func CreateProduct(r *httprequest.Request) {
	var params billing.CreateProductParams
	if !r.BindJSON(&params) {
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	out, err := svc.CreateProduct(r.Request.Context(), params)
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	writeProduct(r, svc, http.StatusCreated, out)
}

func ListProducts(r *httprequest.Request) {
	var params billing.ProductListParams
	if !bindList(r, &params, &params.PageRequest) {
		return
	}
	var ok bool
	if params.IDs, ok = listIDs(r, params.IDs); !ok {
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	params.TierGroup = strings.TrimSpace(params.TierGroup)
	out, err := svc.ListProducts(r.Request.Context(), params)
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	if err := svc.HydratePrices(r.Request.Context(), out.Items); err != nil {
		writeCatalogError(r, err)
		return
	}
	r.JSON(http.StatusOK, out)
}

func GetProduct(r *httprequest.Request) {
	id, ok := productIDParam(r)
	if !ok {
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	out, err := svc.GetProduct(r.Request.Context(), id)
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	writeProduct(r, svc, http.StatusOK, out)
}

func UpdateProduct(r *httprequest.Request) {
	id, ok := productIDParam(r)
	if !ok {
		return
	}
	var params billing.UpdateProductParams
	if !r.BindJSON(&params) {
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	out, err := svc.UpdateProduct(r.Request.Context(), id, params)
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	writeProduct(r, svc, http.StatusOK, out)
}

// DeleteProduct deletes a product nothing ever referred to, with its prices;
// one a record refers to is refused with product_in_use.
func DeleteProduct(r *httprequest.Request) {
	id, ok := productIDParam(r)
	if !ok {
		return
	}
	var params billing.DeleteProductParams
	if !r.BindQuery(&params) {
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	if err := svc.DeleteProduct(r.Request.Context(), id, params); err != nil {
		writeCatalogError(r, err)
		return
	}
	r.NoContent()
}

func productIDParam(r *httprequest.Request) (billing.ProductID, bool) {
	id, err := billing.ParseProductID(r.Param("id"))
	if err != nil || id.IsZero() {
		r.APIError(invalidParam("id", "invalid product id"))
		return billing.ProductID{}, false
	}
	return id, true
}

// writeProduct answers a product with its current prices.
func writeProduct(r *httprequest.Request, svc *billingservice.Service, status int, product *billing.Product) {
	products := []billing.Product{*product}
	if err := svc.HydratePrices(r.Request.Context(), products); err != nil {
		writeCatalogError(r, err)
		return
	}
	r.JSON(status, products[0])
}

// -- Prices ------------------------------------------------------------------

func CreatePrice(r *httprequest.Request) {
	var params billing.CreatePriceParams
	if !r.BindJSON(&params) {
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	out, err := svc.CreatePrice(r.Request.Context(), params)
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	r.JSON(http.StatusCreated, out)
}

// CreateProviderPrice links a price a provider owns and runs to a product,
// with the terms read from its provider.
func CreateProviderPrice(r *httprequest.Request) {
	var params billing.CreateProviderPriceParams
	if !r.BindJSON(&params) {
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	out, err := svc.CreateProviderPrice(r.Request.Context(), params)
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	r.JSON(http.StatusCreated, out)
}

func ListPrices(r *httprequest.Request) {
	var params billing.PriceListParams
	if !bindList(r, &params, &params.PageRequest) {
		return
	}
	var ok bool
	if params.IDs, ok = listIDs(r, params.IDs); !ok {
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	out, err := svc.ListPrices(r.Request.Context(), params)
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	r.JSON(http.StatusOK, out)
}

// GetPrice reads a price.
func GetPrice(r *httprequest.Request) {
	id, ok := priceIDParam(r)
	if !ok {
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	out, err := svc.GetPrice(r.Request.Context(), id)
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	r.JSON(http.StatusOK, out)
}

// VerifyPrice reads a price and a provider price's terms at its provider,
// reporting their drift.
func VerifyPrice(r *httprequest.Request) {
	id, ok := priceIDParam(r)
	if !ok {
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	out, err := svc.VerifyPublicPriceSync(r.Request.Context(), id)
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	r.JSON(http.StatusOK, out)
}

// ListPriceHistory lists when the price's key moved to which price, most
// recent first.
func ListPriceHistory(r *httprequest.Request) {
	id, ok := priceIDParam(r)
	if !ok {
		return
	}
	page, ok := r.Page()
	if !ok {
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	out, err := svc.ListPriceHistory(r.Request.Context(), id, page)
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	r.JSON(http.StatusOK, out)
}

func UpdatePrice(r *httprequest.Request) {
	id, ok := priceIDParam(r)
	if !ok {
		return
	}
	var params billing.UpdatePriceParams
	if !r.BindJSON(&params) {
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	out, err := svc.UpdatePrice(r.Request.Context(), id, params)
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	r.JSON(http.StatusOK, out)
}

func priceIDParam(r *httprequest.Request) (billing.PriceID, bool) {
	id, err := billing.ParsePriceID(r.Param("id"))
	if err != nil || id.IsZero() {
		r.APIError(invalidParam("id", "invalid price id"))
		return billing.PriceID(""), false
	}
	return id, true
}

// -- Helpers -----------------------------------------------------------------

// invalidParam refuses one malformed request parameter.
func invalidParam(param, message string) *api.APIError {
	return api.Coded(billing.CodeInvalidParam, message).WithParam(param)
}
