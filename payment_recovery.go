package openrails

import (
	"context"
	"net/http"

	"github.com/google/uuid"
)

// PaymentRecovery describes a payer-owned resource's current recovery state.
// It is a view, not permission to charge: acceptance checks the same facts
// again under the resource lock.
type PaymentRecovery struct {
	Retryable     bool              `json:"retryable"`
	BlockedReason string            `json:"blocked_reason,omitempty"`
	Operation     *PaymentOperation `json:"operation,omitempty"`
}

type PaymentOperation struct {
	ID     uuid.UUID `json:"id"`
	Status string    `json:"status"`
}

func (o PaymentOperation) Unresolved() bool {
	switch o.Status {
	case "pending", "in_flight", "failed_retryable", "unknown_needs_verify":
		return true
	}
	return false
}

type PayInvoiceNowRequest struct {
	InvoiceID       uuid.UUID       `json:"-"`
	IdempotencyKey  string          `json:"-"`
	PaymentMethodID PaymentMethodID `json:"payment_method_id"`
}

type InvoicePayNowResult struct {
	Invoice   InvoiceDTO               `json:"invoice"`
	Attempt   InvoicePaymentAttemptDTO `json:"attempt"`
	Operation PaymentOperation         `json:"operation"`
	Replayed  bool                     `json:"replayed"`
}

type RetrySubscriptionNowRequest struct {
	SubscriptionID SubscriptionID `json:"-"`
	IdempotencyKey string         `json:"-"`
	// If supplied, the method must be the subscription's current saved method.
	PaymentMethodID *PaymentMethodID `json:"payment_method_id,omitempty"`
}

type SubscriptionRetryNowResult struct {
	Subscription Subscription     `json:"subscription"`
	Operation    PaymentOperation `json:"operation"`
	Replayed     bool             `json:"replayed"`
}

// PayInvoiceNow requires this Client's token provider to supply verified payer
// credentials. Merchant/service credentials cannot attest customer presence.
// Embedded hosts configure the same verifier through embed.Options and supply
// the customer's explicit token; ambient request context is never authority.
func (c *Client) PayInvoiceNow(ctx context.Context, request PayInvoiceNowRequest) (*InvoicePayNowResult, error) {
	id, err := requireUUID("invoice_id", request.InvoiceID)
	if err != nil {
		return nil, err
	}
	if request.PaymentMethodID.IsZero() {
		return nil, invalidErr("payment_method_id is required")
	}
	var out InvoicePayNowResult
	err = c.doWithHeaders(ctx, http.MethodPost, "/v1/me/invoices/"+id+"/pay-now", request, &out, http.Header{"Idempotency-Key": {request.IdempotencyKey}})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// RetrySubscriptionNow requires verified payer credentials, like PayInvoiceNow.
// Unresolved replies are read through the existing subscription resource.
func (c *Client) RetrySubscriptionNow(ctx context.Context, request RetrySubscriptionNowRequest) (*SubscriptionRetryNowResult, error) {
	id, err := requireTypedID("subscription_id", request.SubscriptionID)
	if err != nil {
		return nil, err
	}
	if request.PaymentMethodID != nil && request.PaymentMethodID.IsZero() {
		return nil, invalidErr("payment_method_id is invalid")
	}
	var out SubscriptionRetryNowResult
	err = c.doWithHeaders(ctx, http.MethodPost, "/v1/me/subscriptions/"+id+"/retry-now", request, &out, http.Header{"Idempotency-Key": {request.IdempotencyKey}})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) GetMyInvoice(ctx context.Context, id uuid.UUID) (*InvoiceDTO, error) {
	value, err := requireUUID("invoice_id", id)
	if err != nil {
		return nil, err
	}
	var out InvoiceDTO
	if err := c.do(ctx, http.MethodGet, "/v1/me/invoices/"+value, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) GetMySubscription(ctx context.Context, id SubscriptionID) (*Subscription, error) {
	value, err := requireTypedID("subscription_id", id)
	if err != nil {
		return nil, err
	}
	var out Subscription
	if err := c.do(ctx, http.MethodGet, "/v1/me/subscriptions/"+value, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
