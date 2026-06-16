package admission

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/fx"
	"github.com/open-rails/openrails/internal/modules/abuse"
	"github.com/open-rails/openrails/internal/modules/budgets"
	"github.com/open-rails/openrails/internal/modules/holds"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/ratelimit"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Admitter is the unified admission check (issue #298): throughput (Redis) +
// money (ledger) in one decision. Throughput is evaluated first (one cheap Redis
// op); per the OpenAI model a counted request still counts even if the money gate
// then denies. Money is checked via the existing AuthorizeAndHold (which reserves
// the estimate). Deny on whichever axis blocks first.
// DefaultTier is assigned to actors with no explicit tier — the new-account low
// default (#300): start at the lowest tier until graduated.
const DefaultTier = "free"

// Generic $-denominated admit deny codes (#487).
const (
	// DenySingleChargeCap: the request's EstimatedAmount exceeds the tier's
	// max_single_charge_amount per-charge ceiling.
	DenySingleChargeCap = "single_charge_cap_exceeded"
	// DenyConcurrentHeldCap: placing this hold would push the payer's active
	// (un-settled) hold $ sum past the tier's max_concurrent_held_amount.
	DenyConcurrentHeldCap = "concurrent_held_cap_exceeded"
)

// Wasted-spend deny codes (#488/#497). Payer bad_spend is no longer an admit
// cutoff; direct-payer overage is charged at report time. The remaining
// admit-time wasted-spend deny is the delegated invoker flat cutoff.
const DenyFailureRateLimited = "failure_rate_limited"

type Admitter struct {
	limiter      *ratelimit.Limiter
	money        *money.MoneyService
	policies     *TierPolicyStore
	blocklist    *abuse.BlocklistService // optional; nil disables blocklist checks
	budgets      *budgets.Service        // optional; nil disables fixed money-budget windows (#304, #337)
	budgetScopes *BudgetPolicyStore      // optional; nil disables hierarchical budget scopes (#473)
	holds        *holds.Store            // optional; nil disables Redis hold occupancy in the tier cap
	fxProvider   fx.Provider             // optional; required for cross-currency policy enforcement

	// wasted is the optional $-valued wasted-spend guard (#488); nil disables the
	// wasted-spend admit gate. invokerWastedWindows is the FLAT per-invoker budget
	// default (invokers aren't trusted, so their budget isn't tier-graduated).
	wasted               *abuse.WastedSpendGuard
	invokerWastedWindows []abuse.WastedWindow
}

func NewAdmitter(limiter *ratelimit.Limiter, moneySvc *money.MoneyService, policies *TierPolicyStore, blocklist *abuse.BlocklistService, budgetSvc *budgets.Service) *Admitter {
	return &Admitter{limiter: limiter, money: moneySvc, policies: policies, blocklist: blocklist, budgets: budgetSvc}
}

// WithBudgetScopes enables hierarchical budget-scope composition (#473): the
// platform (subject) cap, the subject's self (subject) cap, every (subject,role)
// cap the invoker holds, plus the (subject,invoker) cap — all reserved in one
// verdict. Nil store leaves only the pre-#473 (subject,invoker) tier-policy path.
func (a *Admitter) WithBudgetScopes(store *BudgetPolicyStore) *Admitter {
	a.budgetScopes = store
	return a
}

func (a *Admitter) WithHolds(store *holds.Store) *Admitter {
	a.holds = store
	return a
}

func (a *Admitter) WithFXProvider(provider fx.Provider) *Admitter {
	a.fxProvider = provider
	return a
}

// WithWastedSpend enables the delegated-invoker wasted-spend admit gate (#497):
// deny delegated invokers when they are over the flat per-invoker wasted budget.
// Direct payer credentials are not cut off here; their wasted-spend reports use
// payer grace then normal ledger charging.
func (a *Admitter) WithWastedSpend(guard *abuse.WastedSpendGuard, invokerWindows []abuse.WastedWindow) *Admitter {
	a.wasted = guard
	a.invokerWastedWindows = invokerWindows
	return a
}

// BlockCheck is one (kind,value) tested against the payment blocklist (#300),
// e.g. {"card_fingerprint","abc"} or {"ip","1.2.3.4"}.
type BlockCheck struct {
	Kind  string
	Value string
}

