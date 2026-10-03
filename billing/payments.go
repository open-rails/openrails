package billing

import (
	"time"
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
	CreatedAt  time.Time         `json:"created_at"`
}

// PriceRecurrence describes a recurring price's interval ("720h", "8760h").
type PriceRecurrence struct {
	Interval string `json:"interval"`
}

// Payment is one rail payment or refund as the merchant surface reports it:
// money that moved. A declined charge is a PaymentAttempt, never a Payment.
// Amount is native units, positive for payments and negative for refunds;
// CreatedAt is an RFC3339 instant.
type Payment struct {
	ID             PaymentID       `json:"id"`
	Object         string          `json:"object"`
	Status         string          `json:"status,omitempty"` // succeeded, pending, failed, refunded, partially_refunded
	Amount         int64           `json:"amount,string"`
	AmountRefunded int64           `json:"amount_refunded,string"`
	Currency       string          `json:"currency"`
	CustomerID     string          `json:"customer_id"`
	SubscriptionID *SubscriptionID `json:"subscription_id,omitempty"`
	Rail           string          `json:"rail"`
	TransactionID  string          `json:"transaction_id"`
	Refunded       bool            `json:"refunded"`
	Captured       bool            `json:"captured,omitempty"`
	Refunds        *PaymentList    `json:"refunds,omitempty"`
	// Product is the product this charge (or the charge a refund reverses) bought.
	Product *ProductSummary `json:"product,omitempty"`
	// RefundedPaymentID and Reason are set on refund objects: the charge the
	// refund reverses and the merchant's stated reason.
	RefundedPaymentID *PaymentID   `json:"refunded_payment_id,omitempty"`
	Reason            string       `json:"reason,omitempty"`
	CreatedAt         time.Time    `json:"created_at"`
	Price             *PublicPrice `json:"price,omitempty"`
}

// PaymentList is the refund list embedded in a single payment.
type PaymentList struct {
	Object string    `json:"object"`
	Data   []Payment `json:"data"`
}

// PaymentFilter selects payments; every field is optional. Status is pending,
// completed or refunded.
type PaymentFilter struct {
	PageOptions
	CustomerID string
	PriceID    string
	Status     string
	Rail       string
}
