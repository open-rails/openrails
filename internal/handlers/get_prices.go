package handlers

import (
	"net/http"
	"strings"

	"github.com/doujins-org/ginapi/response"
	authpolicy "github.com/open-rails/openrails/internal/auth/policy"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/services"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/authprovider"
)

// GetPrices retrieves prices with optional filters.
// Follows Stripe's API pattern: https://docs.stripe.com/api/prices/list
//
// Query params:
//   - active: Only return active (true) or inactive (false) prices. Default: true.
//     Non-admins can only see active=true; any other value is silently ignored.
//   - currency: Only return prices for the given currency (e.g., "usd")
//   - product: Only return prices for the given product ID (with or without prod_ prefix)
//   - type: Only return prices of type "recurring" or "one_time"
//   - limit: Maximum number of items to return (default: 20, max: 100)
//   - offset: Number of items to skip (default: 0)
func GetPrices(r *httprequest.Request) {
	req := new(GetPricesRequest)
	req.SetDefaults()
	if !r.BindQuery(req.Query()) {
		return
	}

	// Build filter
	filter := services.PriceFilter{
		Currency: strings.ToLower(req.Currency),
		Type:     req.Type,
	}

	// Determine active filter
	// By default, only active prices are shown
	// Non-admins can only see active prices
	if req.Active == nil {
		// Default to active only
		active := true
		filter.Active = &active
	} else if *req.Active {
		filter.Active = req.Active
	} else {
		// Requesting inactive prices - only admins can do this
		if uc, ok := authprovider.UserContextFromGin(r.GinCtx); ok {
			if isAdmin, err := authpolicy.IsAdmin(r.Request.Context(), r.State.DB.GetDB(), uc.UserID); err == nil && isAdmin {
				filter.Active = req.Active
			} else {
				// Silently ignore for non-admins, show active only
				active := true
				filter.Active = &active
			}
		} else {
			active := true
			filter.Active = &active
		}
	}

	// Parse product ID if provided
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

	// Convert to API response
	priceObjects := make([]api.PriceObject, len(prices))
	for i, p := range prices {
		priceObjects[i] = PriceToAPI(p)
	}

	r.SuccessJSON(response.NewList(priceObjects, totalItems, req.Limit, req.Offset))
}