// AdmitRequest is one admission decision input.
type AdmitRequest struct {
	CustomerID  identity.CustomerID // the merchant subject
	Invoker     string              // canonical invoker: user:<id> / serviceToken:<key_id> / <issuer>:<sub>
	InvokerType string              // "payer" for direct payer credential; empty/other = delegated
	Tier        string              // the invoker's tier (selects the throughput policy)
	Resource    string              // caller-supplied resource string (namespaces the throughput counters)

	// Roles are the immutable role UUIDs the invoker holds (#473). The host reads
	// them from the delegated JWT/permission set. Each role with a matching
	// (subject, role) budget policy gates this request's spend.
	Roles []uuid.UUID

	// Amounts is the throughput consumption for this request, e.g.
	// {"request":1,"token":150}.
	Amounts map[string]int64

	// Money axis (skipped when EstimatedAmount == 0). Amount precision is implied by Currency.
	Currency        string
	EstimatedAmount int64
	Source          string    // idempotency namespace (e.g. "usage")
	SourceID        string    // idempotency id (e.g. request id)
	ExpiresAt       time.Time // hold expiry

	// BlockChecks (optional) are payment identifiers to test against the #300
	// blocklist (card fingerprint, processor customer, email, ip).
	BlockChecks []BlockCheck
}

// AdmitDecision is the unified outcome.
type AdmitDecision struct {
	Allowed        bool
	BlockedBy      string // "throughput" | "money" | ""
	BlockedUnit    string // throughput: the window unit that blocked
	DenyCode       string // money: the ledger deny code
	RetryAfter     time.Duration
	Windows        []ratelimit.WindowInfo // for x-ratelimit-* headers
	CapacityAmount int64                  // available balance + allowed owed amount before Redis holds

	// BudgetReservationID is the (subject,invoker)-scope money-budget reservation
	// placed when allowed (#304, kept for back-compat); BudgetWindows is the
	// flattened per-window state for introspection.
	BudgetReservationID uuid.UUID
	BudgetWindows       []budgets.WindowStatus

	// BudgetScopes is the per-scope state for the composed multi-scope verdict
	// (#473): platform (subject) + subject (subject) + every (subject,role) +
	// (subject,invoker). On a budget deny, the blocking scope is the one whose
	// windows contain the breached (Allowed=false) window.
	BudgetScopes []budgets.ScopeStatus
	// BudgetSource/BudgetSourceID are the idempotency coords the reservations
	// were placed under, so settlement can capture/release ALL scopes by coords.
	BudgetSource   string
	BudgetSourceID string

	// QueueAcquired reports that queue/batch reservation units (#472 G2) were
	// held for this request; ThroughputBase is the (merchant:payer:release) key
	// they were held under, so settlement releases them by request coords.
	QueueAcquired  bool
	ThroughputBase string

	// ResolvedTier is the tier this verdict was evaluated under (#477): the
	// host-supplied req.Tier when explicit, else the auto-graduated tier from
	// cumulative paid spend (#476), else the lowest default. The host reads it
	// back to drive its own per-tier capacity decisions (e.g. tensorhub's
	// scheduler in-flight concurrency cap) without a second round-trip.
	ResolvedTier string

	// MaxConcurrentHeldAmount is the resolved tier's cap on the sum of the payer's
	// active (un-settled) hold $ (#487; 0 = uncapped). Surfaced on EVERY verdict so
	// a host that enforces true occupancy itself reads cap + per-job estimate and
	// queues in its own scheduler rather than relying on OpenRails' hard deny.
	MaxConcurrentHeldAmount int64
	// HeldAmount is the payer's active-hold $ sum AS EVALUATED for this verdict
	// (#487): on an allowed verdict it INCLUDES the hold just placed; on a
	// concurrent-held deny it is the pre-existing sum the new hold would have
	// breached. Lets the host reason about remaining headroom = cap - held.
	HeldAmount int64
	// MaxSingleChargeAmount is the resolved tier's per-charge ceiling (#487; 0 =
	// uncapped), surfaced for host introspection.
	MaxSingleChargeAmount int64
	PolicyCurrency        string
	PolicyAmount          int64
}

