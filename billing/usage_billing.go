package billing

import (
	"github.com/open-rails/openrails/catalog"
	"time"
)

// UsageBillingSettings configures monthly, threshold-triggered usage billing.
// Amounts use each currency's native units. An absent currency offers no credit.
type UsageBillingSettings struct {
	BillingDay int                    `json:"billing_day"`
	Currencies []UsageBillingCurrency `json:"currencies"`
}

// UsageBillingCurrency is the merchant's default for one currency.
type UsageBillingCurrency struct {
	Currency             string `json:"currency"`
	CreditLimit          int64  `json:"credit_limit,string"`
	MinimumInvoiceAmount int64  `json:"minimum_invoice_amount,string"`
}

// UsageBillingAccount is a customer's stored usage-billing override and schedule.
// A nil minimum inherits the merchant; the fixed anchor never moves on payment.
type UsageBillingAccount struct {
	Currency             string     `json:"currency"`
	BillingAnchor        *time.Time `json:"billing_anchor"`
	NextBillAt           *time.Time `json:"next_bill_at"`
	MinimumInvoiceAmount *int64     `json:"minimum_invoice_amount,string"`
}

// UsageBillingAccountUpdate patches one currency. A null field restores the
// merchant's default; an omitted field is unchanged.
type UsageBillingAccountUpdate struct {
	Currency             string                   `json:"currency"`
	BillingAnchor        catalog.Field[time.Time] `json:"billing_anchor,omitzero"`
	MinimumInvoiceAmount catalog.Field[int64]     `json:"minimum_invoice_amount,omitzero"`
}
