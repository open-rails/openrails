package openrails

import (
	"context"
	"fmt"
	"net/http"

	"github.com/open-rails/openrails/billing"
)

// ListSubscriptions returns one page of the merchant's subscriptions matching
// params, newest first.
func (c *Client) ListSubscriptions(ctx context.Context, params billing.SubscriptionListParams, requestOptions ...RequestOption) (*billing.ListPage[billing.Subscription], error) {
	q := pageValues(nil, params.PageRequest)
	if !params.CustomerID.IsZero() {
		q.Set("customer_id", params.CustomerID.String())
	}
	if params.Status != "" {
		q.Set("status", string(params.Status))
	}
	if params.Rail != "" {
		q.Set("rail", params.Rail)
	}
	if !params.PriceID.IsZero() {
		q.Set("price_id", params.PriceID.String())
	}
	if err := setIDs(q, params.IDs); err != nil {
		return nil, err
	}
	var out billing.ListPage[billing.Subscription]
	if err := c.do(ctx, http.MethodGet, "/v1/admin/subscriptions?"+q.Encode(), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

func subscriptionPath(id billing.SubscriptionID) (string, error) {
	subscription, err := requireTypedID("subscription_id", id)
	if err != nil {
		return "", err
	}
	return "/v1/admin/subscriptions/" + subscription, nil
}

func customerPath(customerID string) (string, error) {
	customer, err := requireCustomerID(customerID)
	if err != nil {
		return "", err
	}
	return "/v1/admin/customers/" + customer, nil
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
// RevokeAccess, immediately, and returns the subscription. Both stop provider
// billing.
func (c *Client) CancelSubscription(ctx context.Context, id billing.SubscriptionID, params billing.CancelSubscriptionParams, requestOptions ...RequestOption) (*billing.Subscription, error) {
	return c.subscriptionAction(ctx, http.MethodPost, id, "/cancel", params, requestOptions...)
}

// ResumeSubscription undoes a scheduled cancel before the paid period ends
// and returns the subscription.
func (c *Client) ResumeSubscription(ctx context.Context, id billing.SubscriptionID, requestOptions ...RequestOption) (*billing.Subscription, error) {
	return c.subscriptionAction(ctx, http.MethodPost, id, "/resume", nil, requestOptions...)
}

// SetSubscriptionPaymentMethod charges a subscription's renewals to
// another of the customer's saved payment methods and returns the
// subscription.
func (c *Client) SetSubscriptionPaymentMethod(ctx context.Context, id billing.SubscriptionID, params billing.SetSubscriptionPaymentMethodParams, requestOptions ...RequestOption) (*billing.Subscription, error) {
	if params.PaymentMethodID.IsZero() {
		return nil, invalidErr("payment_method_id is required")
	}
	return c.subscriptionAction(ctx, http.MethodPut, id, "/payment-method", params, requestOptions...)
}

func (c *Client) subscriptionAction(ctx context.Context, method string, id billing.SubscriptionID, suffix string, body any, requestOptions ...RequestOption) (*billing.Subscription, error) {
	path, err := subscriptionPath(id)
	if err != nil {
		return nil, err
	}
	var out billing.Subscription
	if err := c.do(ctx, method, path+suffix, body, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// PreviewTierChange reports what moving a subscription to another price
// would charge and when it would take effect, without changing anything.
func (c *Client) PreviewTierChange(ctx context.Context, id billing.SubscriptionID, params billing.ChangeTierParams, requestOptions ...RequestOption) (*billing.TierChangePreview, error) {
	path, err := subscriptionPath(id)
	if err != nil {
		return nil, err
	}
	if _, err := requireTypedID("price_id", params.PriceID); err != nil {
		return nil, err
	}
	var out billing.TierChangePreview
	if err := c.do(ctx, http.MethodPost, path+"/change-tier/preview", params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// ChangeTier moves a subscription to another price of its tier group. The
// same IdempotencyKey replays the change.
func (c *Client) ChangeTier(ctx context.Context, id billing.SubscriptionID, params billing.ChangeTierParams, requestOptions ...RequestOption) (*billing.TierChange, error) {
	path, err := subscriptionPath(id)
	if err != nil {
		return nil, err
	}
	if _, err := requireTypedID("price_id", params.PriceID); err != nil {
		return nil, err
	}
	var out billing.TierChange
	if err := c.doWithHeaders(ctx, http.MethodPost, path+"/change-tier", params, &out, http.Header{"Idempotency-Key": {params.IdempotencyKey}}, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListPaymentMethods is one page of a customer's saved cards, newest first.
func (c *Client) ListPaymentMethods(ctx context.Context, customerID billing.CustomerID, params billing.PaymentMethodListParams, requestOptions ...RequestOption) (*billing.ListPage[billing.PaymentMethod], error) {
	path, err := customerIDPath(customerID)
	if err != nil {
		return nil, err
	}
	q := pageValues(nil, params.PageRequest)
	if err := setIDs(q, params.IDs); err != nil {
		return nil, err
	}
	var out billing.ListPage[billing.PaymentMethod]
	if err := c.do(ctx, http.MethodGet, path+"/payment-methods?"+q.Encode(), nil, &out, requestOptions...); err != nil {
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

// ListMandates is one page of a customer's mandates, newest first: the
// stored-credential agreements on their saved cards, with the network
// references each storing transaction established.
func (c *Client) ListMandates(ctx context.Context, customerID billing.CustomerID, params billing.MandateListParams, requestOptions ...RequestOption) (*billing.ListPage[billing.Mandate], error) {
	path, err := customerIDPath(customerID)
	if err != nil {
		return nil, err
	}
	q := pageValues(nil, params.PageRequest)
	if err := setIDs(q, params.IDs); err != nil {
		return nil, err
	}
	var out billing.ListPage[billing.Mandate]
	if err := c.do(ctx, http.MethodGet, path+"/mandates?"+q.Encode(), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}