// Admit runs throughput then money and returns the unified decision.
func (a *Admitter) Admit(ctx context.Context, req AdmitRequest) (AdmitDecision, error) {
	// --- blocklist (#300): deny known-bad payment identifiers up front ---
	if a.blocklist != nil {
		for _, bc := range req.BlockChecks {
			blocked, err := a.blocklist.IsBlocked(ctx, bc.Kind, bc.Value)
			if err != nil {
				return AdmitDecision{}, err
			}
			if blocked {
				return AdmitDecision{Allowed: false, BlockedBy: "blocked", BlockedUnit: bc.Kind}, nil
			}
		}
	}

	// Suspension (#299): a past_due/suspended account is denied all spend.
	if req.EstimatedAmount > 0 {
		suspended, err := a.money.IsSuspended(ctx, req.CustomerID, req.Currency)
		if err != nil {
			return AdmitDecision{}, err
		}
		if suspended {
			return AdmitDecision{Allowed: false, BlockedBy: "suspended"}, nil
		}
	}

	// PM-on-file gate (#299): a credit-line (arrears) account must have a verified
	// payment method before it may spend on credit.
	if req.EstimatedAmount > 0 {
		needV, err := a.money.ArrearsRequiresVerification(ctx, req.CustomerID, req.Currency)
		if err != nil {
			return AdmitDecision{}, err
		}
		if needV {
			return AdmitDecision{Allowed: false, BlockedBy: "unverified"}, nil
		}
	}

	// Tier resolution: explicit tier > graduated tier (#298, earned from paid
	// spend) > lowest default (#300 new-account low default).
	tier := req.Tier
	if tier == "" {
		if req.EstimatedAmount > 0 {
			if t, terr := a.money.GetTier(ctx, req.CustomerID, req.Currency); terr == nil && t != "" {
				tier = t
			}
		}
		if tier == "" {
			tier = DefaultTier
		}
	}

	// --- tier policy + endpoint gating (#298) ---
	pol, err := a.policies.GetTierPolicy(ctx, req.CustomerID, tier)
	if err != nil {
		return AdmitDecision{}, err
	}
	if len(pol.EntitledResources) > 0 && !contains(pol.EntitledResources, req.Resource) {
		return AdmitDecision{Allowed: false, BlockedBy: "resource"}, nil
	}

	policyCurrency, err := effectivePolicyCurrency(req.Currency, pol.PolicyCurrency, pol.BudgetWindows)
	if err != nil {
		return AdmitDecision{}, err
	}
	policyAmount := req.EstimatedAmount
	if req.EstimatedAmount > 0 {
		policyAmount, _, err = fx.ConvertAmount(ctx, a.fxProvider, req.Currency, policyCurrency, req.EstimatedAmount)
		if err != nil {
			return AdmitDecision{}, err
		}
	}

	// Per-charge ceiling (#487): a single Admit whose estimate exceeds the tier's
	// max_single_charge_amount is a generic runaway guard — reject up front.
	if pol.MaxSingleChargeAmount > 0 && policyAmount > pol.MaxSingleChargeAmount {
		return AdmitDecision{
			Allowed: false, BlockedBy: "money", DenyCode: DenySingleChargeCap,
			ResolvedTier: tier, MaxSingleChargeAmount: pol.MaxSingleChargeAmount,
			MaxConcurrentHeldAmount: pol.MaxConcurrentHeldAmount,
			PolicyCurrency:          policyCurrency, PolicyAmount: policyAmount,
		}, nil
	}

	// --- throughput axis (cheap Redis op; counts even if money later denies) ---
	// KEY = (merchant, payer, endpoint-release) — #472 G1. The invoker (invoker) is
	// DELIBERATELY NOT in the key: per-invoker throughput is bypassable (a payer
	// can register unlimited invokers to multiply its limit), so the capacity
	// limit AGGREGATES across all of a payer's invokers. `Resource` is the
	// endpoint-release uuid (the host's releaseID), never a mutable name.
	tid, err := merchant.Require(ctx) // #336: no default merchant
	if err != nil {
		return AdmitDecision{}, err
	}
	tenantID := tid.UUID()

	// --- wasted-spend $ budgets (#497): delegated invokers are cut off when
	// their flat per-invoker wasted budget is over. Direct payer credentials are
	// not denied here; ReportWastedSpend charges over-grace waste through the
	// normal ledger. ---
	if a.wasted != nil && a.wasted.Enabled() {
		if !identity.IsDirectPayerInvoker(req.InvokerType) && req.Invoker != "" && len(a.invokerWastedWindows) > 0 {
			wastedCurrency, werr := effectiveWastedCurrency(req.Currency, a.invokerWastedWindows)
			if werr != nil {
				return AdmitDecision{}, werr
			}
			over, _, werr := a.wasted.InvokerOverBudget(ctx, tenantID.String(), req.CustomerID.UUID().String(), req.Invoker, wastedCurrency, a.invokerWastedWindows)
			if werr != nil {
				return AdmitDecision{}, werr
			}
			if over {
				return AdmitDecision{Allowed: false, BlockedBy: "abuse", DenyCode: DenyFailureRateLimited, ResolvedTier: tier, PolicyCurrency: wastedCurrency}, nil
			}
		}
	}

	base := fmt.Sprintf("%s:%s:%s", tenantID, req.CustomerID.UUID(), req.Resource)
	// Per-release throughput VALUES (#472 G1): a release-specific window list
	// overrides the tier-global default so two releases under one payer can carry
	// different RPM/TPM.
	throughput := pol.ThroughputForRelease(req.Resource)
	tp, err := a.limiter.Check(ctx, base, throughput, req.Amounts)
	if err != nil {
		return AdmitDecision{}, err
	}
	if !tp.Allowed {
		return AdmitDecision{
			Allowed:     false,
			BlockedBy:   "throughput",
			BlockedUnit: tp.BlockedUnit,
			RetryAfter:  retryAfter(tp),
			Windows:     tp.Windows,
		}, nil
	}

	// --- queue/batch reservation pools (#472 G2): acquire the request's units
	// against the per-(payer, endpoint-release) pools; deny BlockedBy="queue" on
	// overflow. Held while the job is pending, freed at settlement. ---
	var queueAcquired bool
	if len(pol.QueueLimits) > 0 && req.Source != "" && req.SourceID != "" {
		limits := make([]ratelimit.QueueLimit, 0, len(pol.QueueLimits))
		for _, ql := range pol.QueueLimits {
			if ql.Max <= 0 {
				continue
			}
			unit := ql.Unit
			if unit == "" {
				unit = "request"
			}
			limits = append(limits, ratelimit.QueueLimit{Unit: unit, Max: ql.Max})
		}
		if len(limits) > 0 {
			ttl := time.Hour
			if !req.ExpiresAt.IsZero() {
				if d := time.Until(req.ExpiresAt); d > 0 {
					ttl = d
				}
			}
			qd, err := a.limiter.AcquireQueue(ctx, base, req.Source, req.SourceID, limits, req.Amounts, ttl)
			if err != nil {
				return AdmitDecision{}, err
			}
			if !qd.Allowed {
				return AdmitDecision{
					Allowed:     false,
					BlockedBy:   "queue",
					BlockedUnit: qd.BlockedUnit,
					Windows:     tp.Windows,
				}, nil
			}
			queueAcquired = true
		}
	}

	// --- money-budget windows (#304/#337 fixed windows; #473 hierarchical
	// scopes): per-scope spend caps composed in one verdict over the ONE payer
	// balance. Each scope reserves a separate budget_reservations row (a GATE,
	// not a wallet); the single balance debit is placed by the ledger below. ---
	var budgetResID uuid.UUID
	var budgetWindows []budgets.WindowStatus
	var budgetScopeStatuses []budgets.ScopeStatus
	if a.budgets != nil && req.EstimatedAmount > 0 {
		ttl := time.Hour
		if !req.ExpiresAt.IsZero() {
			if d := time.Until(req.ExpiresAt); d > 0 {
				ttl = d
			}
		}

		scopes, err := a.buildBudgetScopes(ctx, req, pol)
		if err != nil {
			return AdmitDecision{}, err
		}
		if len(scopes) > 0 {
			policyCurrency, err = effectiveScopeCurrency(policyCurrency, scopes)
			if err != nil {
				return AdmitDecision{}, err
			}
			policyAmount, _, err = fx.ConvertAmount(ctx, a.fxProvider, req.Currency, policyCurrency, req.EstimatedAmount)
			if err != nil {
				return AdmitDecision{}, err
			}
			// Budgets and the ledger use the same currency internal precision. The
			// estimate reserves 1:1 against every matching scope's windows.
			statuses, ok, blockIdx, err := a.budgets.ReserveScopes(ctx, req.CustomerID, scopes, policyCurrency, policyAmount, req.Source, req.SourceID, ttl)
			if err != nil {
				return AdmitDecision{}, err
			}
			budgetScopeStatuses = statuses
			budgetWindows = flattenScopeWindows(statuses)
			for _, st := range statuses {
				if st.Scope == budgets.ScopeInvoker && st.Reserved != uuid.Nil {
					budgetResID = st.Reserved
				}
			}
			if !ok {
				// Free any queue units held above so a budget-denied request
				// doesn't leak its queue reservation.
				if queueAcquired {
					_ = a.limiter.ReleaseQueueByRequest(ctx, req.Source, req.SourceID)
				}
				return AdmitDecision{
					Allowed:        false,
					BlockedBy:      "budget",
					RetryAfter:     scopeRetry(statuses, blockIdx),
					Windows:        tp.Windows,
					BudgetWindows:  budgetWindows,
					BudgetScopes:   statuses,
					PolicyCurrency: policyCurrency,
					PolicyAmount:   policyAmount,
				}, nil
			}
		}
	}

	// --- concurrent-held $ cap (#487): the sum of the payer's ACTIVE (un-settled)
	// hold $ plus this estimate must not exceed the tier's max_concurrent_held.
	// Read BEFORE the hold is placed so the new hold is the (held + estimate)
	// projection. A host that enforces true occupancy itself reads the cap value +
	// estimate off the verdict and queues in its own scheduler instead of taking
	// this hard deny (so OpenRails' gate is the committed-$ admission backstop). ---
	var heldAmount int64
	if req.EstimatedAmount > 0 && pol.MaxConcurrentHeldAmount > 0 {
		held, herr := a.activeHeldInCurrency(ctx, req.CustomerID, policyCurrency)
		if herr != nil {
			return AdmitDecision{}, herr
		}
		heldAmount = held
		if held+policyAmount > pol.MaxConcurrentHeldAmount {
			if len(budgetScopeStatuses) > 0 && a.budgets != nil {
				_ = a.budgets.ReleaseByCoords(ctx, req.CustomerID, policyCurrency, req.Source, req.SourceID)
			}
			if queueAcquired {
				_ = a.limiter.ReleaseQueueByRequest(ctx, req.Source, req.SourceID)
			}
			return AdmitDecision{
				Allowed: false, BlockedBy: "money", DenyCode: DenyConcurrentHeldCap,
				Windows: tp.Windows, BudgetWindows: budgetWindows, BudgetScopes: budgetScopeStatuses,
				ResolvedTier: tier, MaxConcurrentHeldAmount: pol.MaxConcurrentHeldAmount,
				MaxSingleChargeAmount: pol.MaxSingleChargeAmount, HeldAmount: held,
				PolicyCurrency: policyCurrency, PolicyAmount: policyAmount,
			}, nil
		}
	}

	// --- money axis (reserve the estimate via the existing ledger gate) ---
	if req.EstimatedAmount > 0 {
		res, err := a.money.AuthorizeAndHold(ctx, money.AuthorizeHoldInput{
			Payer:           req.CustomerID,
			Invoker:         req.Invoker,
			Currency:        req.Currency,
			EstimatedAmount: req.EstimatedAmount,
			Source:          req.Source,
			SourceID:        req.SourceID,
			ExpiresAt:       req.ExpiresAt,
		})
		if err != nil {
			return AdmitDecision{}, err
		}
		if !res.Decision.Allowed {
			// Roll back ALL reserved budget scopes so a money-denied request
			// doesn't consume any of the payer's budget windows.
			if len(budgetScopeStatuses) > 0 && a.budgets != nil {
				_ = a.budgets.ReleaseByCoords(ctx, req.CustomerID, policyCurrency, req.Source, req.SourceID)
			}
			// Free any queue units held above (money deny must not leak the queue).
			if queueAcquired {
				_ = a.limiter.ReleaseQueueByRequest(ctx, req.Source, req.SourceID)
			}
			return AdmitDecision{Allowed: false, BlockedBy: "money", DenyCode: res.Decision.DenyCode, Windows: tp.Windows, BudgetWindows: budgetWindows, BudgetScopes: budgetScopeStatuses, ResolvedTier: tier, MaxConcurrentHeldAmount: pol.MaxConcurrentHeldAmount, MaxSingleChargeAmount: pol.MaxSingleChargeAmount, HeldAmount: heldAmount, PolicyCurrency: policyCurrency, PolicyAmount: policyAmount}, nil
		}
		// Allowed: the hold is now placed, so the active-held sum includes it.
		return AdmitDecision{Allowed: true, Windows: tp.Windows, CapacityAmount: res.CapacityAmount, BudgetReservationID: budgetResID, BudgetWindows: budgetWindows, BudgetScopes: budgetScopeStatuses, BudgetSource: req.Source, BudgetSourceID: req.SourceID, QueueAcquired: queueAcquired, ThroughputBase: base, ResolvedTier: tier, MaxConcurrentHeldAmount: pol.MaxConcurrentHeldAmount, MaxSingleChargeAmount: pol.MaxSingleChargeAmount, HeldAmount: heldAmount + policyAmount, PolicyCurrency: policyCurrency, PolicyAmount: policyAmount}, nil
	}

	return AdmitDecision{Allowed: true, Windows: tp.Windows, BudgetReservationID: budgetResID, BudgetWindows: budgetWindows, BudgetScopes: budgetScopeStatuses, BudgetSource: req.Source, BudgetSourceID: req.SourceID, QueueAcquired: queueAcquired, ThroughputBase: base, ResolvedTier: tier, MaxConcurrentHeldAmount: pol.MaxConcurrentHeldAmount, MaxSingleChargeAmount: pol.MaxSingleChargeAmount, PolicyCurrency: policyCurrency, PolicyAmount: policyAmount}, nil
}

