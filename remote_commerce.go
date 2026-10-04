package openrails

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/open-rails/openrails/billing"
)

// CreateCheckoutSession hands a purchase to a customer: one price, paid on the
// payment page. The returned ID reads and pays the session with no other
// credential (billing-ui's checkoutSource), so it goes to that customer's
// browser only.
func (c *Client) CreateCheckoutSession(ctx context.Context, request billing.CreateCheckoutSessionRequest, requestOptions ...RequestOption) (*billing.CheckoutSessionLink, error) {
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

// CreateCheckoutAttempt charges one price for a customer now, relaying that
// customer's pay action; a recurring price is enrolled. Retries with the same
// IdempotencyKey never charge twice. A step the buyer must take (a redirect, a
// Solana Pay link) is the attempt's NextAction.
func (c *Client) CreateCheckoutAttempt(ctx context.Context, request billing.CreateCheckoutAttemptRequest, requestOptions ...RequestOption) (*billing.CheckoutAttempt, error) {
	if _, err := requireTypedID("customer.id", request.Customer.ID); err != nil {
		return nil, err
	}
	if request.PriceID.IsZero() == (strings.TrimSpace(request.PriceKey) == "") {
		return nil, invalidErr("exactly one of price_id or price_key is required")
	}
	if strings.TrimSpace(request.IdempotencyKey) == "" {
		return nil, invalidErr("IdempotencyKey is required")
	}
	var out billing.CheckoutAttempt
	if err := c.doWithHeaders(ctx, http.MethodPost, "/v1/merchant/checkout-attempts", request, &out, http.Header{"Idempotency-Key": {request.IdempotencyKey}}, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetCheckoutAttempt reads one checkout attempt.
func (c *Client) GetCheckoutAttempt(ctx context.Context, id billing.CheckoutAttemptID, requestOptions ...RequestOption) (*billing.CheckoutAttempt, error) {
	attempt, err := requireTypedID("id", id)
	if err != nil {
		return nil, err
	}
	var out billing.CheckoutAttempt
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/checkout-attempts/"+attempt, nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// ConfirmCheckoutAttempt completes a Solana attempt with the signature of the
// transaction the buyer's wallet signed (next action
// solana_sign_transactions).
func (c *Client) ConfirmCheckoutAttempt(ctx context.Context, id billing.CheckoutAttemptID, request billing.ConfirmCheckoutAttemptRequest, requestOptions ...RequestOption) (*billing.CheckoutAttempt, error) {
	attempt, err := requireTypedID("id", id)
	if err != nil {
		return nil, err
	}
	var out billing.CheckoutAttempt
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/checkout-attempts/"+attempt+"/confirm", request, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetCheckoutConfig returns the merchant's checkout configuration: its armed
// PSPs and their public values, and with a price, the ways checkout can sell
// it.
func (c *Client) GetCheckoutConfig(ctx context.Context, query billing.CheckoutConfigQuery, requestOptions ...RequestOption) (*billing.CheckoutConfig, error) {
	if !query.PriceID.IsZero() && strings.TrimSpace(query.PriceKey) != "" {
		return nil, invalidErr("price_id and price_key are exclusive")
	}
	values := url.Values{}
	if !query.PriceID.IsZero() {
		values.Set("price_id", query.PriceID.String())
	}
	if strings.TrimSpace(query.PriceKey) != "" {
		values.Set("price_key", query.PriceKey)
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
