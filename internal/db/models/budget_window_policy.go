package models

// BudgetWindowPolicy is one rolling money-budget window: at most Limit (the
// ledger's smallest unit) of spend per WindowSeconds. Used by billing
// policies, invoker spend limits and delegated-invoker wasted-spend windows.
type BudgetWindowPolicy struct {
	Key           string `json:"key"`
	WindowSeconds int64  `json:"window_seconds"`
	Limit         int64  `json:"limit"`
	Currency      string `json:"currency,omitempty"`
}
