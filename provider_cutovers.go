package openrails

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"
)

// ProviderCutoverRequest asserts both accounts and names the customer's newly
// vaulted card. No provider reference or paid-through date comes from the caller.
type ProviderCutoverRequest struct {
	TargetPaymentMethodID PaymentMethodID `json:"target_payment_method_id"`
	ExpectedSourcePSPID   uuid.UUID       `json:"expected_source_psp_id"`
	ExpectedTargetPSPID   uuid.UUID       `json:"expected_target_psp_id"`
}

type ProviderCutover struct {
	ID                    uuid.UUID       `json:"id"`
	MerchantID            uuid.UUID       `json:"merchant_id"`
	SubscriptionID        SubscriptionID  `json:"subscription_id"`
	SourcePSPID           uuid.UUID       `json:"source_psp_id"`
	TargetPSPID           uuid.UUID       `json:"target_psp_id"`
	TargetPaymentMethodID PaymentMethodID `json:"target_payment_method_id"`
	TargetSubscriptionID  string          `json:"target_subscription_id"`
	Anchor                time.Time       `json:"anchor"`
	Status                string          `json:"status"`
	Stage                 string          `json:"stage"`
	Reason                string          `json:"reason"`
}

func (c *Client) PreviewProviderCutover(ctx context.Context, subscriptionID SubscriptionID, req ProviderCutoverRequest, requestOptions ...RequestOption) (*ProviderCutover, error) {
	var out ProviderCutover
	path, err := subscriptionPath(subscriptionID)
	if err != nil {
		return nil, err
	}
	if err := c.do(ctx, http.MethodPost, path+"/provider-cutover/preview", req, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) CutoverProvider(ctx context.Context, subscriptionID SubscriptionID, key string, req ProviderCutoverRequest, requestOptions ...RequestOption) (*ProviderCutover, error) {
	var out ProviderCutover
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

func (c *Client) GetProviderCutover(ctx context.Context, subscriptionID SubscriptionID, key string, requestOptions ...RequestOption) (*ProviderCutover, error) {
	var out ProviderCutover
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