// buildBudgetScopes assembles every budget scope to reserve for this request
// (#473): the (subject,invoker) windows from the tier policy (the pre-#473 path,
// always present) PLUS, when a BudgetPolicyStore is wired, the platform/subject
// (subject) caps and every (subject,role) cap the invoker holds. When no
// BudgetPolicyStore is wired this returns ONLY the invoker scope, exactly
// reproducing the pre-#473 single-window behavior.
func (a *Admitter) buildBudgetScopes(ctx context.Context, req AdmitRequest, pol ResolvedPolicy) ([]budgets.ScopeReservation, error) {
	var scopes []budgets.ScopeReservation

	// (subject, invoker) — the tier-policy windows; preserves the existing path.
	if len(pol.BudgetWindows) > 0 {
		sc, err := budgets.MakeScopeReservation(budgets.ScopeInvoker, "subject", req.Invoker, uuid.Nil, pol.BudgetWindows)
		if err != nil {
			return nil, err
		}
		scopes = append(scopes, sc)
	}

	if a.budgetScopes == nil {
		return scopes, nil
	}

	policies, err := a.budgetScopes.LoadAll(ctx, req.CustomerID)
	if err != nil {
		return nil, err
	}
	// #491 reversal: budgets are PER-INVOKER only. Subject/role POOLS are dropped;
	// only stored per-invoker overrides matching THIS invoker apply.
	for _, p := range policies {
		if budgets.NormalizeScope(p.Scope) != budgets.ScopeInvoker || p.ScopeKey != req.Invoker {
			continue
		}
		windows := toBudgetWindows(p.Windows)
		if len(windows) == 0 {
			continue
		}
		sc, err := budgets.MakeScopeReservation(budgets.ScopeInvoker, p.Owner, req.Invoker, uuid.Nil, windows)
		if err != nil {
			return nil, err
		}
		scopes = append(scopes, sc)
	}
	return scopes, nil
}

