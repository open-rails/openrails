// Package admission implements OpenRails service admission: the payer money
// affordability + delegated spend-cap gate and the delegated wasted-spend cutoff.
//
// #513 hard cut: admission is ONE atomic Redis decision (internal/modules/admission/spendgate).
// The admitter resolves the payer trust level, enforces the delegated wasted-spend
// cutoff, loads the cached cap windows, reads the O(1) ledger balance, and runs
// the single spendgate EVAL that checks affordability + every window and places
// the in-flight hold. No Postgres locks, no per-request budget reservation rows.
//
// Delegated metering is PER-INVOKER (#563): a role/trust-level grant only selects
// WHICH invokers a window applies to — it is never a pool shared across them. The
// Invoker string and role UUIDs are host-owned, opaque, stable keys; OpenRails
// never materializes a row for a delegated invoker. Only the payer scope is
// aggregate across the whole account.
package admission

import (
	"context"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/modules/abuse"
	"github.com/open-rails/openrails/internal/modules/admission/spendgate"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
)

// DefaultTrustLevel is assigned to actors with no explicit trust level — the
// new-account low default (#300): start at the lowest level until graduated.
const DefaultTrustLevel = "free"

// DenyFailureRateLimited is the delegated-invoker wasted-spend cutoff deny code.
const DenyFailureRateLimited = "failure_rate_limited"

// DenyDelegatedSpendNotAllowed denies a delegated invoker that has no explicit
// budget grant (a delegated invoker may never spend the payer's money without a
// scope=invoker/role/invoker_tier cap — #473).
const DenyDelegatedSpendNotAllowed = "delegated_spend_not_allowed"

// DenyBudgetExceeded is the deny code when a spend-cap window blocks the request.
const DenyBudgetExceeded = "budget_exceeded"

// DenyOutstandingCap is the or#897 outstanding_cap refusal: LEDGER-measured
// unpaid arrears have reached the payer's credit line. Distinct from
// insufficient_credit (which is "this one request does not fit") and from
// delinquent (the TIME axis) — this is "your debt is at the cap".
const DenyOutstandingCap = "outstanding_cap_reached"

// DenyDelinquent is the or#878 TIME-axis refusal: the payer has a debt that has
// outlived the merchant's grace window. Deliberately DISTINCT from
// insufficient_credit — "you are over your limit" and "you have an unpaid
// invoice" call for different things from the customer, and a host that cannot
// tell them apart cannot say anything useful to either.
const DenyDelinquent = "delinquent_unpaid_invoice"

// Admitter is the service-admit money gate.
type Admitter struct {
	money  *money.MoneyService
	gate   *spendgate.Gate
	loader *SpendgatePolicyLoader

	// delinquency is the optional or#878 arrears delinquency gate; nil disables
	// it (the amount cap still applies — the two axes are independent).
	delinquency DelinquencyGate

	// wasted is the optional $-valued delegated-invoker wasted-spend cutoff (#497);
	// nil disables it. invokerWastedWindows is the flat per-invoker backstop.
	wasted               *abuse.WastedSpendGuard
	invokerWastedWindows []abuse.WastedWindow

	// denials is the optional #733 denial counter (Redis hourly aggregates);
	// nil disables recording.
	denials *DenialRecorder
}

// NewAdmitter builds the admitter over the Redis gate + the Postgres→policy loader.
func NewAdmitter(moneySvc *money.MoneyService, gate *spendgate.Gate, loader *SpendgatePolicyLoader) *Admitter {
	return &Admitter{money: moneySvc, gate: gate, loader: loader}
}

// DelinquencyGate answers whether a payer's arrears debt has outlived the
// merchant's grace window. An interface so admission depends on the QUESTION,
// not on the delinquency module's storage.
type DelinquencyGate interface {
	IsDelinquent(ctx context.Context, payer identity.CustomerID, currency string) (bool, error)
}

// WithDelinquency enables the or#878 arrears delinquency admit gate.
func (a *Admitter) WithDelinquency(g DelinquencyGate) *Admitter {
	a.delinquency = g
	return a
}

// WithWastedSpend enables the delegated-invoker wasted-spend admit gate (#497).
func (a *Admitter) WithWastedSpend(guard *abuse.WastedSpendGuard, invokerWindows []abuse.WastedWindow) *Admitter {
	a.wasted = guard
	a.invokerWastedWindows = invokerWindows
	return a
}

// WithDenialRecorder enables aggregated denial counting (#733).
func (a *Admitter) WithDenialRecorder(r *DenialRecorder) *Admitter {
	a.denials = r
	return a
}

func (a *Admitter) recordDenial(ctx context.Context, merchantID string, customer identity.CustomerID, code string) {
	if a.denials != nil {
		a.denials.Record(ctx, merchantID, customer.UUID().String(), code, time.Now())
	}
}

// AdmitRequest is one admission decision input.
type AdmitRequest struct {
	CustomerID  identity.CustomerID // the merchant subject
	Invoker     string              // canonical invoker: user:<id> / apiKey:<key_id> / <issuer>:<sub>
	InvokerType string              // "payer" for direct payer credential; empty/other = delegated
	TrustLevel  string              // optional payer trust level; empty resolves from OpenRails money state
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
	// RetryAfterSeconds is when the blocking budget window next resets (0 unless
	// a window was the binding gate). Hosts stamp it as the 429 Retry-After.
	RetryAfterSeconds int64
}

