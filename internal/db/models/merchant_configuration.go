package models

import (
	"github.com/open-rails/openrails/billing"
)

// MerchantConfiguration is a merchant's validated settings, as readers use
// them (merchantconfig.Normalize).
type MerchantConfiguration struct {
	Profile MerchantProfileConfiguration `json:"profile,omitempty"`

	// Usage billing settings: the billing period and collection knobs.
	// Missing values use money service defaults.
	CollectionThreshold   *int64 `json:"collection_threshold,omitempty"`
	MonthlyFloor          *int64 `json:"monthly_floor,omitempty"`
	BillingPeriodBoundary string `json:"billing_period_boundary,omitempty"`

	// ArrearsGraceDays is how many days past a usage order's due_at a payer keeps
	// before its debt is delinquent. Nil ⇒ delinquency.DefaultGraceDays; zero
	// is a valid choice.
	ArrearsGraceDays *int `json:"arrears_grace_days,omitempty"`

	// ArrearsDelinquencyFloor (micros) is the smallest overdue balance that can
	// make a payer delinquent. Nil ⇒ derived from MonthlyFloor, else
	// delinquency.DefaultAmountFloor.
	ArrearsDelinquencyFloor *int64 `json:"arrears_delinquency_floor,omitempty"`

	// DelegatedInvokerWastedSpendWindows are merchant-wide abuse cutoffs for
	// delegated invokers. Missing or empty windows use the service default.
	DelegatedInvokerWastedSpendWindows []BudgetWindowPolicy `json:"delegated_invoker_wasted_spend_windows,omitempty"`

	// AlertEmail receives the merchant's critical alerts. Unset ⇒ the email
	// channel is inactive.
	AlertEmail string `json:"alert_email,omitempty"`

	// RepriceNoticeWindowDays is the minimum advance notice a subscription
	// price increase must give existing subscribers; decreases are exempt.
	// Nil ⇒ DefaultPriceIncreaseNoticeDays; zero enforces none.
	RepriceNoticeWindowDays *int `json:"reprice_notice_window_days,omitempty"`

	// ChargePSPs are the card PSPs new card charges go to, in order; nil is
	// every armed card PSP.
	ChargePSPs []string `json:"charge_psps,omitempty"`

	// ProviderRefundAccess decides what a refund made at the provider (not
	// through OpenRails) does to the refunded charge's access: revoke_on_full
	// ("" default), revoke_on_any or keep.
	ProviderRefundAccess string `json:"provider_refund_access,omitempty"`

	// DunningPolicy replaces the built-in dunning schedule. Nil ⇒
	// collection.DefaultPolicy.
	DunningPolicy *billing.DunningPolicy `json:"dunning_policy,omitempty"`

	// PSPRouting is the merchant's processor preference: ordered rules,
	// first match wins. Empty ⇒ the built-in default order.
	PSPRouting []PSPRoutingRule `json:"psp_routing,omitempty"`

	// BalanceNotices are the customers' default low-balance lines.
	BalanceNotices BalanceNoticeConfiguration `json:"balance_notices,omitempty"`
}

// BalanceNoticeConfiguration is a merchant's validated default low-balance
// lines: RunwayHours nil ⇒ the built-in 24; a currency absent from LowAmounts
// has no amount line.
type BalanceNoticeConfiguration struct {
	RunwayHours *int             `json:"runway_hours,omitempty"`
	LowAmounts  map[string]int64 `json:"low_amounts,omitempty"`
}

type PSPRoutingRule = billing.PSPRoutingRule
type PSPRoutingMatch = billing.PSPRoutingMatch

// MerchantProfileConfiguration is merchant-owned public/communication metadata.
// It is stored per merchant, not in process-wide runtime config.
type MerchantProfileConfiguration struct {
	DisplayName string `json:"display_name,omitempty"`
	LogoURL     string `json:"logo_url,omitempty"`
	FromEmail   string `json:"from_email,omitempty"`
	SupportURL  string `json:"support_url,omitempty"`
	// SubscribeURL is where new customers, and those whose access ended,
	// buy a subscription.
	SubscribeURL string `json:"subscribe_url,omitempty"`
	// BillingURL is where customers manage and cancel their subscriptions
	// and payment agreements.
	BillingURL          string `json:"billing_url,omitempty"`
	TermsURL            string `json:"terms_url,omitempty"`
	PrivacyURL          string `json:"privacy_url,omitempty"`
	StatementDescriptor string `json:"statement_descriptor,omitempty"`
}