// flattenScopeWindows concatenates every scope's window statuses for the
// introspection field (back-compat with the single-scope BudgetWindows shape).
func flattenScopeWindows(scopes []budgets.ScopeStatus) []budgets.WindowStatus {
	var out []budgets.WindowStatus
	for _, sc := range scopes {
		out = append(out, sc.Windows...)
	}
	return out
}

// scopeRetry returns the soonest reset of the blocking scope's breached windows.
func scopeRetry(scopes []budgets.ScopeStatus, blockIdx int) time.Duration {
	if blockIdx >= 0 && blockIdx < len(scopes) {
		return budgetRetry(scopes[blockIdx].Windows)
	}
	// Fallback: scan all scopes for the soonest breached reset.
	var best time.Duration
	for _, sc := range scopes {
		if d := budgetRetry(sc.Windows); d > 0 && (best == 0 || d < best) {
			best = d
		}
	}
	return best
}

func budgetRetry(ws []budgets.WindowStatus) time.Duration {
	var best int64
	for _, w := range ws {
		if !w.Allowed && w.RetryAfterSeconds > 0 && (best == 0 || w.RetryAfterSeconds < best) {
			best = w.RetryAfterSeconds
		}
	}
	return time.Duration(best) * time.Second
}

// retryAfter returns the reset time of the window that blocked.
func retryAfter(d ratelimit.Decision) time.Duration {
	for _, w := range d.Windows {
		if w.Unit == d.BlockedUnit {
			return w.ResetAfter
		}
	}
	return 0
}

