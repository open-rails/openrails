package handlers

import (
	"net/http"
	"strings"

	"github.com/doujins-org/ginapi/response"
	authpolicy "github.com/open-rails/openrails/internal/auth/policy"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/authprovider"
)

type catalogPaginationParams struct {
	Limit  int `form:"limit"`
	Offset int `form:"offset"`
}

type getProductsQuery struct {
	catalogPaginationParams
	Active *bool `form:"active"`
}

type getPricesQuery struct {
	catalogPaginationParams
	Active   *bool  `form:"active"`
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

	includeInactive := false
	if req.Active != nil && !*req.Active {
		if uc, ok := authprovider.UserContextFromGin(r.GinCtx); ok {
			if isAdmin, err := authpolicy.IsAdmin(r.Request.Context(), r.State.DB.GetDB(), uc.UserID); err == nil && isAdmin {
				includeInactive = true
			}
		}
	}

	result, err := r.State.PublicSubscriptionService.GetProductsPaginated(
		r.Request.Context(),
		includeInactive,
		req.Limit,
		req.Offset,
	)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, err.Error())
		return
	}

	productObjects := make([]api.ProductObject, len(result.Products))
	for i, p := range result.Products {
		productObjects[i] = ProductToAPI(p.Product, p.Prices)
	}

	r.SuccessJSON(response.NewList(productObjects, result.TotalItems, req.Limit, req.Offset))
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

	if req.Active == nil {
		active := true
		filter.Active = &active
	} else if *req.Active {
		filter.Active = req.Active
	} else {
		if uc, ok := authprovider.UserContextFromGin(r.GinCtx); ok {
			if isAdmin, err := authpolicy.IsAdmin(r.Request.Context(), r.State.DB.GetDB(), uc.UserID); err == nil && isAdmin {
				filter.Active = req.Active
			} else {
				active := true
				filter.Active = &active
			}
		} else {
			active := true
			filter.Active = &active
		}
	}

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
		r.ErrorJSON(http.StatusInternalServerError, err.Error())
		return
	}

	priceObjects := make([]api.PriceObject, len(prices))
	for i, p := range prices {
		priceObjects[i] = PriceToAPI(p)
	}

	r.SuccessJSON(response.NewList(priceObjects, totalItems, req.Limit, req.Offset))
}
