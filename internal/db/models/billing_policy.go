package models

// BillingPolicy is a declared policy's body in the merchant's settings.
//
// Every quantity is measured from the double-entry ledger — never from invoices
// (presentation artifacts) and never from merchant-supplied numbers. The
// merchant declares the policy and binds it; OpenRails measures and enforces.
// Its caps are independent: a policy sets any of them.
type BillingPolicy struct {
	// OutstandingCapAmount is the credit line on unpaid arrears, in micros, of
	// a bound customer without its own credit limit; what it owes is taken
	// from it. Zero defers to the merchant's usage terms.
	OutstandingCapAmount int64 `json:"outstanding_cap_amount,omitempty"`

	// SpendWindows are the rolling new-spend ceilings: at most Limit of spend
	// per WindowSeconds.
	SpendWindows []BudgetWindowPolicy `json:"spend_windows,omitempty"`

	// BadSpendWindows are wasted/failed-spend grace windows for the direct
	// payer: at most Limit of wasted spend is forgiven per window, and overage
	// is charged at report time.
	BadSpendWindows []BudgetWindowPolicy `json:"bad_spend_windows,omitempty"`

	// CollectionThresholdAmount (micros) is when this payer's accrued arrears is
	// invoiced. Overrides its currency's usage_billing collection_threshold for payers
	// bound to this policy; unset defers to it.
	CollectionThresholdAmount *int64 `json:"collection_threshold_amount,omitempty"`

	// CollectionCycleBoundary is declarable and refused: a payer's statement
	// periods must tile its lifetime, and rebinding a policy mid-cycle would
	// bill a stretch twice or never. It stays merchant-wide
	// (invoice.billing_period_boundary).
	CollectionCycleBoundary string `json:"collection_cycle_boundary,omitempty"`

	// DelinquencyGraceDays / DelinquencyAmountFloor override the merchant's
	// arrears_grace_days / its currency's delinquency_floor for bound payers;
	// unset defers to them. DelinquencyAmountFloor is in PolicyCurrency and
	// applies to debt in it.
	DelinquencyGraceDays   *int   `json:"delinquency_grace_days,omitempty"`
	DelinquencyAmountFloor *int64 `json:"delinquency_amount_floor,omitempty"`

	// PolicyCurrency is the currency of the policy's amounts and of a window
	// that carries none. Blank means the request's currency.
	PolicyCurrency string `json:"policy_currency,omitempty"`
}
