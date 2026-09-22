package openrails

import (
	"context"
	"net/http"
	"net/url"
)

func (c *Client) CreateCheckoutSession(ctx context.Context, request CreateCheckoutSessionRequest) (*CheckoutSession, error) {
	var out CheckoutSession
	if err := c.doWithHeaders(ctx, http.MethodPost, "/v1/merchant/checkout-sessions", request, &out, http.Header{"Idempotency-Key": {request.IdempotencyKey}}); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) CreatePaymentMethodSession(ctx context.Context, request CreatePaymentMethodSessionRequest) (*CheckoutSession, error) {
	var out CheckoutSession
	if err := c.doWithHeaders(ctx, http.MethodPost, "/v1/merchant/payment-method-sessions", request, &out, http.Header{"Idempotency-Key": {request.IdempotencyKey}}); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) CreateSolanaCancelSession(ctx context.Context, request CreateSolanaCancelSessionRequest) (*CheckoutSession, error) {
	var out CheckoutSession
	if err := c.doWithHeaders(ctx, http.MethodPost, "/v1/merchant/solana-cancel-sessions", request, &out, http.Header{"Idempotency-Key": {request.IdempotencyKey}}); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) CreateSolanaTierChangeSession(ctx context.Context, request CreateSolanaTierChangeSessionRequest) (*CheckoutSession, error) {
	var out CheckoutSession
	if err := c.doWithHeaders(ctx, http.MethodPost, "/v1/merchant/solana-tier-change-sessions", request, &out, http.Header{"Idempotency-Key": {request.IdempotencyKey}}); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) GetCheckoutSession(ctx context.Context, customerID string, sessionID string) (*CheckoutSession, error) {
	typedCustomer, err := ParseCustomerID(customerID)
	if err != nil {
		return nil, invalidErr("invalid customer_id")
	}
	customer, err := requireTypedID("customer_id", typedCustomer)
	if err != nil {
		return nil, err
	}
	typedSession, err := ParseCheckoutSessionID(sessionID)
	if err != nil {
		return nil, invalidErr("invalid session_id")
	}
	session, err := requireTypedID("session_id", typedSession)
	if err != nil {
		return nil, err
	}
	var out CheckoutSession
	path := "/v1/merchant/checkout-sessions/" + session + "?" + url.Values{"customer_id": {customer}}.Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) ConfirmCheckoutSession(ctx context.Context, sessionID string, request ConfirmCheckoutSessionRequest) (*CheckoutSession, error) {
	typedSession, err := ParseCheckoutSessionID(sessionID)
	if err != nil {
		return nil, invalidErr("invalid session_id")
	}
	session, err := requireTypedID("session_id", typedSession)
	if err != nil {
		return nil, err
	}
	var out CheckoutSession
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/checkout-sessions/"+session+"/confirm", request, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) ListCheckoutRailOptions(ctx context.Context, priceID PriceID) ([]CheckoutRailOption, error) {
	price, err := requireTypedID("price_id", priceID)
	if err != nil {
		return nil, err
	}
	var out []CheckoutRailOption
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/checkout-options?"+url.Values{"price_id": {price}}.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetCheckoutConfig returns the bound merchant's public checkout configuration.
func (c *Client) GetCheckoutConfig(ctx context.Context) (*CheckoutConfig, error) {
	var out CheckoutConfig
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/checkout-config", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) ResolveEffectiveTier(ctx context.Context, customerID CustomerID, group string) (*EffectiveTier, error) {
	path, err := customerPath(customerID)
	if err != nil {
		return nil, err
	}
	var out *EffectiveTier
	path += "/effective-tier?" + url.Values{"group": {group}}.Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}
