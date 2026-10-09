package openrails

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/open-rails/openrails/billing"
)

// CreateCheckoutSession hands a customer a purchase with the merchant's
// credential: one price, paid on the payment page. Usually the customer
// creates their own with their own credential (POST /v1/me/checkout-sessions,
// billing-ui). The returned ID reads and pays the session with no other
// credential (billing-ui's checkoutSource), so it goes to that customer's
// browser only; paying with a saved card needs the customer's own proof.
func (c *Client) CreateCheckoutSession(ctx context.Context, request billing.CreateCheckoutSessionParams, requestOptions ...RequestOption) (*billing.CheckoutSessionLink, error) {
	if _, err := requireTypedID("customer.id", request.Customer.ID); err != nil {
		return nil, err
	}
	if request.PriceID.IsZero() == (strings.TrimSpace(request.PriceKey) == "") {
		return nil, invalidErr("exactly one of price_id or price_key is required")
	}
	var out billing.CheckoutSessionLink
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/checkout-sessions", request, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetCheckoutConfig returns the merchant's checkout configuration: its armed
// PSPs and their public values, and with a price, the ways checkout can sell
// it.
func (c *Client) GetCheckoutConfig(ctx context.Context, query billing.GetCheckoutConfigParams, requestOptions ...RequestOption) (*billing.CheckoutConfig, error) {
	if !query.PriceID.IsZero() && strings.TrimSpace(query.PriceKey) != "" {
		return nil, invalidErr("price_id and price_key are exclusive")
	}
	if (strings.TrimSpace(query.PriceKey) != "") != (strings.TrimSpace(query.ProductKey) != "") {
		return nil, invalidErr("product_key and price_key must be supplied together")
	}
	values := url.Values{}
	if !query.PriceID.IsZero() {
		values.Set("price_id", query.PriceID.String())
	}
	if strings.TrimSpace(query.PriceKey) != "" {
		values.Set("price_key", query.PriceKey)
		values.Set("product_key", query.ProductKey)
	}
	path := "/v1/merchant/checkout-config"
	if len(values) > 0 {
		path += "?" + values.Encode()
	}
	var out billing.CheckoutConfig
	if err := c.do(ctx, http.MethodGet, path, nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}
