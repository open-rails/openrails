package openrails

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/open-rails/openrails/billing"
)

func (c *Client) CreateCheckoutSession(ctx context.Context, request billing.CreateCheckoutSessionRequest, requestOptions ...RequestOption) (*billing.CheckoutSession, error) {
	if (strings.TrimSpace(request.PriceID) == "") == (strings.TrimSpace(request.PriceKey) == "") {
		return nil, invalidErr("exactly one of price_id or price_key is required")
	}
	if request.PriceID != "" {
		if id, err := billing.ParsePriceID(request.PriceID); err != nil || id.IsZero() {
			return nil, invalidErr("price_id must be a valid price ID; use price_key for an opaque key")
		}
	}
	var out billing.CheckoutSession
	if err := c.doWithHeaders(ctx, http.MethodPost, "/v1/merchant/checkout-sessions", request, &out, http.Header{"Idempotency-Key": {request.IdempotencyKey}}, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// LookupCheckoutSession is a read-only idempotency probe for thin host
// wrappers. It never creates, routes, or contacts a provider.
func (c *Client) LookupCheckoutSession(ctx context.Context, request billing.CreateCheckoutSessionRequest, requestOptions ...RequestOption) (*billing.CheckoutSession, error) {
	if strings.TrimSpace(request.IdempotencyKey) == "" {
		return nil, invalidErr("Idempotency-Key required")
	}
	if (strings.TrimSpace(request.PriceID) == "") == (strings.TrimSpace(request.PriceKey) == "") {
		return nil, invalidErr("exactly one of price_id or price_key is required")
	}
	var out billing.CheckoutSession
	if err := c.doWithHeaders(ctx, http.MethodPost, "/v1/merchant/checkout-sessions/lookup", request, &out, http.Header{"Idempotency-Key": {request.IdempotencyKey}}, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) CreatePaymentMethodSession(ctx context.Context, request billing.CreatePaymentMethodSessionRequest, requestOptions ...RequestOption) (*billing.CheckoutSession, error) {
	var out billing.CheckoutSession
	if err := c.doWithHeaders(ctx, http.MethodPost, "/v1/merchant/payment-method-sessions", request, &out, http.Header{"Idempotency-Key": {request.IdempotencyKey}}, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) CreateSolanaCancelSession(ctx context.Context, request billing.CreateSolanaCancelSessionRequest, requestOptions ...RequestOption) (*billing.CheckoutSession, error) {
	var out billing.CheckoutSession
	if err := c.doWithHeaders(ctx, http.MethodPost, "/v1/merchant/solana-cancel-sessions", request, &out, http.Header{"Idempotency-Key": {request.IdempotencyKey}}, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) CreateSolanaTierChangeSession(ctx context.Context, request billing.CreateSolanaTierChangeSessionRequest, requestOptions ...RequestOption) (*billing.CheckoutSession, error) {
	var out billing.CheckoutSession
	if err := c.doWithHeaders(ctx, http.MethodPost, "/v1/merchant/solana-tier-change-sessions", request, &out, http.Header{"Idempotency-Key": {request.IdempotencyKey}}, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) GetCheckoutSession(ctx context.Context, customerID string, sessionID string, requestOptions ...RequestOption) (*billing.CheckoutSession, error) {
	customer, err := requireCustomerID(customerID)
	if err != nil {
		return nil, err
	}
	typedSession, err := billing.ParseCheckoutSessionID(sessionID)
	if err != nil {
		return nil, invalidErr("invalid session_id")
	}
	session, err := requireTypedID("session_id", typedSession)
	if err != nil {
		return nil, err
	}
	var out billing.CheckoutSession
	path := "/v1/merchant/checkout-sessions/" + session + "?" + url.Values{"customer_id": {customer}}.Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetCheckoutSessionByKey retrieves an accepted resource checkout without its
// original payment token. This read never resumes dispatch or alters the strict
// Create/Lookup request fingerprint. Entitlement must match admission exactly.
func (c *Client) GetCheckoutSessionByKey(ctx context.Context, customerID, idempotencyKey, entitlement string, requestOptions ...RequestOption) (*billing.CheckoutSession, error) {
	customer, err := requireCustomerID(customerID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(idempotencyKey) == "" || strings.TrimSpace(entitlement) == "" {
		return nil, invalidErr("idempotency key and entitlement are required")
	}
	var out billing.CheckoutSession
	path := "/v1/merchant/checkout-sessions/by-key?" + url.Values{"customer_id": {customer}, "entitlement": {entitlement}}.Encode()
	if err := c.doWithHeaders(ctx, http.MethodGet, path, nil, &out, http.Header{"Idempotency-Key": {idempotencyKey}}, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) ConfirmCheckoutSession(ctx context.Context, sessionID string, request billing.ConfirmCheckoutSessionRequest, requestOptions ...RequestOption) (*billing.CheckoutSession, error) {
	typedSession, err := billing.ParseCheckoutSessionID(sessionID)
	if err != nil {
		return nil, invalidErr("invalid session_id")
	}
	session, err := requireTypedID("session_id", typedSession)
	if err != nil {
		return nil, err
	}
	var out billing.CheckoutSession
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/checkout-sessions/"+session+"/confirm", request, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) ListCheckoutRailOptions(ctx context.Context, priceID string, requestOptions ...RequestOption) ([]billing.CheckoutRailOption, error) {
	price, err := requirePriceID(priceID)
	if err != nil {
		return nil, err
	}
	var out []billing.CheckoutRailOption
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/checkout-options?"+url.Values{"price_id": {price}}.Encode(), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return out, nil
}

// ListCheckoutRailOptionsByKey reads ready providers for the current offer
// named by an opaque price key, including keys that resemble price IDs.
func (c *Client) ListCheckoutRailOptionsByKey(ctx context.Context, priceKey string, requestOptions ...RequestOption) ([]billing.CheckoutRailOption, error) {
	if strings.TrimSpace(priceKey) == "" {
		return nil, invalidErr("price_key is required")
	}
	var out []billing.CheckoutRailOption
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/checkout-options?"+url.Values{"price_key": {priceKey}}.Encode(), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return out, nil
}

// GetCheckoutConfig returns the bound merchant's public checkout configuration.
func (c *Client) GetCheckoutConfig(ctx context.Context, requestOptions ...RequestOption) (*billing.CheckoutConfig, error) {
	var out billing.CheckoutConfig
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/checkout-config", nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) ResolveEffectiveTier(ctx context.Context, customerID string, group string, requestOptions ...RequestOption) (*billing.EffectiveTier, error) {
	path, err := customerPath(customerID)
	if err != nil {
		return nil, err
	}
	var out *billing.EffectiveTier
	path += "/effective-tier?" + url.Values{"group": {group}}.Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return out, nil
}
