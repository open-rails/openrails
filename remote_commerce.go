package openrails

import (
	"context"
	"net/http"
	"net/url"
	"strings"
)

func (c *Client) CreateCheckoutSession(ctx context.Context, request CreateCheckoutSessionRequest, requestOptions ...RequestOption) (*CheckoutSession, error) {
	if (strings.TrimSpace(request.PriceID) == "") == (strings.TrimSpace(request.PriceKey) == "") {
		return nil, invalidErr("exactly one of price_id or price_key is required")
	}
	if request.PriceID != "" {
		if id, err := ParsePriceID(request.PriceID); err != nil || id.IsZero() {
			return nil, invalidErr("price_id must be a valid price ID; use price_key for an opaque key")
		}
	}
	var out CheckoutSession
	if err := c.doWithHeaders(ctx, http.MethodPost, "/v1/merchant/checkout-sessions", request, &out, http.Header{"Idempotency-Key": {request.IdempotencyKey}}, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) CreatePaymentMethodSession(ctx context.Context, request CreatePaymentMethodSessionRequest, requestOptions ...RequestOption) (*CheckoutSession, error) {
	var out CheckoutSession
	if err := c.doWithHeaders(ctx, http.MethodPost, "/v1/merchant/payment-method-sessions", request, &out, http.Header{"Idempotency-Key": {request.IdempotencyKey}}, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) CreateSolanaCancelSession(ctx context.Context, request CreateSolanaCancelSessionRequest, requestOptions ...RequestOption) (*CheckoutSession, error) {
	var out CheckoutSession
	if err := c.doWithHeaders(ctx, http.MethodPost, "/v1/merchant/solana-cancel-sessions", request, &out, http.Header{"Idempotency-Key": {request.IdempotencyKey}}, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) CreateSolanaTierChangeSession(ctx context.Context, request CreateSolanaTierChangeSessionRequest, requestOptions ...RequestOption) (*CheckoutSession, error) {
	var out CheckoutSession
	if err := c.doWithHeaders(ctx, http.MethodPost, "/v1/merchant/solana-tier-change-sessions", request, &out, http.Header{"Idempotency-Key": {request.IdempotencyKey}}, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) GetCheckoutSession(ctx context.Context, customerID string, sessionID string, requestOptions ...RequestOption) (*CheckoutSession, error) {
	customer, err := requireCustomerID(customerID)
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
	if err := c.do(ctx, http.MethodGet, path, nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) ConfirmCheckoutSession(ctx context.Context, sessionID string, request ConfirmCheckoutSessionRequest, requestOptions ...RequestOption) (*CheckoutSession, error) {
	typedSession, err := ParseCheckoutSessionID(sessionID)
	if err != nil {
		return nil, invalidErr("invalid session_id")
	}
	session, err := requireTypedID("session_id", typedSession)
	if err != nil {
		return nil, err
	}
	var out CheckoutSession
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/checkout-sessions/"+session+"/confirm", request, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) ListCheckoutRailOptions(ctx context.Context, priceID string, requestOptions ...RequestOption) ([]CheckoutRailOption, error) {
	price, err := requirePriceID(priceID)
	if err != nil {
		return nil, err
	}
	var out []CheckoutRailOption
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/checkout-options?"+url.Values{"price_id": {price}}.Encode(), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return out, nil
}

// ListCheckoutRailOptionsByKey reads ready providers for the current offer
// named by an opaque price key, including keys that resemble price IDs.
func (c *Client) ListCheckoutRailOptionsByKey(ctx context.Context, priceKey string, requestOptions ...RequestOption) ([]CheckoutRailOption, error) {
	if strings.TrimSpace(priceKey) == "" {
		return nil, invalidErr("price_key is required")
	}
	var out []CheckoutRailOption
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/checkout-options?"+url.Values{"price_key": {priceKey}}.Encode(), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return out, nil
}

// GetCheckoutConfig returns the bound merchant's public checkout configuration.
func (c *Client) GetCheckoutConfig(ctx context.Context, requestOptions ...RequestOption) (*CheckoutConfig, error) {
	var out CheckoutConfig
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/checkout-config", nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) ResolveEffectiveTier(ctx context.Context, customerID string, group string, requestOptions ...RequestOption) (*EffectiveTier, error) {
	path, err := customerPath(customerID)
	if err != nil {
		return nil, err
	}
	var out *EffectiveTier
	path += "/effective-tier?" + url.Values{"group": {group}}.Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return out, nil
}
