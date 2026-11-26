package billing

import (
	"time"

	"github.com/open-rails/openrails/catalog"
)

// CustomerSettingsResult confirms a customer's saved billing settings.
type CustomerSettingsResult struct {
	ID       CustomerID       `json:"id"`
	Settings CustomerSettings `json:"settings"`
}

// CustomerSettings is the configuration a merchant keeps for one customer:
// its credit lines, trust levels, low-balance lines, limit overrides,
// billing policy and invoice terms. A field at its default is absent: a
// currency with no credit limit, trust level or own low-balance line, and a
// limit with no override, is not listed, a null
// BillingPolicy inherits the customer's tier's or the merchant's default
// policy, and a null InvoiceProfile invoices at net 0, charged automatically.
type CustomerSettings struct {
	UsageBilling    []UsageBillingAccount `json:"usage_billing,omitempty"`
	CreditLimits    []CreditLimit         `json:"credit_limits"`
	TrustLevels     []TrustLevel          `json:"trust_levels"`
	LowBalanceLines []LowBalanceLine      `json:"low_balance_lines"`
	Limits          []LimitOverride       `json:"limits"`
	BillingPolicy   *string               `json:"billing_policy"`
	InvoiceProfile  *InvoiceProfile       `json:"invoice_profile"`
}

// MaxLowBalanceRunwayHours bounds a runway line: one year.
const MaxLowBalanceRunwayHours = 8760

// LowBalanceLine is a customer's own low-balance line in one currency. The
// customer counts as low when what it may spend is under the larger of Amount
// and its burn rate times RunwayHours. 0 turns that part off; null inherits
// the merchant's settings.balance_notices, and RunwayHours then 24.
type LowBalanceLine struct {
	Currency    catalog.Currency `json:"currency"`
	Amount      *int64           `json:"amount,string"`
	RunwayHours *int             `json:"runway_hours"`
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
	Currency    catalog.Currency     `json:"currency"`
	Amount      int64                `json:"amount,string"`
	RunwayHours int                  `json:"runway_hours"`
	Source      LowBalanceLineSource `json:"source"`
}

// CreditLimit is how much a customer may owe in arrears in one currency;
// zero allows no arrears.
type CreditLimit struct {
	// Inherit clears an override. Amount zero without Inherit explicitly disables credit.
	Inherit  bool             `json:"inherit,omitempty"`
	Currency catalog.Currency `json:"currency"`
	Amount   int64            `json:"amount,string"`
}

// TrustLevel is a customer's explicit trust tier in one currency: it beats
// the merchant's trust_tiers rules. A request's own trust level picks only
// that request's spend windows.
type TrustLevel struct {
	Currency   catalog.Currency `json:"currency"`
	TrustLevel string           `json:"trust_level"`
}

// TrustLevelSource is where an effective trust level comes from.
type TrustLevelSource string

const (
	// TrustLevelExplicit: the customer's own trust level, or the request's.
	TrustLevelExplicit TrustLevelSource = "explicit"
	// TrustLevelRule: the highest of the merchant's trust_tiers it meets.
	TrustLevelRule TrustLevelSource = "rule"
	// TrustLevelDefault: neither; the default tier, free.
	TrustLevelDefault TrustLevelSource = "default"
)

// EffectiveTrustLevel is the trust tier a customer's requests in one
// currency are judged at, with what the rules read: what it paid net of
// refunds and chargebacks, and when it first paid.
type EffectiveTrustLevel struct {
	Currency      catalog.Currency `json:"currency"`
	TrustLevel    string           `json:"trust_level"`
	Source        TrustLevelSource `json:"source"`
	NetPaidAmount int64            `json:"net_paid_amount,string"`
	FirstPaidAt   *time.Time       `json:"first_paid_at"`
}

// UpdateCustomerParams changes the settings it names for one customer; an
// omitted field is unchanged. CreditLimits, TrustLevels and LowBalanceLines
// merge by currency: a credit limit's inherit clears that currency's and
// amount 0 allows no arrears, an empty level clears that currency's, and a
// low-balance line replaces that currency's (nulls inherit the merchant's).
// Limits merge by limit: an override replaces what the customer's products
// give, and inherit clears it. A null BillingPolicy or InvoiceProfile clears
// it. Every field can change the customer's spending authority, so a write
// needs the admin route's permission; no recent sign-in is asked for.
type UpdateCustomerParams struct {
	UsageBilling    []UsageBillingAccountUpdate   `json:"usage_billing,omitempty"`
	CreditLimits    []CreditLimit                 `json:"credit_limits,omitempty"`
	TrustLevels     []TrustLevel                  `json:"trust_levels,omitempty"`
	LowBalanceLines []LowBalanceLine              `json:"low_balance_lines,omitempty"`
	Limits          []LimitOverride               `json:"limits,omitempty"`
	BillingPolicy   catalog.Field[string]         `json:"billing_policy,omitzero"`
	InvoiceProfile  catalog.Field[InvoiceProfile] `json:"invoice_profile,omitzero"`
}