// Admit resolves the trust level, enforces the delegated wasted-spend cutoff,
// then runs the single spendgate EVAL (affordability + spend-cap windows + hold
// placement).
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

	// Trust level resolution: explicit > graduated (#298) > lowest default (#300).
	trustLevel := req.TrustLevel
	if trustLevel == "" {
		if t, terr := a.money.GetTrustLevel(ctx, req.CustomerID, req.Currency); terr == nil && t != "" {
			trustLevel = t
		}
		if trustLevel == "" {
			trustLevel = DefaultTrustLevel
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
			a.recordDenial(ctx, merchantID, req.CustomerID, DenyFailureRateLimited)
			return AdmitDecision{Allowed: false, BlockedBy: "abuse", DenyCode: DenyFailureRateLimited}, nil
		}
	}

	// or#878, the TIME axis. The amount cap below asks "can this payer afford
	// one more request"; this asks "has this payer's existing debt outlived the
	// merchant's grace window". A payer can pass either and fail the other, so
	// both gates run and each has its own deny code.
	//
	// This is the ONE enforcement lever OpenRails is authoritative over, and for
	// usage billing it is the meaningful cutoff: refusing new spend is what stops
	// the bill growing. It revokes nothing and cancels nothing — whatever the
	// host is already running is the host's to shut off, told by the durable
	// delinquency signal.
	//
	// Fails OPEN by construction: the gate below only refuses on a recorded
	// transition that a live re-read of the invoices still agrees with.
	if a.delinquency != nil {
		delinquent, derr := a.delinquency.IsDelinquent(ctx, req.CustomerID, req.Currency)
		if derr != nil {
			return AdmitDecision{}, derr
		}
		if delinquent {
			a.recordDenial(ctx, merchantID, req.CustomerID, DenyDelinquent)
			return AdmitDecision{Allowed: false, BlockedBy: "money", DenyCode: DenyDelinquent}, nil
		}
	}

	sgReq := spendgate.Request{Invoker: req.Invoker, TrustLevel: trustLevel, Roles: roleStrings(req.Roles), Measure: req.Resource}

	// or#897: the merchant's bound billing policy is resolved FIRST, because its
	// KIND decides whether prior debt reduces this payer's headroom at all. The
	// merchant chose which policy binds to this payer; OpenRails measures and
	// enforces it. Served from the process-local cache when warm.
	resolved, err := a.loader.ResolvePolicy(ctx, req.CustomerID, trustLevel)
	if err != nil {
		return AdmitDecision{}, err
	}

	// Cap windows (FX-normalized to the request currency).
	policy, hasGrant, err := a.loader.Load(ctx, req.CustomerID, trustLevel, req.Currency, sgReq, resolved)
	if err != nil {
		return AdmitDecision{}, err
	}

	// A delegated invoker must hold an explicit spend grant (#473 guarantee).
	if !identity.IsDirectPayerInvoker(req.InvokerType) && !hasGrant {
		a.recordDenial(ctx, merchantID, req.CustomerID, DenyDelegatedSpendNotAllowed)
		return AdmitDecision{Allowed: false, BlockedBy: "budget", DenyCode: DenyDelegatedSpendNotAllowed}, nil
	}

	// Unlocked balance + arrears credit line (the gate's affordability inputs).
	// Phase H makes the settled balance an O(1) ledger account read, so this stays
	// direct and avoids a staleness window.
	available, creditLine, outstanding, err := payerCapacity(ctx, a.money, req.CustomerID, req.Currency, resolved)
	if err != nil {
		return AdmitDecision{}, err
	}

	// or#897 outstanding cap: unpaid arrears have consumed the whole credit line,
	// so nothing more may be spent on credit. Distinct from the affordability
	// refusal below — a host must be able to tell "your debt is at the cap, pay
	// it down" from "you are out of prepaid balance". A window_spend_cap payer
	// never reaches this: its line does not move with debt, so there is no cap
	// to reach and unpaid invoices are the delinquency gate's business.
	if resolved.GatesOnOutstandingOwed() && creditLine == 0 && available <= 0 && outstanding > 0 {
		a.recordDenial(ctx, merchantID, req.CustomerID, DenyOutstandingCap)
		return AdmitDecision{Allowed: false, BlockedBy: "money", DenyCode: DenyOutstandingCap}, nil
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
		a.recordDenial(ctx, merchantID, req.CustomerID, code)
		return AdmitDecision{Allowed: false, BlockedBy: "money", DenyCode: code, AvailableAmount: available}, nil
	default: // window blocked
		a.recordDenial(ctx, merchantID, req.CustomerID, DenyBudgetExceeded)
		return AdmitDecision{Allowed: false, BlockedBy: "budget", DenyCode: DenyBudgetExceeded, AvailableAmount: available,
			RetryAfterSeconds: int64(math.Ceil(dec.RetryAfter.Seconds()))}, nil
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
	if err := moneyutil.ValidateCurrency(cur); err != nil {
		return "", err
	}
	explicit := false
	for _, w := range windows {
		if w.Currency == "" {
			continue
		}
		wc := money.NormalizeCurrency(w.Currency)
		if err := moneyutil.ValidateCurrency(wc); err != nil {
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
