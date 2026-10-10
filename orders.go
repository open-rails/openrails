package openrails

import (
	"context"
	"net/http"

	"github.com/open-rails/openrails/billing"
)

// ListOrders is one page of the merchant's orders, newest first. Status
// paid with PriceID answers whether a customer bought a price.
func (c *Client) ListOrders(ctx context.Context, params billing.OrderListParams, requestOptions ...RequestOption) (*billing.ListPage[billing.Order], error) {
	q := pageValues(nil, params.PageRequest)
	setQuery(q, map[string]string{"customer_id": params.CustomerID.String(), "price_id": params.PriceID.String(), "status": string(params.Status)})
	var out billing.ListPage[billing.Order]
	if err := c.do(ctx, http.MethodGet, "/v1/admin/orders?"+q.Encode(), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetOrder reads one order. Staff never pay, so it carries no next_action.
func (c *Client) GetOrder(ctx context.Context, id billing.OrderID, requestOptions ...RequestOption) (*billing.Order, error) {
	order, err := requireTypedID("order_id", id)
	if err != nil {
		return nil, err
	}
	var out billing.Order
	if err := c.do(ctx, http.MethodGet, "/v1/admin/orders/"+order, nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}
