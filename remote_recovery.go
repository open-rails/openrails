package openrails

import (
	"context"
	"fmt"
	"github.com/google/uuid"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

func pageQuery(options PageOptions) url.Values {
	limit := options.Limit
	if limit == 0 {
		limit = 50
	}
	return url.Values{"limit": {strconv.Itoa(limit)}, "offset": {strconv.Itoa(options.Offset)}}
}

func (c *Client) ListSubscriptions(ctx context.Context, filter SubscriptionFilter) (*Page[Subscription], error) {
	q := pageQuery(filter.PageOptions)
	if filter.CustomerID != "" {
		q.Set("user_id", filter.CustomerID)
	}
	if filter.Status != "" {
		q.Set("status", filter.Status)
	}
	if filter.Rail != "" {
		q.Set("rail", filter.Rail)
	}
	var out Page[Subscription]
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/subscriptions?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func subscriptionPath(id string) (string, error) {
	id, err := pathID("subscription_id", id)
	if err != nil {
		return "", err
	}
	return "/v1/merchant/subscriptions/" + id, nil
}

func customerPath(customerID string) (string, error) {
	id, err := pathID("customer_id", customerID)
	if err != nil {
		return "", err
	}
	return "/v1/merchant/customers/" + id, nil
}

func (c *Client) GetSubscription(ctx context.Context, id string) (*Subscription, error) {
	path, err := subscriptionPath(id)
	if err != nil {
		return nil, err
	}
	var out Subscription
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) CancelSubscription(ctx context.Context, id string, request CancelSubscriptionRequest) error {
	path, err := subscriptionPath(id)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, path+"/cancel", request, nil)
}

// ResumeSubscription queues recovery. A successful return confirms durable
// acceptance; GetSubscription reads the resulting state after worker execution.
func (c *Client) ResumeSubscription(ctx context.Context, id string) error {
	path, err := subscriptionPath(id)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, path+"/resume", nil, nil)
}

func (c *Client) UpdateSubscriptionPaymentMethod(ctx context.Context, id string, request UpdateSubscriptionPaymentMethodRequest) error {
	path, err := subscriptionPath(id)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPut, path+"/payment-method", request, nil)
}

func (c *Client) PreviewTierChange(ctx context.Context, id string, request ChangeTierRequest) (*TierChangePreviewResponse, error) {
	path, err := subscriptionPath(id)
	if err != nil {
		return nil, err
	}
	var out TierChangePreviewResponse
	if err := c.do(ctx, http.MethodPost, path+"/change-tier/preview", request, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) ChangeTier(ctx context.Context, id, key string, request ChangeTierRequest) (*TierChangeResponse, error) {
	path, err := subscriptionPath(id)
	if err != nil {
		return nil, err
	}
	var out TierChangeResponse
	if err := c.doWithHeaders(ctx, http.MethodPost, path+"/change-tier", request, &out, http.Header{"Idempotency-Key": {key}}); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) ListPaymentMethods(ctx context.Context, customerID string, options PageOptions) (*Page[PaymentMethod], error) {
	path, err := customerPath(customerID)
	if err != nil {
		return nil, err
	}
	var out Page[PaymentMethod]
	if err := c.do(ctx, http.MethodGet, path+"/payment-methods?"+pageQuery(options).Encode(), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PaymentMethodDeletion distinguishes completed deletion from a durable
// operation awaiting provider reconciliation. Pending is never reported deleted.
type PaymentMethodDeletion struct{ Pending bool }

func (c *Client) DeletePaymentMethod(ctx context.Context, customerID, methodID string) (*PaymentMethodDeletion, error) {
	path, err := customerPath(customerID)
	if err != nil {
		return nil, err
	}
	method, err := pathID("payment_method_id", methodID)
	if err != nil {
		return nil, err
	}
	response, err := c.doResponse(ctx, http.MethodDelete, path+"/payment-methods/"+method, nil, nil)
	if err != nil {
		return nil, err
	}
	switch response.status {
	case http.StatusNoContent:
		return &PaymentMethodDeletion{}, nil
	case http.StatusAccepted:
		return &PaymentMethodDeletion{Pending: true}, nil
	default:
		return nil, fmt.Errorf("%w: unexpected payment deletion status %d", ErrUnreachable, response.status)
	}
}

// PayInvoiceNow charges one open or past-due invoice of request.CustomerID
// through their saved method, as the customer's own pay-now would. The
// engine creates a new immutable attempt under request.IdempotencyKey; the
// same key replays the same attempt without a second charge. A decline is
// ErrCardDeclined (402); a provider-managed rail is
// ErrPaymentRecoveryRailUnsupported before any provider traffic. An
// unresolved outcome returns normally with Operation.Unresolved().
func (c *Client) PayInvoiceNow(ctx context.Context, request PayInvoiceNowRequest) (*InvoicePayNowResult, error) {
	path, err := customerPath(request.CustomerID.String())
	if err != nil {
		return nil, err
	}
	invoice, err := requireUUID("invoice_id", request.InvoiceID)
	if err != nil {
		return nil, err
	}
	if request.PaymentMethodID == uuid.Nil {
		return nil, invalidErr("payment_method_id is required")
	}
	if strings.TrimSpace(request.IdempotencyKey) == "" {
		return nil, invalidErr("idempotency_key is required")
	}
	var out InvoicePayNowResult
	if err := c.doWithHeaders(ctx, http.MethodPost, path+"/invoices/"+invoice+"/pay-now", request, &out, http.Header{"Idempotency-Key": {request.IdempotencyKey}}); err != nil {
		return nil, err
	}
	return &out, nil
}

// RetrySubscriptionNow rebills one past-due subscription of request.CustomerID
// now through its current saved method, as the customer's own retry-now
// would. A confirmed charge renews the subscription and clears its dunning
// schedule; a decline is ErrCardDeclined (402); an unresolved outcome returns
// normally with Operation.Unresolved().
func (c *Client) RetrySubscriptionNow(ctx context.Context, request RetrySubscriptionNowRequest) (*SubscriptionRetryNowResult, error) {
	path, err := customerPath(request.CustomerID.String())
	if err != nil {
		return nil, err
	}
	subscription, err := requireUUID("subscription_id", request.SubscriptionID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(request.IdempotencyKey) == "" {
		return nil, invalidErr("idempotency_key is required")
	}
	var out SubscriptionRetryNowResult
	if err := c.doWithHeaders(ctx, http.MethodPost, path+"/subscriptions/"+subscription+"/retry-now", request, &out, http.Header{"Idempotency-Key": {request.IdempotencyKey}}); err != nil {
		return nil, err
	}
	return &out, nil
}
