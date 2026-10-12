package openrails

import (
	"context"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/billing"
)

// ListOrders is one page of the merchant's orders, newest first. Status
// complete with PriceID answers whether a customer bought a price.
func (c *Client) ListOrders(ctx context.Context, params billing.OrderListParams, requestOptions ...RequestOption) (*billing.ListPage[billing.Order], error) {
	q := pageValues(nil, params.PageRequest)
	overdue := ""
	if params.Overdue {
		overdue = "true"
	}
	setQuery(q, map[string]string{"customer_id": params.CustomerID.String(), "price_id": params.PriceID.String(), "subscription_id": params.SubscriptionID.String(),
		"status": string(params.Status), "origin": string(params.Origin), "overdue": overdue})
	if err := setIDs(q, params.IDs); err != nil {
		return nil, err
	}
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

// PreviewOrder prices lines for one customer, as a merchant order would
// freeze them: per-line refusals, the total and what can pay it.
func (c *Client) PreviewOrder(ctx context.Context, params billing.PreviewOrderParams, requestOptions ...RequestOption) (*billing.OrderPreview, error) {
	if params.CustomerID.IsZero() {
		return nil, invalidErr("customer_id is required")
	}
	var out billing.OrderPreview
	if err := c.do(ctx, http.MethodPost, "/v1/app/orders/preview", params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateOrder creates a merchant order: an unpaid order for one customer,
// which they pay on your site (POST /v1/me/orders/{id}/pay, or billing-ui's
// OrderCheckout). It never charges. A retry with the same IdempotencyKey
// answers the same order.
func (c *Client) CreateOrder(ctx context.Context, params billing.CreateOrderParams, requestOptions ...RequestOption) (*billing.Order, error) {
	if params.CustomerID.IsZero() {
		return nil, invalidErr("customer_id is required")
	}
	if strings.TrimSpace(params.IdempotencyKey) == "" {
		return nil, invalidErr("Idempotency-Key required")
	}
	var out billing.Order
	if err := c.doWithHeaders(ctx, http.MethodPost, "/v1/app/orders", params, &out, http.Header{"Idempotency-Key": {params.IdempotencyKey}}, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// CancelOrder withdraws an open order you created.
func (c *Client) CancelOrder(ctx context.Context, id billing.OrderID, requestOptions ...RequestOption) (*billing.Order, error) {
	order, err := requireTypedID("order_id", id)
	if err != nil {
		return nil, err
	}
	var out billing.Order
	if err := c.do(ctx, http.MethodPost, "/v1/app/orders/"+order+"/cancel", nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// adminOrderPost posts to one of an order's admin actions.
func (c *Client) adminOrderPost(ctx context.Context, id billing.OrderID, action string, body any, headers http.Header, requestOptions []RequestOption) (*billing.Order, error) {
	order, err := requireTypedID("order_id", id)
	if err != nil {
		return nil, err
	}
	var out billing.Order
	if err := c.doWithHeaders(ctx, http.MethodPost, "/v1/admin/orders/"+order+"/"+action, body, &out, headers, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// WaiveOrderBalance forgives the unpaid balance of a usage order. Existing payments remain recorded.
// Repeating it changes nothing.
func (c *Client) WaiveOrderBalance(ctx context.Context, id billing.OrderID, requestOptions ...RequestOption) (*billing.Order, error) {
	return c.adminOrderPost(ctx, id, "waive-balance", nil, nil, requestOptions)
}

// MarkOrderUncollectible writes an open usage order off; collecting it
// again reopens it. Repeating it changes nothing.
func (c *Client) MarkOrderUncollectible(ctx context.Context, id billing.OrderID, requestOptions ...RequestOption) (*billing.Order, error) {
	return c.adminOrderPost(ctx, id, "mark-uncollectible", nil, nil, requestOptions)
}
