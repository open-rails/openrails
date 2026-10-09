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

// GetPublicConfig returns what GET /v1/config serves a browser: the
// capabilities of the mount serving the request, the currency registry and
// the merchant's payment setup.
func (c *Client) GetPublicConfig(ctx context.Context, requestOptions ...RequestOption) (*billing.PublicConfig, error) {
	var out billing.PublicConfig
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/config", nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListCheckoutOptions lists the ways checkout can sell one price now, in
// routing order, each with the browser driver and public values that render
// it.
func (c *Client) ListCheckoutOptions(ctx context.Context, query billing.CheckoutOptionListParams, requestOptions ...RequestOption) (*billing.ListPage[billing.CheckoutOption], error) {
	hasKey := strings.TrimSpace(query.PriceKey) != ""
	switch {
	case !query.PriceID.IsZero() && hasKey:
		return nil, invalidErr("price_id and price_key are exclusive")
	case hasKey != (strings.TrimSpace(query.ProductKey) != ""):
		return nil, invalidErr("product_key and price_key must be supplied together")
	case query.PriceID.IsZero() && !hasKey:
		return nil, invalidErr("price_id or product_key and price_key is required")
	}
	values := url.Values{}
	if !query.PriceID.IsZero() {
		values.Set("price_id", query.PriceID.String())
	} else {
		values.Set("price_key", query.PriceKey)
		values.Set("product_key", query.ProductKey)
	}
	var out billing.ListPage[billing.CheckoutOption]
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/checkout-options?"+values.Encode(), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}
