package models

// BudgetWindowPolicy is one rolling money-budget window: at most Limit (the
// ledger's smallest unit) of spend per WindowSeconds. Used by billing policies
// (or#897 spend/bad-spend windows), invoker spend limits (#473) and the
// merchant-wide delegated-invoker wasted-spend windows (#646).
type BudgetWindowPolicy struct {
	Key           string `json:"key"`
	WindowSeconds int64  `json:"window_seconds"`
	Limit         int64  `json:"limit"`
	Currency      string `json:"currency,omitempty"`
}
