package admission

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/abuse"
	"github.com/open-rails/openrails/internal/modules/budgets"
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

type Admitter struct {
	limiter      *ratelimit.Limiter
	money        *money.MoneyService
	policies     *TierPolicyStore
	blocklist    *abuse.BlocklistService // optional; nil disables blocklist checks
	budgets      *budgets.Service        // optional; nil disables fixed money-budget windows (#304, #337)
	budgetScopes *BudgetPolicyStore      // optional; nil disables hierarchical budget scopes (#473)
}

func NewAdmitter(limiter *ratelimit.Limiter, moneySvc *money.MoneyService, policies *TierPolicyStore, blocklist *abuse.BlocklistService, budgetSvc *budgets.Service) *Admitter {
	return &Admitter{limiter: limiter, money: moneySvc, policies: policies, blocklist: blocklist, budgets: budgetSvc}
}

// WithBudgetScopes enables hierarchical budget-scope composition (#473): the
// platform (subject) cap, the subject's self (subject) cap, every (subject,role)
// cap the actor holds, plus the (subject,actor) cap — all reserved in one
// verdict. Nil store leaves only the pre-#473 (subject,actor) tier-policy path.
func (a *Admitter) WithBudgetScopes(store *BudgetPolicyStore) *Admitter {
	a.budgetScopes = store
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
	MerchantSubjectID identity.MerchantSubjectID // the tenant subject
	Actor           string                   // canonical actor: user:<id> / serviceToken:<key_id> / <issuer>:<sub>
	Tier            string                   // the actor's tier (selects the throughput policy)
	Resource        string                   // caller-supplied resource string (namespaces the throughput counters)

	// Roles are the immutable role UUIDs the actor holds (#473). The host reads
	// them from the delegated JWT/permission set. Each role with a matching
	// (subject, role) budget policy gates this request's spend.
	Roles []uuid.UUID

	// Amounts is the throughput consumption for this request, e.g.
	// {"request":1,"token":150}.
	Amounts map[string]int64

	// Money axis (skipped when EstimateMicros == 0). Money is always micro-dollars.
	EstimateMicros int64
	Source         string    // idempotency namespace (e.g. "usage")
	SourceID       string    // idempotency id (e.g. request id)
	ExpiresAt      time.Time // hold expiry

	// BlockChecks (optional) are payment identifiers to test against the #300
	// blocklist (card fingerprint, processor customer, email, ip).
	BlockChecks []BlockCheck
}

