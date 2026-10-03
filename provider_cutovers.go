package openrails

import (
	"context"
	"net/http"
	"net/url"

	"github.com/open-rails/openrails/billing"
)

func (c *Client) PreviewProviderCutover(ctx context.Context, subscriptionID billing.SubscriptionID, req billing.ProviderCutoverRequest, requestOptions ...RequestOption) (*billing.ProviderCutover, error) {
	var out billing.ProviderCutover
	path, err := subscriptionPath(subscriptionID)
	if err != nil {
		return nil, err
	}
	if err := c.do(ctx, http.MethodPost, path+"/provider-cutover/preview", req, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) CutoverProvider(ctx context.Context, subscriptionID billing.SubscriptionID, key string, req billing.ProviderCutoverRequest, requestOptions ...RequestOption) (*billing.ProviderCutover, error) {
	var out billing.ProviderCutover
	path, err := subscriptionPath(subscriptionID)
	if err != nil {
		return nil, err
	}
	if key == "" {
		return nil, invalidErr("idempotency key required")
	}
	if err := c.doWithHeaders(ctx, http.MethodPost, path+"/provider-cutover", req, &out, http.Header{"Idempotency-Key": {key}}, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) GetProviderCutover(ctx context.Context, subscriptionID billing.SubscriptionID, key string, requestOptions ...RequestOption) (*billing.ProviderCutover, error) {
	var out billing.ProviderCutover
	path, err := subscriptionPath(subscriptionID)
	if err != nil {
		return nil, err
	}
	if key == "" {
		return nil, invalidErr("idempotency key required")
	}
	if err := c.do(ctx, http.MethodGet, path+"/provider-cutover?idempotency_key="+url.QueryEscape(key), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}
