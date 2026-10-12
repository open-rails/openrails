package openrails

import (
	"context"
	"net/http"

	"github.com/open-rails/openrails/billing"
)

// GetPayment reads one payment with its refunds.
func (c *Client) GetPayment(ctx context.Context, id billing.PaymentID, requestOptions ...RequestOption) (*billing.Payment, error) {
	payment, err := requireTypedID("payment_id", id)
	if err != nil {
		return nil, err
	}
	var out billing.Payment
	if err := c.do(ctx, http.MethodGet, "/v1/admin/payments/"+payment, nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListPayments is one page of the merchant's payments, newest first.
func (c *Client) ListPayments(ctx context.Context, params billing.PaymentListParams, requestOptions ...RequestOption) (*billing.ListPage[billing.Payment], error) {
	q := pageValues(nil, params.PageRequest)
	setQuery(q, map[string]string{"customer_id": params.CustomerID.String(), "subscription_id": params.SubscriptionID.String(),
		"order_id": params.OrderID.String(), "status": string(params.Status), "rail": params.Rail, "kind": string(params.Kind), "transaction_id": params.TransactionID})
	if err := setIDs(q, params.IDs); err != nil {
		return nil, err
	}
	var out billing.ListPage[billing.Payment]
	if err := c.do(ctx, http.MethodGet, "/v1/admin/payments?"+q.Encode(), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// RecordExternalPayment records money the merchant received outside OpenRails for
// one order: part or all of what a usage order owes, or another order's
// total. Recording the same TransactionID again with the
// same terms answers the first payment; with other terms it is
// billing.ErrIdempotencyKeyReused.
func (c *Client) RecordExternalPayment(ctx context.Context, params billing.RecordExternalPaymentParams, requestOptions ...RequestOption) (*billing.Payment, error) {
	var out billing.Payment
	if err := c.do(ctx, http.MethodPost, "/v1/admin/payments", params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}
