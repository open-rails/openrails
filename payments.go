package openrails

import (
	"context"
	"net/http"
	"net/url"
	"strings"
)

// PublicPrice is the price object public catalog and payment responses embed.
// Amounts are native units at the currency's registered scale.
type PublicPrice struct {
	ID string `json:"id"`
	// Key is the durable, merchant-unique handle for this price's version
	// chain, usable anywhere id is accepted.
	Key        string            `json:"key,omitempty"`
	Object     string            `json:"object"`
	UnitAmount int64             `json:"unit_amount,string"`
	Currency   string            `json:"currency"`
	Type       string            `json:"type,omitempty"` // one_time or recurring
	Recurring  *PriceRecurrence  `json:"recurring,omitempty"`
	Product    string            `json:"product"`
	Active     bool              `json:"active"`
	Providers  []string          `json:"providers,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	Created    int64             `json:"created"`
}

// PriceRecurrence describes a recurring price's interval ("720h", "8760h").
type PriceRecurrence struct {
	Interval string `json:"interval"`
}

// Payment is one rail payment or refund as the merchant surface reports it.
// Amount is native units, positive for payments and negative for refunds.
type Payment struct {
	ID             string  `json:"id"`
	Object         string  `json:"object"`
	Status         string  `json:"status,omitempty"` // succeeded, pending, failed, refunded, partially_refunded
	Amount         int64   `json:"amount,string"`
	AmountRefunded int64   `json:"amount_refunded,string"`
	Currency       string  `json:"currency"`
	User           string  `json:"user"`
	Subscription   *string `json:"subscription,omitempty"`
	Rail           string  `json:"rail"`
	TransactionID  string  `json:"transaction_id"`
	Refunded       bool    `json:"refunded"`
	Captured       bool    `json:"captured,omitempty"`
	// FailureCode is the raw rail decline code; FailureReason the normalized category.
	FailureCode   *string      `json:"failure_code,omitempty"`
	FailureReason *string      `json:"failure_reason,omitempty"`
	Refunds       *PaymentList `json:"refunds,omitempty"`
	Created       int64        `json:"created"`
	Price         *PublicPrice `json:"price,omitempty"`
}

// PaymentList is the refund list embedded in a single payment.
type PaymentList struct {
	Object string    `json:"object"`
	Data   []Payment `json:"data"`
}

// PaymentFilter selects payments. CustomerID and PriceID are plain UUIDs (the
// customer and catalog price ids); Status (pending, completed, failed,
// refunded) and Rail are optional.
type PaymentFilter struct {
	PageOptions
	CustomerID string
	PriceID    string
	Status     string
	Rail       string
}

// GetPayment reads one payment with its refunds.
func (c *Client) GetPayment(ctx context.Context, id string) (*Payment, error) {
	id, err := requireID("payment id", id)
	if err != nil {
		return nil, err
	}
	var out Payment
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/payments/"+url.PathEscape(id), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListPayments lists the merchant's payments, newest first.
func (c *Client) ListPayments(ctx context.Context, filter PaymentFilter) (*Page[Payment], error) {
	q := pageQuery(filter.PageOptions)
	for key, value := range map[string]string{"user_id": filter.CustomerID, "price_id": filter.PriceID, "status": filter.Status, "rail": filter.Rail} {
		if value = strings.TrimSpace(value); value != "" {
			q.Set(key, value)
		}
	}
	var out Page[Payment]
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/payments?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
