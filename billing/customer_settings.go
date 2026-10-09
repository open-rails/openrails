package billing

import "github.com/open-rails/openrails/catalog"

// CustomerSettings is the configuration a merchant keeps for one customer:
// its credit lines, trust levels, billing policy and invoice terms. A field
// at its default is absent: a currency with no credit limit or trust level
// is not listed, a null BillingPolicy inherits the customer's tier's or the
// merchant's default policy, and a null InvoiceProfile invoices at net 0,
// charged automatically.
type CustomerSettings struct {
	CreditLimits   []CreditLimit   `json:"credit_limits"`
	TrustLevels    []TrustLevel    `json:"trust_levels"`
	BillingPolicy  *string         `json:"billing_policy"`
	InvoiceProfile *InvoiceProfile `json:"invoice_profile"`
}

// CreditLimit is how much a customer may owe in arrears in one currency;
// zero allows no arrears.
type CreditLimit struct {
	Currency string `json:"currency"`
	Amount   int64  `json:"amount,string"`
}

// TrustLevel is the trust tier a customer's admissions are judged at in one
// currency when a request names none; empty is the default tier.
type TrustLevel struct {
	Currency   string `json:"currency"`
	TrustLevel string `json:"trust_level"`
}

// UpdateCustomerParams changes the settings it names for one customer; an
// omitted field is unchanged. CreditLimits and TrustLevels merge by currency:
// amount 0 or an empty level clears that currency's. A null BillingPolicy or
// InvoiceProfile clears it. Every field can change the customer's spending
// authority: a person signed in needs a recent sign-in for any write.
type UpdateCustomerParams struct {
	CreditLimits   []CreditLimit                 `json:"credit_limits,omitempty"`
	TrustLevels    []TrustLevel                  `json:"trust_levels,omitempty"`
	BillingPolicy  catalog.Field[string]         `json:"billing_policy,omitzero"`
	InvoiceProfile catalog.Field[InvoiceProfile] `json:"invoice_profile,omitzero"`
}
