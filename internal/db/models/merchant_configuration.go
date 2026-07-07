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
