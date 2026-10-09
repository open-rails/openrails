package openrails

import (
	"context"
	"net/http"
	"net/url"

	"github.com/open-rails/openrails/billing"
)

// GetPayment reads one payment with its refunds.
func (c *Client) GetPayment(ctx context.Context, id billing.PaymentID, requestOptions ...RequestOption) (*billing.Payment, error) {
	payment, err := requireTypedID("payment_id", id)
	if err != nil {
		return nil, err
	}
	var out billing.Payment
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/payments/"+payment, nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListPayments is one page of the merchant's payments, newest first.
func (c *Client) ListPayments(ctx context.Context, params billing.PaymentListParams, requestOptions ...RequestOption) (*billing.ListPage[billing.Payment], error) {
	q := pageValues(nil, params.PageRequest)
	setQuery(q, map[string]string{"customer_id": params.CustomerID.String(), "subscription_id": params.SubscriptionID.String(), "price_id": params.PriceID.String(),
		"rail": params.Rail, "kind": string(params.Kind), "transaction_id": params.TransactionID})
	if err := setIDs(q, params.IDs); err != nil {
		return nil, err
	}
	var out billing.ListPage[billing.Payment]
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/payments?"+q.Encode(), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateOffChannelPayment records a purchase a customer paid outside any rail
// (cash, bank transfer). Recording the same TransactionID again with the same
// terms answers the first payment; with other terms it is
// billing.ErrIdempotencyKeyReused.
func (c *Client) CreateOffChannelPayment(ctx context.Context, customerID billing.CustomerID, params billing.CreateOffChannelPaymentParams, requestOptions ...RequestOption) (*billing.Payment, error) {
	path, err := customerIDPath(customerID)
	if err != nil {
		return nil, err
	}
	if params.PriceID.IsZero() || params.TransactionID == "" {
		return nil, invalidErr("price_id and transaction_id are required")
	}
	var out billing.Payment
	if err := c.do(ctx, http.MethodPost, path+"/payments/off-channel", params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetPaymentSettlementStatus reports whether a customer ever paid for a price
// through a rail. Refunding or archiving that payment does not undo it.
func (c *Client) GetPaymentSettlementStatus(ctx context.Context, customerID billing.CustomerID, priceID billing.PriceID, requestOptions ...RequestOption) (*billing.PaymentSettlementStatus, error) {
	path, err := customerIDPath(customerID)
	if err != nil {
		return nil, err
	}
	price, err := requireTypedID("price_id", priceID)
	if err != nil {
		return nil, err
	}
	var out billing.PaymentSettlementStatus
	if err := c.do(ctx, http.MethodGet, path+"/payment-settlement-status?price_id="+url.QueryEscape(price), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}