// toWastedWindows maps the tier policy's $-budget windows to the wasted-spend
// guard's window shape (#488). Window seconds → duration; the budget key is the
// window unit so each window is its own Redis counter.
func toWastedWindows(ws []models.BudgetWindowPolicy) []abuse.WastedWindow {
	out := make([]abuse.WastedWindow, 0, len(ws))
	for _, w := range ws {
		if w.WindowSeconds <= 0 {
			continue
		}
		out = append(out, abuse.WastedWindow{
			Key:      w.Key,
			Window:   time.Duration(w.WindowSeconds) * time.Second,
			Limit:    w.Limit,
			Currency: w.Currency,
		})
	}
	return out
}

func effectivePolicyCurrency(requestCurrency, configured string, windows []budgets.BudgetWindow) (string, error) {
	cur := money.NormalizeCurrency(requestCurrency)
	explicit := false
	if configured != "" {
		cur = money.NormalizeCurrency(configured)
		explicit = true
	}
	if err := money.ValidateCurrency(cur); err != nil {
		return "", err
	}
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
		if cur != wc {
			return "", fmt.Errorf("mixed policy currencies are not supported in one admit policy: %s and %s", cur, wc)
		}
	}
	return cur, nil
}

func effectiveScopeCurrency(cur string, scopes []budgets.ScopeReservation) (string, error) {
	for _, sc := range scopes {
		next, err := effectivePolicyCurrency(cur, "", sc.Windows)
		if err != nil {
			return "", err
		}
		if next != cur {
			return "", fmt.Errorf("mixed policy currencies are not supported in one admit policy: %s and %s", cur, next)
		}
	}
	return cur, nil
}

