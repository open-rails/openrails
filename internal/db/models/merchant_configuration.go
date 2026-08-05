package models

// MerchantConfiguration is the JSONB payload stored in
// openrails.merchant_configurations.
type MerchantConfiguration struct {
	Profile MerchantProfileConfiguration `json:"profile,omitempty"`

	// Invoice settings are merchant-owned billing cadence and collection knobs.
	// Missing values use money service defaults.
	InvoiceCollectionThreshold *int64 `json:"collection_threshold,omitempty"`
	InvoiceMonthlyFloor        *int64 `json:"monthly_floor,omitempty"`
	InvoiceBillingBoundary     string `json:"billing_period_boundary,omitempty"`

	// ArrearsGraceDays (or#878) is how many days past an invoice's due_at a payer
	// keeps grace before the debt is called DELINQUENT. Business policy, so it is
	// the merchant's: a cloud vendor may give 7 days where a SaaS gives 30. Nil ⇒
	// delinquency.DefaultGraceDays (14). Zero is a valid explicit choice.
	ArrearsGraceDays *int `json:"arrears_grace_days,omitempty"`

	// ArrearsDelinquencyFloor (or#878, micros) is the smallest overdue balance
	// that can escalate a payer to delinquent. Nil ⇒ DERIVED from
	// InvoiceMonthlyFloor — a debt already declared too small to collect is too
	// small to cut anyone off for — falling back to
	// delinquency.DefaultAmountFloor.
	ArrearsDelinquencyFloor *int64 `json:"arrears_delinquency_floor,omitempty"`

	// DelegatedInvokerWastedSpendWindows are merchant-wide abuse cutoffs for
	// delegated invokers. Missing or empty windows use the service default.
	DelegatedInvokerWastedSpendWindows []BudgetWindowPolicy `json:"delegated_invoker_wasted_spend_windows,omitempty"`

	// AlertEmail is the merchant-operator address the #736 alerting engine sends
	// critical alerts to. Unset ⇒ the email channel is inactive (fail-soft skip).
	AlertEmail string `json:"alert_email,omitempty"`

	// RepriceNoticeWindowDays (#781) is the minimum number of days' advance
	// notice a subscription price INCREASE's effective_at must give existing
	// subscribers. Decreases are exempt. Nil ⇒ DefaultRepriceNoticeWindowDays
	// (30). Zero is a valid explicit merchant choice (no minimum enforced).
	RepriceNoticeWindowDays *int `json:"reprice_notice_window_days,omitempty"`

	// CheckoutRouting (or#288) is the merchant's deterministic processor
	// preference policy: ordered rules, first match wins. Empty ⇒ the built-in
	// default order.
	CheckoutRouting []CheckoutRoutingRule `json:"checkout_routing,omitempty"`
}

// CheckoutRoutingRule is one processor-preference rule (or#288). Rules are
// evaluated in declaration order and the FIRST whose Match accepts the routing
// inputs wins — no scoring, no second pass. Prefer is that rule's ranked
// candidate list AND its whitelist: a PSP the winning rule does not name is not
// eligible, so a rule can constrain a product to one rail.
type CheckoutRoutingRule struct {
	Match CheckoutRoutingMatch `json:"match,omitempty"`
	// Prefer are checkout selectors in preference order — PSP keys ("mobius"),
	// or a rail kind where the #848 wire accepts one (exactly one armed PSP).
	Prefer []string `json:"prefer"`
}

// CheckoutRoutingMatch is a rule's condition. Every SET field must match the
// routing inputs; an all-empty match accepts everything (the catch-all rule).
type CheckoutRoutingMatch struct {
	Currency string `json:"currency,omitempty"` // ISO-4217, lowercase
	Product  string `json:"product,omitempty"`  // product key
	Price    string `json:"price,omitempty"`    // price key
	Mode     string `json:"mode,omitempty"`     // one_off | subscription
	Country  string `json:"country,omitempty"`  // payer country, ISO-3166-1 alpha-2
}

// IsCatchAll reports a match with no conditions — it accepts every input, so
// no later rule can ever be reached.
func (m CheckoutRoutingMatch) IsCatchAll() bool {
	return m.Currency == "" && m.Product == "" && m.Price == "" && m.Mode == "" && m.Country == ""
}

// MerchantProfileConfiguration is merchant-owned public/communication metadata.
// It is stored per merchant, not in process-wide runtime config.
type MerchantProfileConfiguration struct {
	DisplayName string `json:"display_name,omitempty"`
	LogoURL     string `json:"logo_url,omitempty"`
	FromEmail   string `json:"from_email,omitempty"`
	SupportURL  string `json:"support_url,omitempty"`
	// SignupURL (#789) is the winback/signup page access-ended and expiry
	// emails link to. "" ⇒ no CTA rendered.
	SignupURL string `json:"signup_url,omitempty"`
}
