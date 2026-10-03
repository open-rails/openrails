package openrails

import (
	"context"
	"net/http"

	"github.com/open-rails/openrails/billing"
)

func (c *Client) PreviewEngineTakeover(ctx context.Context, subscriptionID billing.SubscriptionID, requestOptions ...RequestOption) (*billing.EngineTakeover, error) {
	return c.engineTakeover(ctx, http.MethodPost, subscriptionID, "/engine-takeover/preview", nil, requestOptions...)
}

// TakeOverBilling admits and runs the takeover; the same key replays it.
func (c *Client) TakeOverBilling(ctx context.Context, subscriptionID billing.SubscriptionID, idempotencyKey string, requestOptions ...RequestOption) (*billing.EngineTakeover, error) {
	if idempotencyKey == "" {
		return nil, invalidErr("idempotency key required")
	}
	return c.engineTakeover(ctx, http.MethodPost, subscriptionID, "/engine-takeover", http.Header{"Idempotency-Key": {idempotencyKey}}, requestOptions...)
}

// GetEngineTakeover reads the subscription's latest takeover.
func (c *Client) GetEngineTakeover(ctx context.Context, subscriptionID billing.SubscriptionID, requestOptions ...RequestOption) (*billing.EngineTakeover, error) {
	return c.engineTakeover(ctx, http.MethodGet, subscriptionID, "/engine-takeover", nil, requestOptions...)
}

// AbandonEngineTakeover ends a takeover that has not yet changed NMI.
func (c *Client) AbandonEngineTakeover(ctx context.Context, subscriptionID billing.SubscriptionID, requestOptions ...RequestOption) (*billing.EngineTakeover, error) {
	return c.engineTakeover(ctx, http.MethodPost, subscriptionID, "/engine-takeover/abandon", nil, requestOptions...)
}

// TakeOverBillingBatch admits takeovers in bulk; the executor runs them under
// the destructive switch and volume breaker.
func (c *Client) TakeOverBillingBatch(ctx context.Context, request billing.EngineTakeoverBatchRequest, requestOptions ...RequestOption) (*billing.EngineTakeoverBatchResult, error) {
	if request.MaxSubscriptions <= 0 {
		return nil, invalidErr("max_subscriptions must be positive")
	}
	var out billing.EngineTakeoverBatchResult
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/engine-takeovers", request, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) engineTakeover(ctx context.Context, method string, subscriptionID billing.SubscriptionID, suffix string, headers http.Header, requestOptions ...RequestOption) (*billing.EngineTakeover, error) {
	path, err := subscriptionPath(subscriptionID)
	if err != nil {
		return nil, err
	}
	var body any
	if method == http.MethodPost {
		body = struct{}{}
	}
	var out billing.EngineTakeover
	if err := c.doWithHeaders(ctx, method, path+suffix, body, &out, headers, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}
