package billing

import (
	"time"

	"github.com/open-rails/openrails/catalog"
)

// UsageBillingSettings configures monthly, threshold-triggered usage billing.
// Amounts use each currency's native units. An absent currency offers no credit.
type UsageBillingSettings struct {
	BillingDay int                    `json:"billing_day"`
	Currencies []UsageBillingCurrency `json:"currencies"`
}

// UsageBillingCurrency is the merchant's default for one currency.
type UsageBillingCurrency struct {
	Currency             catalog.Currency `json:"currency"`
	CreditLimit          int64            `json:"credit_limit,string"`
	MinimumInvoiceAmount int64            `json:"minimum_invoice_amount,string"`
	// CollectionThreshold invoices arrears in the currency once they reach it,
	// before the credit limit: nil is the credit limit.
	CollectionThreshold *int64 `json:"collection_threshold,string"`
	// DelinquencyFloor is the smallest overdue balance in the currency that
	// can make a customer delinquent: nil is one unit of the currency, 0
	// delinquent once anything is overdue.
	DelinquencyFloor *int64 `json:"delinquency_floor,string"`
}

// UsageBillingAccount is a customer's stored usage-billing override and schedule.
// A nil minimum inherits the merchant; the fixed anchor never moves on payment.
type UsageBillingAccount struct {
	Currency             catalog.Currency `json:"currency"`
	BillingAnchor        *time.Time       `json:"billing_anchor"`
	NextBillAt           *time.Time       `json:"next_bill_at"`
	MinimumInvoiceAmount *int64           `json:"minimum_invoice_amount,string"`
}

// UsageBillingAccountUpdate patches one currency. A null field restores the
// merchant's default; an omitted field is unchanged.
type UsageBillingAccountUpdate struct {
	Currency             catalog.Currency         `json:"currency"`
	BillingAnchor        catalog.Field[time.Time] `json:"billing_anchor,omitzero"`
	MinimumInvoiceAmount catalog.Field[int64]     `json:"minimum_invoice_amount,omitzero"`
}
