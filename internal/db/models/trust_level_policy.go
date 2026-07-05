package models

import (
	"time"

	"github.com/google/uuid"
)

// TrustLevelPolicy is a per-payer, per-trust-level admission policy for rolling
// money-budget windows and wasted-spend grace.
type TrustLevelPolicy struct {
	ID         uuid.UUID `json:"id"`
	MerchantID uuid.UUID `json:"merchant_id"`
	// CustomerID the policy belongs to. The all-zero uuid is the reserved sentinel
	// for merchant-wide defaults; a real payer id scopes policy to that payer.
	CustomerID uuid.UUID `json:"customer_id"`
	// TrustLevel name (e.g. "free", "trust_1"); the policy applies to actors at this level.
	TrustLevel string `json:"trust_level"`
	// Policy is the money policy JSON.
	Policy        TrustLevelMoneyPolicy `json:"policy"`
	PolicyVersion int64                 `json:"policy_version"`
	CreatedAt     time.Time             `json:"created_at"`
	UpdatedAt     time.Time             `json:"updated_at"`
}

// TrustLevelMoneyPolicy is the JSONB-stored trust-level money policy: the
// per-trust-level $-budget windows and wasted-spend grace. (It carries no
// throughput/concurrency axis — fleet scheduling/fairness is owned by the
// consumer's scheduler, not OpenRails.)
type TrustLevelMoneyPolicy struct {
	BudgetWindows []BudgetWindowPolicy `json:"budget_windows,omitempty"`
	// PolicyCurrency is the currency used for money policy checks whose window
	// does not carry its own Currency. Blank means same currency as the request.
	PolicyCurrency string `json:"policy_currency,omitempty"`

	// BadSpendWindows are $-VALUED wasted/failed-spend grace windows for this
	// trust level (#497): at most Limit of direct-payer wasted spend is forgiven
	// per WindowSeconds. Overage is charged at report time. Delegated invokers use
	// the separate flat invoker cutoff policy.
	BadSpendWindows []BudgetWindowPolicy `json:"bad_spend_windows,omitempty"`
}

// BudgetWindowPolicy is one rolling money-budget window for a trust level
// (#304): at most Limit (the ledger's smallest unit) of spend per WindowSeconds.
type BudgetWindowPolicy struct {
	Key           string `json:"key"`
	WindowSeconds int64  `json:"window_seconds"`
	Limit         int64  `json:"limit"`
	Currency      string `json:"currency,omitempty"`
}
