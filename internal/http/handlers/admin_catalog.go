package handlers

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	billingservice "github.com/open-rails/openrails/internal/service"
)

// Catalog handlers serve both the merchant's catalog routes
// (/v1/merchant/catalog) and a creator's own (/v1/catalog); the owner scope on
// the request selects which catalog they act on.

func newAdminBillingService(r *httprequest.Request) (*billingservice.Service, bool) {
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorCode(billing.CodeInternalError, "billing service unavailable")
		return nil, false
	}
	return svc, true
}

func writeCatalogError(r *httprequest.Request, err error) {
	writeRefusal(r, err, "catalog operation failed")
}

// ProductListQuery filters ListProducts.
type ProductListQuery struct {
	Archived  *bool  `form:"archived"`
	TierGroup string `form:"tier_group"`
}

// PriceListQuery filters ListPrices.
type PriceListQuery struct {
	ProductID billing.ProductID `form:"product_id"`
	Currency  string            `form:"currency"`
	Recurring *bool             `form:"recurring"`
	Archived  *bool             `form:"archived"`
}

// PriceQuery is GetPrice's query.
type PriceQuery struct {
	Verify bool `form:"verify"`
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

// EnsureProduct creates the product under the path's key unless it exists;
// an existing product is returned unchanged.
func EnsureProduct(r *httprequest.Request) {
	var params billing.CreateProductParams
	if !r.BindJSON(&params) {
		return
	}
	key := r.Param("product_key")
	if params.Key != "" && params.Key != key {
		r.ErrorCode(billing.CodeInvalidParam, "product key in path and body must match")
		return
	}
	params.Key = key
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	out, err := svc.EnsureProduct(r.Request.Context(), params)
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	writeProduct(r, svc, http.StatusOK, out)
}

func ListProducts(r *httprequest.Request) {
	page, ok := r.Page()
	if !ok {
		return
	}
	var query ProductListQuery
	if !r.BindQuery(&query) {
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	out, err := svc.ListProducts(r.Request.Context(), billing.ProductListParams{PageRequest: page, Archived: query.Archived, TierGroup: strings.TrimSpace(query.TierGroup)})
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

func GetProductByKey(r *httprequest.Request) {
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	out, err := svc.GetProductByKey(r.Request.Context(), r.Param("product_key"))
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

func ListPrices(r *httprequest.Request) {
	page, ok := r.Page()
	if !ok {
		return
	}
	var query PriceListQuery
	if !r.BindQuery(&query) {
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	out, err := svc.ListPrices(r.Request.Context(), billing.PriceListParams{PageRequest: page, ProductID: query.ProductID,
		Currency: query.Currency, Recurring: query.Recurring, Archived: query.Archived})
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	r.JSON(http.StatusOK, out)
}

// GetPrice reads a price; ?verify=true also reads each linked PSP's copy and
// reports its drift.
func GetPrice(r *httprequest.Request) {
	id, ok := priceIDParam(r)
	if !ok {
		return
	}
	var query PriceQuery
	if !r.BindQuery(&query) {
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
	if query.Verify {
		if states, vErr := svc.VerifyPriceSync(r.Request.Context(), id.UUID()); vErr == nil && len(states) > 0 {
			out.PSPs = states
		}
	}
	r.JSON(http.StatusOK, out)
}

// GetPriceByKey reads the price a key currently names.
func GetPriceByKey(r *httprequest.Request) {
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	out, err := svc.GetPriceByKey(r.Request.Context(), r.Param("product_key"), r.Param("key"))
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	r.JSON(http.StatusOK, out)
}

// ListPriceKeyHistory lists when a key moved to which price, most recent
// first.
func ListPriceKeyHistory(r *httprequest.Request) {
	page, ok := r.Page()
	if !ok {
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	out, err := svc.ListPriceKeyHistory(r.Request.Context(), r.Param("product_key"), r.Param("key"), page)
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
		return billing.PriceID{}, false
	}
	return id, true
}

// -- Helpers -----------------------------------------------------------------

// invalidParam refuses one malformed request parameter.
func invalidParam(param, message string) *api.APIError {
	return api.Coded(billing.CodeInvalidParam, message).WithParam(param)
}

// PaginatedResponse is the offset page some routes outside the catalog still
// answer.
type PaginatedResponse[T any] struct {
	Items  []T   `json:"items"`
	Total  int64 `json:"total"`
	Limit  int   `json:"limit"`
	Offset int   `json:"offset"`
}

func parseIntDefault(s string, def int) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return def
	}
	return n
}

func parseBool(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "1", "yes", "y", "on":
		return true
	default:
		return false
	}
}
