// Package admission implements OpenRails service admission: the payer money
// affordability + delegated spend-cap gate and the delegated wasted-spend cutoff.
//
// #513 hard cut: admission is ONE atomic Redis decision (internal/modules/admission/spendgate).
// The admitter resolves the payer trust tier, enforces the delegated wasted-spend
// cutoff, loads the cached cap windows, reads the O(1) ledger balance, and runs
// the single spendgate EVAL that checks affordability + every window and places
// the in-flight hold. No Postgres locks, no per-request budget reservation rows.
package admission

import (
	"time"

	"context"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/modules/abuse"
	"github.com/open-rails/openrails/internal/modules/admission/spendgate"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
)

// DefaultTier is assigned to actors with no explicit tier — the new-account low
// default (#300): start at the lowest tier until graduated.
const DefaultTier = "free"

// DenyFailureRateLimited is the delegated-invoker wasted-spend cutoff deny code.
const DenyFailureRateLimited = "failure_rate_limited"

// DenyDelegatedSpendNotAllowed denies a delegated invoker that has no explicit
// budget grant (a delegated invoker may never spend the payer's money without a
// scope=invoker/role/invoker_tier cap — #473).
const DenyDelegatedSpendNotAllowed = "delegated_spend_not_allowed"

// DenyBudgetExceeded is the deny code when a spend-cap window blocks the request.
const DenyBudgetExceeded = "budget_exceeded"

// Admitter is the service-admit money gate.
type Admitter struct {
	money  *money.MoneyService
	gate   *spendgate.Gate
	loader *SpendgatePolicyLoader

	// wasted is the optional $-valued delegated-invoker wasted-spend cutoff (#497);
	// nil disables it. invokerWastedWindows is the flat per-invoker backstop.
	wasted               *abuse.WastedSpendGuard
	invokerWastedWindows []abuse.WastedWindow
}

// NewAdmitter builds the admitter over the Redis gate + the Postgres→policy loader.
func NewAdmitter(moneySvc *money.MoneyService, gate *spendgate.Gate, loader *SpendgatePolicyLoader) *Admitter {
	return &Admitter{money: moneySvc, gate: gate, loader: loader}
}

// WithWastedSpend enables the delegated-invoker wasted-spend admit gate (#497).
func (a *Admitter) WithWastedSpend(guard *abuse.WastedSpendGuard, invokerWindows []abuse.WastedWindow) *Admitter {
	a.wasted = guard
	a.invokerWastedWindows = invokerWindows
	return a
}

// AdmitRequest is one admission decision input.
type AdmitRequest struct {
	CustomerID  identity.CustomerID // the merchant subject
	Invoker     string              // canonical invoker: user:<id> / serviceToken:<key_id> / <issuer>:<sub>
	InvokerType string              // "payer" for direct payer credential; empty/other = delegated
	Tier        string              // optional payer trust tier; empty resolves from OpenRails money state
	Resource    string              // caller-supplied resource string for host-side attribution only
	Roles       []uuid.UUID         // immutable role UUIDs the invoker holds (#473)

	Currency        string
	EstimatedAmount int64
	Source          string    // idempotency namespace (e.g. "usage")
	SourceID        string    // idempotency id (request id) — the hold key
	ExpiresAt       time.Time // hold expiry
}

// AdmitDecision is the unified outcome.
type AdmitDecision struct {
	Allowed   bool
	BlockedBy string // "budget" | "abuse" | "money" | ""
	DenyCode  string
	// AvailableAmount is the payer's spendable balance (settled − money-window
	// reservations) the affordability gate was evaluated against; HeldAmount is the
	// total in-flight reservation after this admit. StartCapacity = Available − Held.
	AvailableAmount int64
	HeldAmount      int64
}

