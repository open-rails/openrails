package handlers

import (
	"net/http"

	"github.com/doujins-org/ginapi/response"
	authpolicy "github.com/open-rails/openrails/internal/auth/policy"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/authprovider"
)

// GetProducts retrieves products and prices for subscription.
// Follows Stripe's API pattern: https://docs.stripe.com/api/products/list
//
// Query params:
//   - active: Only return active (true) or inactive (false) products. Default: true.
//     Non-admins can only see active=true; any other value is silently ignored.
//   - limit: Maximum items to return (default: 20, max: 100)
//   - offset: Number of items to skip (default: 0)
func GetProducts(r *httprequest.Request) {
	req := new(GetProductsRequest)
	req.SetDefaults()
	if !r.BindQuery(req.Query()) {
		return
	}

	// Determine whether to include inactive products
	includeInactive := false
	if req.Active != nil && !*req.Active {
		// Only admins can view inactive products
		if uc, ok := authprovider.UserContextFromGin(r.GinCtx); ok {
			if isAdmin, err := authpolicy.IsAdmin(r.Request.Context(), r.State.DB.GetDB(), uc.UserID); err == nil && isAdmin {
				includeInactive = true
			}
		}
		// Non-admins requesting active=false are silently shown active products only
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

	// Convert to API objects
	productObjects := make([]api.ProductObject, len(result.Products))
	for i, p := range result.Products {
		productObjects[i] = ProductToAPI(p.Product, p.Prices)
	}

	listResp := response.NewList(productObjects, result.TotalItems, req.Limit, req.Offset)
	r.SuccessJSON(listResp)
}
