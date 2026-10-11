package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/handlers"
)

// cartsRoutes are the customer's cart (#1168): lines kept across the
// merchant's sites and priced live; an order buys it with cart: true.
var cartsRoutes = []Route{
	{Method: GET, Path: "/v1/me/cart", Group: Customer, Auth: AuthCustomer,
		Responses: []Reply{{200, billing.Cart{}}}, Errors: codes("service_unavailable"), Handler: h(handlers.GetMyCart)},
	{Method: PUT, Path: "/v1/me/cart/lines/{price_id}", Group: Customer, Auth: AuthCustomer,
		Request: billing.SetCartLineParams{}, Responses: []Reply{{200, billing.Cart{}}}, Errors: codes("invalid_param", "resource_not_found", "service_unavailable"), Handler: h(handlers.SetMyCartLine)},
	{Method: DELETE, Path: "/v1/me/cart", Group: Customer, Auth: AuthCustomer,
		Responses: []Reply{{204, nil}}, Errors: codes("service_unavailable"), Handler: h(handlers.ClearMyCart)},
}