// AdmitDecision is the unified outcome.
type AdmitDecision struct {
	Allowed     bool
	BlockedBy   string // "throughput" | "money" | ""
	BlockedUnit string // throughput: the window unit that blocked
	DenyCode    string // money: the ledger deny code
	RetryAfter  time.Duration
	Windows     []ratelimit.WindowInfo   // for x-ratelimit-* headers
	Hold        *models.MoneyTransaction // the placed money hold when allowed

	// BudgetReservationID is the (subject,actor)-scope money-budget reservation
	// placed when allowed (#304, kept for back-compat); BudgetWindows is the
	// flattened per-window state for introspection.
	BudgetReservationID uuid.UUID
	BudgetWindows       []budgets.WindowStatus

	// BudgetScopes is the per-scope state for the composed multi-scope verdict
	// (#473): platform (subject) + subject (subject) + every (subject,role) +
	// (subject,actor). On a budget deny, the blocking scope is the one whose
	// windows contain the breached (Allowed=false) window.
	BudgetScopes []budgets.ScopeStatus
	// BudgetSource/BudgetSourceID are the idempotency coords the reservations
	// were placed under, so settlement can capture/release ALL scopes by coords.
	BudgetSource   string
	BudgetSourceID string

	// QueueAcquired reports that queue/batch reservation units (#472 G2) were
	// held for this request; ThroughputBase is the (tenant:payer:release) key
	// they were held under, so settlement releases them by request coords.
	QueueAcquired  bool
	ThroughputBase string

	// ResolvedTier is the tier this verdict was evaluated under (#477): the
	// host-supplied req.Tier when explicit, else the auto-graduated tier from
	// cumulative paid spend (#476), else the lowest default. The host reads it
	// back to drive its own per-tier capacity decisions (e.g. tensorhub's
	// scheduler in-flight concurrency cap) without a second round-trip.
	ResolvedTier string
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
	if req.EstimateMicros > 0 {
		suspended, err := a.money.IsSuspended(ctx, req.MerchantSubjectID)
		if err != nil {
			return AdmitDecision{}, err
		}
		if suspended {
			return AdmitDecision{Allowed: false, BlockedBy: "suspended"}, nil
		}
	}

	// PM-on-file gate (#299): a credit-line (arrears) account must have a verified
	// payment method before it may spend on credit.
	if req.EstimateMicros > 0 {
		needV, err := a.money.ArrearsRequiresVerification(ctx, req.MerchantSubjectID)
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
		if req.EstimateMicros > 0 {
			if t, terr := a.money.GetTier(ctx, req.MerchantSubjectID); terr == nil && t != "" {
				tier = t
			}
		}
		if tier == "" {
			tier = DefaultTier
		}
	}

	// --- tier policy + endpoint gating (#298) ---
	pol, err := a.policies.GetTierPolicy(ctx, req.MerchantSubjectID, tier)
	if err != nil {
		return AdmitDecision{}, err
	}
	if len(pol.EntitledResources) > 0 && !contains(pol.EntitledResources, req.Resource) {
		return AdmitDecision{Allowed: false, BlockedBy: "resource"}, nil
	}

	// --- throughput axis (cheap Redis op; counts even if money later denies) ---
	// KEY = (tenant, payer, endpoint-release) — #472 G1. The invoker (actor) is
	// DELIBERATELY NOT in the key: per-invoker throughput is bypassable (a payer
	// can register unlimited invokers to multiply its limit), so the capacity
	// limit AGGREGATES across all of a payer's invokers. `Resource` is the
	// endpoint-release uuid (the host's releaseID), never a mutable name.
	tid, err := merchant.Require(ctx) // #336: no default tenant
	if err != nil {
		return AdmitDecision{}, err
	}
	tenantID := tid.UUID()
	base := fmt.Sprintf("%s:%s:%s", tenantID, req.MerchantSubjectID.UUID(), req.Resource)
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
	if a.budgets != nil && req.EstimateMicros > 0 {
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
			// Budgets and the ledger are BOTH micro-dollars (#337/#463). The
			// estimate reserves 1:1 against every matching scope's windows.
			statuses, ok, blockIdx, err := a.budgets.ReserveScopes(ctx, req.MerchantSubjectID, scopes, req.EstimateMicros, req.Source, req.SourceID, ttl)
			if err != nil {
				return AdmitDecision{}, err
			}
			budgetScopeStatuses = statuses
			budgetWindows = flattenScopeWindows(statuses)
			for _, st := range statuses {
				if st.Scope == budgets.ScopeActor && st.Reserved != uuid.Nil {
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
					Allowed:       false,
					BlockedBy:     "budget",
					RetryAfter:    scopeRetry(statuses, blockIdx),
					Windows:       tp.Windows,
					BudgetWindows: budgetWindows,
					BudgetScopes:  statuses,
				}, nil
			}
		}
	}

	// --- money axis (reserve the estimate via the existing ledger gate) ---
	if req.EstimateMicros > 0 {
		res, err := a.money.AuthorizeAndHold(ctx, money.AuthorizeHoldInput{
			Payer:          req.MerchantSubjectID,
			Actor:          req.Actor,
			EstimateMicros: req.EstimateMicros,
			Source:         req.Source,
			SourceID:       req.SourceID,
			ExpiresAt:      req.ExpiresAt,
		})
		if err != nil {
			return AdmitDecision{}, err
		}
		if !res.Decision.Allowed {
			// Roll back ALL reserved budget scopes so a money-denied request
			// doesn't consume any of the payer's budget windows.
			if len(budgetScopeStatuses) > 0 && a.budgets != nil {
				_ = a.budgets.ReleaseByCoords(ctx, req.MerchantSubjectID, req.Source, req.SourceID)
			}
			// Free any queue units held above (money deny must not leak the queue).
			if queueAcquired {
				_ = a.limiter.ReleaseQueueByRequest(ctx, req.Source, req.SourceID)
			}
			return AdmitDecision{Allowed: false, BlockedBy: "money", DenyCode: res.Decision.DenyCode, Windows: tp.Windows, BudgetWindows: budgetWindows, BudgetScopes: budgetScopeStatuses}, nil
		}
		return AdmitDecision{Allowed: true, Windows: tp.Windows, Hold: res.Hold, BudgetReservationID: budgetResID, BudgetWindows: budgetWindows, BudgetScopes: budgetScopeStatuses, BudgetSource: req.Source, BudgetSourceID: req.SourceID, QueueAcquired: queueAcquired, ThroughputBase: base, ResolvedTier: tier}, nil
	}

	return AdmitDecision{Allowed: true, Windows: tp.Windows, BudgetReservationID: budgetResID, BudgetWindows: budgetWindows, BudgetScopes: budgetScopeStatuses, BudgetSource: req.Source, BudgetSourceID: req.SourceID, QueueAcquired: queueAcquired, ThroughputBase: base, ResolvedTier: tier}, nil
}

