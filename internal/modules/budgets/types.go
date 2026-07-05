// Package budgets holds the money-budget VOCABULARY shared by the admission
// policy loader (#513): the window shape and the scope identifiers.
//
// The Postgres window-reservation engine (budget_window_state /
// budget_inflight_holds / budget_reservations + FOR UPDATE per request) was
// removed in the #513 hard cut — all budget accounting now lives in the Redis
// spendgate (internal/modules/admission/spendgate). Only these read-mostly
// policy types remain, consumed by admission.SpendgatePolicyLoader.
package budgets

import "strings"

// BudgetWindow is one fixed money-budget window in a trust-level/budget policy:
// at most Limit (the currency's minor units) of spend per WindowSeconds. The
// loader maps this onto a spendgate.Window (reset boundaries staggered per
// payer, #337).
type BudgetWindow struct {
	// Key is a stable identifier for the window (e.g. "5h", "7d").
	Key string
	// WindowSeconds is the window length in seconds.
	WindowSeconds int64
	// Limit is the spend cap over the window, in the currency's internal precision.
	Limit int64
	// Currency is the window's policy currency (the loader FX-converts the limit to
	// the request currency when they differ); blank means the request currency.
	Currency string
}

// Invoker spend-limit scopes (#473/#517): a limit is {scope, scope_key, windows[]};
// at admit time a request (invoker, roles[]) is gated by EVERY matching scope's
// windows. These identifiers are stored in invoker_spend_limits.scope.
//
// ScopeInvokerTrustLevel's wire/stored value stays "invoker_tier" — it is
// constrained by the invoker_spend_limits.scope CHECK constraint in the DB
// schema, and renaming the stored value would require a schema migration
// (out of scope for the Go-level trust-tier -> trust-level rename).
const (
	ScopeInvoker           = "invoker"
	ScopeRole              = "role"
	ScopeInvokerTrustLevel = "invoker_tier"
)

// NormalizeScope canonicalizes a stored scope string.
func NormalizeScope(scope string) string {
	return strings.TrimSpace(scope)
}
