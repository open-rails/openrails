package billing

import "github.com/open-rails/openrails/catalog"

// CustomerSettingsResult confirms a customer's saved billing settings.
type CustomerSettingsResult struct {
	ID       CustomerID       `json:"id"`
	Settings CustomerSettings `json:"settings"`
}

// CustomerSettings is the configuration a merchant keeps for one customer:
// its credit lines, trust levels, low-balance lines, billing policy and
// invoice terms. A field at its default is absent: a currency with no credit
// limit, trust level or own low-balance line is not listed, a null
// BillingPolicy inherits the customer's tier's or the merchant's default
// policy, and a null InvoiceProfile invoices at net 0, charged automatically.
type CustomerSettings struct {
	CreditLimits    []CreditLimit    `json:"credit_limits"`
	TrustLevels     []TrustLevel     `json:"trust_levels"`
	LowBalanceLines []LowBalanceLine `json:"low_balance_lines"`
	BillingPolicy   *string          `json:"billing_policy"`
	InvoiceProfile  *InvoiceProfile  `json:"invoice_profile"`
}

// MaxLowBalanceRunwayHours bounds a runway line: one year.
const MaxLowBalanceRunwayHours = 8760

// LowBalanceLine is a customer's own low-balance line in one currency. The
// customer counts as low when what it may spend is under the larger of Amount
// and its burn rate times RunwayHours. 0 turns that part off; null inherits
// the merchant's settings.balance_notices, and RunwayHours then 24.
type LowBalanceLine struct {
	Currency    string `json:"currency"`
	Amount      *int64 `json:"amount,string"`
	RunwayHours *int   `json:"runway_hours"`
}

// SetLowBalanceLineParams sets the signed-in customer's low-balance line in
// one currency; null inherits the merchant's.
type SetLowBalanceLineParams struct {
	Amount      *int64 `json:"amount,string"`
	RunwayHours *int   `json:"runway_hours"`
}

// LowBalanceLineSource is where an effective low-balance line comes from.
type LowBalanceLineSource string

const (
	// LowBalanceLineCustomer: the customer set part of it.
	LowBalanceLineCustomer LowBalanceLineSource = "customer"
	// LowBalanceLineMerchant: the merchant's settings.balance_notices.
	LowBalanceLineMerchant LowBalanceLineSource = "merchant"
	// LowBalanceLineDefault: neither; a 24-hour runway and no amount line.
	LowBalanceLineDefault LowBalanceLineSource = "default"
)

// EffectiveLowBalanceLine is the low-balance line a customer's balance in
// one currency is judged at, each part resolved customer, merchant, built in.
// Source is the most specific that set a part.
type EffectiveLowBalanceLine struct {
	Currency    string               `json:"currency"`
	Amount      int64                `json:"amount,string"`
	RunwayHours int                  `json:"runway_hours"`
	Source      LowBalanceLineSource `json:"source"`
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
// omitted field is unchanged. CreditLimits, TrustLevels and LowBalanceLines
// merge by currency: amount 0 or an empty level clears that currency's, and a
// low-balance line replaces that currency's (nulls inherit the merchant's). A
// null BillingPolicy or InvoiceProfile clears it. Every field can change the
// customer's spending authority: a person signed in needs a recent sign-in for
// any write.
type UpdateCustomerParams struct {
	CreditLimits    []CreditLimit                 `json:"credit_limits,omitempty"`
	TrustLevels     []TrustLevel                  `json:"trust_levels,omitempty"`
	LowBalanceLines []LowBalanceLine              `json:"low_balance_lines,omitempty"`
	BillingPolicy   catalog.Field[string]         `json:"billing_policy,omitzero"`
	InvoiceProfile  catalog.Field[InvoiceProfile] `json:"invoice_profile,omitzero"`
}
