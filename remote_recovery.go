package openrails

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/open-rails/openrails/billing"
)

func pageQuery(options billing.PageOptions) url.Values {
	limit := options.Limit
	if limit == 0 {
		limit = 50
	}
	return url.Values{"limit": {strconv.Itoa(limit)}, "offset": {strconv.Itoa(options.Offset)}}
}

// ListSubscriptions returns one page of the merchant's subscriptions matching
// filter.
func (c *Client) ListSubscriptions(ctx context.Context, filter billing.SubscriptionFilter, requestOptions ...RequestOption) (*billing.Page[billing.Subscription], error) {
	q := pageQuery(filter.PageOptions)
	if filter.CustomerID != "" {
		q.Set("customer_id", filter.CustomerID)
	}
	if filter.Status != "" {
		q.Set("status", filter.Status)
	}
	if filter.Rail != "" {
		q.Set("rail", filter.Rail)
	}
	var out billing.Page[billing.Subscription]
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/subscriptions?"+q.Encode(), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

func subscriptionPath(id billing.SubscriptionID) (string, error) {
	subscription, err := requireTypedID("subscription_id", id)
	if err != nil {
		return "", err
	}
	return "/v1/merchant/subscriptions/" + subscription, nil
}

func customerPath(customerID string) (string, error) {
	customer, err := requireCustomerID(customerID)
	if err != nil {
		return "", err
	}
	return "/v1/merchant/customers/" + customer, nil
}

// GetSubscription reads one subscription.
func (c *Client) GetSubscription(ctx context.Context, id billing.SubscriptionID, requestOptions ...RequestOption) (*billing.Subscription, error) {
	path, err := subscriptionPath(id)
	if err != nil {
		return nil, err
	}
	var out billing.Subscription
	if err := c.do(ctx, http.MethodGet, path, nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// CancelSubscription cancels at the end of the paid period or, with
// RevokeAccess, immediately. Both stop provider billing.
func (c *Client) CancelSubscription(ctx context.Context, id billing.SubscriptionID, request billing.CancelSubscriptionRequest, requestOptions ...RequestOption) error {
	path, err := subscriptionPath(id)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, path+"/cancel", request, nil, requestOptions...)
}

// ResumeSubscription queues recovery. A successful return confirms durable
// acceptance; GetSubscription reads the resulting state after worker execution.
func (c *Client) ResumeSubscription(ctx context.Context, id billing.SubscriptionID, requestOptions ...RequestOption) error {
	path, err := subscriptionPath(id)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, path+"/resume", nil, nil, requestOptions...)
}

// UpdateSubscriptionPaymentMethod charges a subscription's renewals to
// another of the customer's saved payment methods.
func (c *Client) UpdateSubscriptionPaymentMethod(ctx context.Context, id billing.SubscriptionID, request billing.UpdateSubscriptionPaymentMethodRequest, requestOptions ...RequestOption) error {
	path, err := subscriptionPath(id)
	if err != nil {
		return err
	}
	if request.PaymentMethodID.IsZero() {
		return invalidErr("payment_method_id is required")
	}
	return c.do(ctx, http.MethodPut, path+"/payment-method", request, nil, requestOptions...)
}

// PreviewTierChange reports what moving a subscription to another price
// would charge and when it would take effect, without changing anything.
func (c *Client) PreviewTierChange(ctx context.Context, id billing.SubscriptionID, request billing.ChangeTierRequest, requestOptions ...RequestOption) (*billing.TierChangePreviewResponse, error) {
	path, err := subscriptionPath(id)
	if err != nil {
		return nil, err
	}
	if _, err := resourcePriceID(request.PriceID); err != nil {
		return nil, err
	}
	var out billing.TierChangePreviewResponse
	if err := c.do(ctx, http.MethodPost, path+"/change-tier/preview", request, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// ChangeTier moves a subscription to another price. The same key replays the
// change.
func (c *Client) ChangeTier(ctx context.Context, id billing.SubscriptionID, key string, request billing.ChangeTierRequest, requestOptions ...RequestOption) (*billing.TierChangeResponse, error) {
	path, err := subscriptionPath(id)
	if err != nil {
		return nil, err
	}
	if _, err := resourcePriceID(request.PriceID); err != nil {
		return nil, err
	}
	var out billing.TierChangeResponse
	if err := c.doWithHeaders(ctx, http.MethodPost, path+"/change-tier", request, &out, http.Header{"Idempotency-Key": {key}}, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListPaymentMethods is one page of a customer's saved cards, newest first.
func (c *Client) ListPaymentMethods(ctx context.Context, customerID billing.CustomerID, page billing.PageRequest, requestOptions ...RequestOption) (*billing.ListPage[billing.PaymentMethod], error) {
	path, err := customerIDPath(customerID)
	if err != nil {
		return nil, err
	}
	var out billing.ListPage[billing.PaymentMethod]
	if err := c.do(ctx, http.MethodGet, path+"/payment-methods?"+pageValues(nil, page).Encode(), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeletePaymentMethod removes a saved payment method. The result says
// whether it is deleted or awaits the provider; pending is never reported
// deleted.
func (c *Client) DeletePaymentMethod(ctx context.Context, customerID billing.CustomerID, methodID billing.PaymentMethodID, requestOptions ...RequestOption) (*billing.PaymentMethodDeletion, error) {
	path, err := customerIDPath(customerID)
	if err != nil {
		return nil, err
	}
	method, err := requireTypedID("payment_method_id", methodID)
	if err != nil {
		return nil, err
	}
	response, err := c.doResponse(ctx, http.MethodDelete, path+"/payment-methods/"+method, nil, nil, requestOptions...)
	if err != nil {
		return nil, err
	}
	switch response.status {
	case http.StatusNoContent:
		return &billing.PaymentMethodDeletion{}, nil
	case http.StatusAccepted:
		return &billing.PaymentMethodDeletion{Pending: true}, nil
	default:
		return nil, fmt.Errorf("%w: unexpected payment deletion status %d", billing.ErrUnreachable, response.status)
	}
}