// buildBudgetScopes assembles every budget scope to reserve for this request
// (#473): the (subject,actor) windows from the tier policy (the pre-#473 path,
// always present) PLUS, when a BudgetPolicyStore is wired, the platform/subject
// (subject) caps and every (subject,role) cap the actor holds. When no
// BudgetPolicyStore is wired this returns ONLY the actor scope, exactly
// reproducing the pre-#473 single-window behavior.
func (a *Admitter) buildBudgetScopes(ctx context.Context, req AdmitRequest, pol ResolvedPolicy) ([]budgets.ScopeReservation, error) {
	var scopes []budgets.ScopeReservation

	// (subject, actor) — the tier-policy windows; preserves the existing path.
	if len(pol.BudgetWindows) > 0 {
		sc, err := budgets.MakeScopeReservation(budgets.ScopeActor, "subject", req.Actor, uuid.Nil, pol.BudgetWindows)
		if err != nil {
			return nil, err
		}
		scopes = append(scopes, sc)
	}

	if a.budgetScopes == nil {
		return scopes, nil
	}

	policies, err := a.budgetScopes.LoadAll(ctx, req.MerchantSubjectID)
	if err != nil {
		return nil, err
	}
	roleSet := make(map[uuid.UUID]bool, len(req.Roles))
	for _, r := range req.Roles {
		roleSet[r] = true
	}
	for _, p := range policies {
		windows := toBudgetWindows(p.Windows)
		if len(windows) == 0 {
			continue
		}
		switch p.Scope {
		case budgets.ScopeSubject:
			sc, err := budgets.MakeScopeReservation(budgets.ScopeSubject, p.Owner, "", uuid.Nil, windows)
			if err != nil {
				return nil, err
			}
			scopes = append(scopes, sc)
		case budgets.ScopeRole:
			rid, perr := uuid.Parse(p.ScopeKey)
			if perr != nil || !roleSet[rid] {
				continue // not a role this actor holds (or a malformed key)
			}
			sc, err := budgets.MakeScopeReservation(budgets.ScopeRole, p.Owner, "", rid, windows)
			if err != nil {
				return nil, err
			}
			scopes = append(scopes, sc)
		case budgets.ScopeActor:
			// A stored (subject,actor) override applies only to the matching actor.
			if p.ScopeKey != req.Actor {
				continue
			}
			sc, err := budgets.MakeScopeReservation(budgets.ScopeActor, p.Owner, req.Actor, uuid.Nil, windows)
			if err != nil {
				return nil, err
			}
			scopes = append(scopes, sc)
		}
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

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
