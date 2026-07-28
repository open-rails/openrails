package handlers

import (
	"net/http"
	"strings"

	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/pkg/api"
)

type catalogPaginationParams struct {
	Limit  int `form:"limit"`
	Offset int `form:"offset"`
}

type getProductsQuery struct {
	catalogPaginationParams
}

type getPricesQuery struct {
	catalogPaginationParams
	Currency string `form:"currency"`
	Product  string `form:"product"`
	Type     string `form:"type"`
}

func (q *catalogPaginationParams) setDefaults(defaultLimit int) {
	q.Limit = defaultLimit
	q.Offset = 0
}

func GetProducts(r *httprequest.Request) {
	req := &getProductsQuery{}
	req.setDefaults(20)
	if !r.BindQuery(req) {
		return
	}

	result, err := r.State.PublicSubscriptionService.GetProductsPaginated(
		r.Request.Context(),
		false,
		req.Limit,
		req.Offset,
	)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to retrieve products")
		return
	}

	productObjects := make([]api.ProductObject, len(result.Products))
	for i, p := range result.Products {
		productObjects[i] = ProductToAPI(p.Product, p.Prices)
	}

	r.SuccessJSON(api.NewList(productObjects, result.TotalItems, req.Limit, req.Offset))
}

func GetPrices(r *httprequest.Request) {
	req := &getPricesQuery{}
	req.setDefaults(20)
	if !r.BindQuery(req) {
		return
	}

	filter := catalog.PriceFilter{
		Currency: strings.ToLower(req.Currency),
		Type:     req.Type,
	}

	// The public surface never exposes archived prices, whatever the caller
	// asked for.
	archived := false
	filter.Archived = &archived

	if req.Product != "" {
		productID, err := api.ParseProductID(req.Product)
		if err != nil {
			r.ErrorJSON(http.StatusBadRequest, "Invalid product ID format")
			return
		}
		filter.ProductID = &productID
	}

	prices, totalItems, err := r.State.PriceService.ListPaginated(
		r.Request.Context(),
		filter,
		req.Limit,
		req.Offset,
	)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to retrieve prices")
		return
	}

	priceObjects := make([]api.PriceObject, len(prices))
	for i, p := range prices {
		priceObjects[i] = PriceToAPI(p)
	}

	r.SuccessJSON(api.NewList(priceObjects, totalItems, req.Limit, req.Offset))
}