func effectiveWastedCurrency(requestCurrency string, windows []abuse.WastedWindow) (string, error) {
	cur := money.NormalizeCurrency(requestCurrency)
	explicit := false
	if err := money.ValidateCurrency(cur); err != nil {
		return "", err
	}
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
			return "", fmt.Errorf("mixed wasted-spend currencies are not supported in one policy: %s and %s", cur, wc)
		}
	}
	return cur, nil
}

func (a *Admitter) activeHeldInCurrency(ctx context.Context, payer identity.CustomerID, policyCurrency string) (int64, error) {
	var total int64
	tid, err := merchant.Require(ctx)
	if err != nil {
		return 0, err
	}
	for _, cur := range money.CurrencyCodes() {
		held, err := a.money.ActiveHeldForCurrency(ctx, payer, cur)
		if err != nil {
			return 0, err
		}
		if a.holds != nil {
			redisHeld, rerr := a.holds.ActiveAmount(ctx, tid.UUID().String(), payer.UUID().String(), cur)
			if rerr != nil {
				return 0, rerr
			}
			held += redisHeld
		}
		if held == 0 {
			continue
		}
		converted, _, err := fx.ConvertAmount(ctx, a.fxProvider, cur, policyCurrency, held)
		if err != nil {
			return 0, err
		}
		total += converted
	}
	return total, nil
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
