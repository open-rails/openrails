package openrails

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
)

// PayInvoiceNow requires this Client's token provider to supply verified payer
// credentials. Merchant/service credentials cannot attest customer presence.
// Embedded hosts configure the same verifier through Deps and supply
// the customer's explicit token; ambient request context is never authority.
func (c *Client) PayInvoiceNow(ctx context.Context, request billing.PayInvoiceNowRequest, requestOptions ...RequestOption) (*billing.InvoicePayNowResult, error) {
	id, err := requireUUID("invoice_id", request.InvoiceID)
	if err != nil {
		return nil, err
	}
	if request.PaymentMethodID.IsZero() {
		param := "payment_method_id"
		return nil, &billing.StatusError{Status: http.StatusBadRequest, ErrorDetails: billing.ErrorDetails{Type: "invalid_request_error", Code: billing.CodePaymentMethodRequired, Param: &param, Message: "payment_method_id is required"}}
	}
	var out billing.InvoicePayNowResult
	err = c.doWithHeaders(ctx, http.MethodPost, "/v1/me/invoices/"+id+"/pay-now", request, &out, http.Header{"Idempotency-Key": {request.IdempotencyKey}}, requestOptions...)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// RetrySubscriptionNow requires verified payer credentials, like PayInvoiceNow.
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

// GetMyInvoice reads one of the customer's own invoices. Like PayInvoiceNow
// it needs the customer's own credential.
func (c *Client) GetMyInvoice(ctx context.Context, id uuid.UUID, requestOptions ...RequestOption) (*billing.InvoiceDTO, error) {
	value, err := requireUUID("invoice_id", id)
	if err != nil {
		return nil, err
	}
	var out billing.InvoiceDTO
	if err := c.do(ctx, http.MethodGet, "/v1/me/invoices/"+value, nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetMySubscription reads one of the customer's own subscriptions. Like
// PayInvoiceNow it needs the customer's own credential.
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
