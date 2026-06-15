package models

// MerchantConfiguration is the JSONB payload stored in
// openrails.merchant_configurations.
type MerchantConfiguration struct {
	// DelegatedInvokerWastedSpendWindows are merchant-wide abuse cutoffs for
	// delegated invokers. Missing or empty windows use the service default.
	DelegatedInvokerWastedSpendWindows []BudgetWindowPolicy `json:"delegated_invoker_wasted_spend_windows,omitempty"`
}