// Admit resolves the tier, enforces the delegated wasted-spend cutoff, then runs
// the single spendgate EVAL (affordability + spend-cap windows + hold placement).
func (a *Admitter) Admit(ctx context.Context, req AdmitRequest) (AdmitDecision, error) {
	tid, err := merchant.Require(ctx) // #336: no default merchant
	if err != nil {
		return AdmitDecision{}, err
	}
	merchantID := tid.UUID().String()

	// No money axis → nothing to gate (admission has no non-money axis post-#513).
	if req.EstimatedAmount <= 0 {
		return AdmitDecision{Allowed: true}, nil
	}

	// Tier resolution: explicit > graduated (#298) > lowest default (#300).
	tier := req.Tier
	if tier == "" {
		if t, terr := a.money.GetTier(ctx, req.CustomerID, req.Currency); terr == nil && t != "" {
			tier = t
		}
		if tier == "" {
			tier = DefaultTier
		}
	}

	// Delegated-invoker wasted-spend cutoff (#497): direct payer credentials are
	// not cut off here (their over-grace waste is charged at report time).
	if a.wasted != nil && a.wasted.Enabled() &&
		!identity.IsDirectPayerInvoker(req.InvokerType) && req.Invoker != "" && len(a.invokerWastedWindows) > 0 {
		wastedCurrency, werr := effectiveWastedCurrency(req.Currency, a.invokerWastedWindows)
		if werr != nil {
			return AdmitDecision{}, werr
		}
		over, _, werr := a.wasted.InvokerOverBudget(ctx, merchantID, req.CustomerID.UUID().String(), req.Invoker, wastedCurrency, a.invokerWastedWindows)
		if werr != nil {
			return AdmitDecision{}, werr
		}
		if over {
			return AdmitDecision{Allowed: false, BlockedBy: "abuse", DenyCode: DenyFailureRateLimited}, nil
		}
	}

	sgReq := spendgate.Request{Invoker: req.Invoker, Tier: tier, Roles: roleStrings(req.Roles)}

	// Cached cap windows (FX-normalized to the request currency).
	policy, hasGrant, err := a.loader.Load(ctx, req.CustomerID, tier, req.Currency, sgReq)
	if err != nil {
		return AdmitDecision{}, err
	}

	// A delegated invoker must hold an explicit spend grant (#473 guarantee).
	if !identity.IsDirectPayerInvoker(req.InvokerType) && !hasGrant {
		return AdmitDecision{Allowed: false, BlockedBy: "budget", DenyCode: DenyDelegatedSpendNotAllowed}, nil
	}

	// Unlocked balance + arrears credit line (the gate's affordability inputs).
	// Phase H makes the settled balance an O(1) ledger account read, so this stays
	// direct and avoids a staleness window.
	available, creditLine, err := payerCapacity(ctx, a.money, req.CustomerID, req.Currency)
	if err != nil {
		return AdmitDecision{}, err
	}

	dec, err := a.gate.Admit(ctx, spendgate.AdmitInput{
		Merchant:       merchantID,
		Customer:       req.CustomerID.UUID().String(),
		Currency:       req.Currency,
		RequestID:      req.SourceID,
		Invoker:        req.Invoker,
		Source:         req.Source,
		Cost:           req.EstimatedAmount,
		AccountBalance: available,
		CreditLimit:    creditLine,
		HoldTTL:        holdTTL(req.ExpiresAt),
		Policy:         policy,
		Request:        sgReq,
	})
	if err != nil {
		return AdmitDecision{}, err
	}

	switch {
	case dec.Allowed:
		held, herr := a.gate.HeldAmount(ctx, merchantID, req.CustomerID.UUID().String(), req.Currency)
		if herr != nil {
			return AdmitDecision{}, herr
		}
		return AdmitDecision{Allowed: true, AvailableAmount: available, HeldAmount: held}, nil
	case dec.BlockedBalance:
		code := money.DenyInsufficientBalance
		if creditLine > 0 {
			code = money.DenyInsufficientCredit
		}
		return AdmitDecision{Allowed: false, BlockedBy: "money", DenyCode: code, AvailableAmount: available}, nil
	default: // window blocked
		return AdmitDecision{Allowed: false, BlockedBy: "budget", DenyCode: DenyBudgetExceeded, AvailableAmount: available}, nil
	}
}

// roleStrings maps the invoker's role UUIDs to the strings the spendgate role
// scope matches on (the invoker_spend_limits role scope key is the role uuid string).
func roleStrings(roles []uuid.UUID) []string {
	out := make([]string, 0, len(roles))
	for _, r := range roles {
		if r != uuid.Nil {
			out = append(out, r.String())
		}
	}
	return out
}

// holdTTL bounds an abandoned hold: the caller's expiry when set, else 1h.
func holdTTL(expiresAt time.Time) time.Duration {
	if expiresAt.IsZero() {
		return time.Hour
	}
	if d := time.Until(expiresAt); d > 0 {
		return d
	}
	return time.Hour
}

// effectiveWastedCurrency validates the wasted-spend windows resolve to one
// currency (cross-currency wasted policies are unsupported in one policy).
func effectiveWastedCurrency(requestCurrency string, windows []abuse.WastedWindow) (string, error) {
	cur := money.NormalizeCurrency(requestCurrency)
	if err := money.ValidateCurrency(cur); err != nil {
		return "", err
	}
	explicit := false
	for _, w := range windows {
		if w.Currency == "" {
			continue
		}
		wc := money.NormalizeCurrency(w.Currency)
		if err := money.ValidateCurrency(wc); err != nil {
			return "", err
		}
		if !explicit {
			cur = wc
			explicit = true
			continue
		}
		if wc != cur {
			return "", errMixedWastedCurrency(cur, wc)
		}
	}
	return cur, nil
}

func errMixedWastedCurrency(a, b string) error {
	return &mixedCurrencyError{a: a, b: b}
}

type mixedCurrencyError struct{ a, b string }

func (e *mixedCurrencyError) Error() string {
	return "mixed wasted-spend currencies are not supported in one policy: " + e.a + " and " + e.b
}
