package billing

import "github.com/open-rails/openrails/catalog"

// PSPRoutingRule is one processor-preference rule. Rules are evaluated in
// declaration order and the first whose Match accepts the routing inputs wins.
// Prefer is that rule's ranked candidate list and its whitelist: a PSP the
// winning rule does not name is not eligible, so a rule can constrain a
// product to one rail.
type PSPRoutingRule struct {
	// Match is the purchases the rule applies to; empty matches every one.
	Match PSPRoutingMatch `json:"match,omitempty"`
	// Prefer are purchase selectors in preference order: PSP keys ("mobius"),
	// or a rail kind, which resolves to that rail's one armed PSP.
	Prefer []string `json:"prefer"`
}

// PSPRoutingMatch is a rule's condition. Every SET field must match the
// routing inputs; an all-empty match accepts everything (the catch-all rule).
type PSPRoutingMatch struct {
	Currency catalog.Currency `json:"currency,omitempty"` // Registered currency code, uppercase (matched case-insensitively)
	Product  string           `json:"product,omitempty"`  // product key
	Price    string           `json:"price,omitempty"`    // price key
	Mode     string           `json:"mode,omitempty"`     // one_off | subscription
	Country  string           `json:"country,omitempty"`  // unsupported; nonempty values are rejected
}

// IsCatchAll reports a match with no conditions — it accepts every input, so
// no later rule can ever be reached.
func (m PSPRoutingMatch) IsCatchAll() bool {
	return m.Currency == "" && m.Product == "" && m.Price == "" && m.Mode == "" && m.Country == ""
}

// ProviderRefundAccess is what a refund made in the provider's own dashboard
// does to access.
type ProviderRefundAccess string

const (
	ProviderRefundRevokeOnFull ProviderRefundAccess = "revoke_on_full"
	ProviderRefundRevokeOnAny  ProviderRefundAccess = "revoke_on_any"
	ProviderRefundKeep         ProviderRefundAccess = "keep"
)

// MerchantCurrency is a unit the merchant defines (credits, API tokens, an
// in-game currency): a balance its customers hold, never money. Code is four
// to twelve upper-case letters or digits starting with a letter, never a
// currency in OpenRails' registry; three-letter codes are ISO 4217's. Name is
// how it reads ("Gold coins"); Symbol is its display mark, without a currency
// sign, the code when omitted. An amount in it is an integer count of
// 10^-Scale units: Scale 0 (the default) counts whole units, at most 6.
// Each merchant's units are its own: another merchant does not know them.
type MerchantCurrency struct {
	// Code is the unit's code: up to twelve upper-case letters or digits, starting with a letter.
	Code catalog.Currency `json:"code"`
	// Name is how it reads ("Gold coins").
	Name string `json:"name"`
	// Symbol is its display mark; the code when omitted.
	Symbol string `json:"symbol,omitempty"`
	// Scale is its decimal places: 0 counts whole units, at most 6.
	Scale int `json:"scale"`
}

// FraudWarningRefund is what an actionable early fraud warning (an issuer's
// fraud report on a charge not yet disputed) does.
type FraudWarningRefund string

const (
	// FraudWarningRefundCharge refunds the charge in full, ends what it paid
	// for and blocks its card, as a chargeback would: about 40% of reports
	// become disputes otherwise. The default.
	FraudWarningRefundCharge FraudWarningRefund = "refund"
	// FraudWarningRecord records the report only.
	FraudWarningRecord FraudWarningRefund = "record"
)
