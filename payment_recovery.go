package openrails

import (
	"context"
	"net/http"

	"github.com/open-rails/openrails/billing"
)

// RetrySubscriptionNow requires verified payer credentials.
// Unresolved replies are read through the existing subscription resource.
func (c *Client) RetrySubscriptionNow(ctx context.Context, request billing.RetrySubscriptionNowRequest, requestOptions ...RequestOption) (*billing.SubscriptionRetryNowResult, error) {
	id, err := requireTypedID("subscription_id", request.SubscriptionID)
	if err != nil {
		return nil, err
	}
	if request.PaymentMethodID != nil && request.PaymentMethodID.IsZero() {
		return nil, invalidErr("payment_method_id is invalid")
	}
	var out billing.SubscriptionRetryNowResult
	err = c.doWithHeaders(ctx, http.MethodPost, "/v1/me/subscriptions/"+id+"/retry-now", request, &out, http.Header{"Idempotency-Key": {request.IdempotencyKey}}, requestOptions...)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// GetMySubscription reads one of the customer's own subscriptions; it needs
// the customer's own credential.
func (c *Client) GetMySubscription(ctx context.Context, id billing.SubscriptionID, requestOptions ...RequestOption) (*billing.Subscription, error) {
	value, err := requireTypedID("subscription_id", id)
	if err != nil {
		return nil, err
	}
	var out billing.Subscription
	if err := c.do(ctx, http.MethodGet, "/v1/me/subscriptions/"+value, nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}
