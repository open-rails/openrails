package openrails

import (
	"context"
	"net/http"
	"strings"

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

// ListPayments lists the merchant's payments, newest first.
func (c *Client) ListPayments(ctx context.Context, filter billing.PaymentFilter, requestOptions ...RequestOption) (*billing.Page[billing.Payment], error) {
	q := pageQuery(filter.PageOptions)
	for key, value := range map[string]string{"customer_id": filter.CustomerID, "price_id": filter.PriceID, "status": filter.Status, "rail": filter.Rail} {
		if value = strings.TrimSpace(value); value != "" {
			q.Set(key, value)
		}
	}
	var out billing.Page[billing.Payment]
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/payments?"+q.Encode(), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}
