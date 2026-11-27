package models

import (
	"github.com/open-rails/openrails/billing"
)

// MerchantConfiguration is a merchant's validated settings, as readers use
// them (merchantconfig.Normalize).
type MerchantConfiguration struct {
	UsageBilling *billing.UsageBillingSettings `json:"usage_billing,omitempty"`
	Profile      MerchantProfileConfiguration  `json:"profile,omitempty"`

	// Usage billing settings: the billing period and collection knobs.
	// Missing values use money service defaults.
	BillingPeriodBoundary string `json:"billing_period_boundary,omitempty"`

	// ArrearsGraceDays is how many days past a usage invoice's due_at a customer keeps
	// before its debt is delinquent. Nil ⇒ delinquency.DefaultGraceDays; zero
	// is a valid choice.
	ArrearsGraceDays *int `json:"arrears_grace_days,omitempty"`

	// DelegatedInvokerWastedSpendWindows are merchant-wide abuse cutoffs for
	// delegated invokers. Missing or empty windows use the service default.
	DelegatedInvokerWastedSpendWindows []BudgetWindowPolicy `json:"delegated_invoker_wasted_spend_windows,omitempty"`

	// AlertEmail receives the merchant's critical alerts. Unset ⇒ the email
	// channel is inactive.
	AlertEmail string `json:"alert_email,omitempty"`

	// RepriceNoticeWindowDays is the minimum advance notice a price migration
	// must give a subscriber whose cost per unit of time rises; equal or lower
	// cost is exempt.
	// Nil ⇒ DefaultPriceIncreaseNoticeDays; zero enforces none.
	RepriceNoticeWindowDays *int `json:"reprice_notice_window_days,omitempty"`

	// ChargePSPs are the card PSPs new card charges go to, in order; nil is
	// every armed card PSP.
	ChargePSPs []string `json:"charge_psps,omitempty"`

	// ProviderRefundAccess decides what a refund made at the provider (not
	// through OpenRails) does to the refunded charge's access: revoke_on_full
	// ("" default), revoke_on_any or keep.
	ProviderRefundAccess billing.ProviderRefundAccess `json:"provider_refund_access,omitempty"`

	// FraudWarningRefund decides what an actionable early fraud warning does:
	// refund ("" default) or record.
	FraudWarningRefund billing.FraudWarningRefund `json:"fraud_warning_refund,omitempty"`

	// DunningPolicy replaces the built-in dunning schedule. Nil ⇒
	// collection.DefaultPolicy.
	DunningPolicy *billing.DunningPolicy `json:"dunning_policy,omitempty"`

	// PSPRouting is the merchant's processor preference: ordered rules,
	// first match wins. Empty ⇒ the built-in default order.
	PSPRouting []PSPRoutingRule `json:"psp_routing,omitempty"`

	// BalanceNotices are the customers' default low-balance lines.
	BalanceNotices BalanceNoticeConfiguration `json:"balance_notices,omitempty"`

	// AccountingTimezone is the IANA zone the merchant's books are dated in;
	// never empty (UTC by default).
	AccountingTimezone string `json:"accounting_timezone,omitempty"`

	// BillingFields are the billing details a new card needs, defaults
	// filled in.
	BillingFields billing.BillingFieldSettings `json:"billing_fields"`
}

// BalanceNoticeConfiguration is a merchant's validated default low-balance
// lines: a currency absent from LowAmounts has none.
type BalanceNoticeConfiguration struct {
	LowAmounts map[string]int64 `json:"low_amounts,omitempty"`
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
