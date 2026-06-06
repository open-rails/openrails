package admission

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/abuse"
	"github.com/open-rails/openrails/internal/modules/budgets"
	"github.com/open-rails/openrails/internal/modules/credits"
	"github.com/open-rails/openrails/internal/modules/ratelimit"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/tenant"
)

// Admitter is the unified admission check (issue #298): throughput (Redis) +
// money (ledger) in one decision. Throughput is evaluated first (one cheap Redis
// op); per the OpenAI model a counted request still counts even if the money gate
// then denies. Money is checked via the existing AuthorizeAndHold (which reserves
// the estimate). Deny on whichever axis blocks first.
// DefaultTier is assigned to invokers with no explicit tier — the new-account low
// default (#300): start at the lowest tier until graduated.
const DefaultTier = "free"

type Admitter struct {
	limiter   *ratelimit.Limiter
	credits   *credits.CreditsService
	policies  *TierPolicyStore
	blocklist *abuse.BlocklistService // optional; nil disables blocklist checks
	budgets   *budgets.Service        // optional; nil disables rolling money-budget windows (#304)
}

func NewAdmitter(limiter *ratelimit.Limiter, creditsSvc *credits.CreditsService, policies *TierPolicyStore, blocklist *abuse.BlocklistService, budgetSvc *budgets.Service) *Admitter {
	return &Admitter{limiter: limiter, credits: creditsSvc, policies: policies, blocklist: blocklist, budgets: budgetSvc}
}

// BlockCheck is one (kind,value) tested against the payment blocklist (#300),
// e.g. {"card_fingerprint","abc"} or {"ip","1.2.3.4"}.
type BlockCheck struct {
	Kind  string
	Value string
}

// AdmitRequest is one admission decision input.
type AdmitRequest struct {
	TenantSubjectID identity.TenantSubjectID // the tenant subject
	Invoker         string                   // canonical invoker: user:<id> / serviceToken:<key_id> / <issuer>:<sub>
	Tier            string                   // the invoker's tier (selects the throughput policy)
	Model           string                   // endpoint/model (namespaces the throughput counters)

	// Amounts is the throughput consumption for this request, e.g.
	// {"request":1,"token":150}.
	Amounts map[string]int64

	// Money axis (skipped when EstimateCents == 0).
	CreditType    string
	EstimateCents int64
	Source        string    // idempotency namespace (e.g. "usage")
	SourceID      string    // idempotency id (e.g. request id)
	ExpiresAt     time.Time // hold expiry

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
	Windows     []ratelimit.WindowInfo    // for x-ratelimit-* headers
	Hold        *models.CreditTransaction // the placed money hold when allowed

	// BudgetReservationID is the rolling money-budget reservation placed when
	// allowed (#304); BudgetWindows is the per-window state for introspection.
	BudgetReservationID uuid.UUID
	BudgetWindows       []budgets.WindowStatus
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
	if req.CreditType != "" {
		suspended, err := a.credits.IsSuspended(ctx, req.TenantSubjectID, req.CreditType)
		if err != nil {
			return AdmitDecision{}, err
		}
		if suspended {
			return AdmitDecision{Allowed: false, BlockedBy: "suspended"}, nil
		}
	}

	// PM-on-file gate (#299): a credit-line (arrears) account must have a verified
	// payment method before it may spend on credit.
	if req.CreditType != "" {
		needV, err := a.credits.ArrearsRequiresVerification(ctx, req.TenantSubjectID, req.CreditType)
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
		if req.CreditType != "" {
			if t, terr := a.credits.GetTier(ctx, req.TenantSubjectID, req.CreditType); terr == nil && t != "" {
				tier = t
			}
		}
		if tier == "" {
			tier = DefaultTier
		}
	}

	// --- tier policy + endpoint gating (#298) ---
	pol, err := a.policies.GetTierPolicy(ctx, req.TenantSubjectID, tier)
	if err != nil {
		return AdmitDecision{}, err
	}
	if len(pol.EntitledEndpoints) > 0 && !contains(pol.EntitledEndpoints, req.Model) {
		return AdmitDecision{Allowed: false, BlockedBy: "endpoint"}, nil
	}

	// --- throughput axis (cheap Redis op; counts even if money later denies) ---
	tenantID := tenant.FromContextOrDefault(ctx).UUID()
	base := fmt.Sprintf("%s:%s:%s:%s", tenantID, req.TenantSubjectID.UUID(), req.Invoker, req.Model)
	tp, err := a.limiter.Check(ctx, base, pol.Throughput, req.Amounts)
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

	// --- money-budget windows (#304): rolling per-tier spend caps on the invoker ---
	var budgetResID uuid.UUID
	var budgetWindows []budgets.WindowStatus
	if a.budgets != nil && len(pol.BudgetWindows) > 0 && req.EstimateCents > 0 {
		ttl := time.Hour
		if !req.ExpiresAt.IsZero() {
			if d := time.Until(req.ExpiresAt); d > 0 {
				ttl = d
			}
		}
		resID, statuses, ok, err := a.budgets.Reserve(ctx, req.TenantSubjectID, req.Invoker, pol.BudgetWindows, req.EstimateCents, req.Source, req.SourceID, ttl)
		if err != nil {
			return AdmitDecision{}, err
		}
		budgetWindows = statuses
		if !ok {
			return AdmitDecision{Allowed: false, BlockedBy: "budget", RetryAfter: budgetRetry(statuses), Windows: tp.Windows, BudgetWindows: statuses}, nil
		}
		budgetResID = resID
	}

	// --- money axis (reserve the estimate via the existing ledger gate) ---
	if req.EstimateCents > 0 {
		res, err := a.credits.AuthorizeAndHold(ctx, credits.AuthorizeHoldInput{
			Payer:         req.TenantSubjectID,
			Invoker:       req.Invoker,
			CreditType:    req.CreditType,
			EstimateCents: req.EstimateCents,
			Source:        req.Source,
			SourceID:      req.SourceID,
			ExpiresAt:     req.ExpiresAt,
		})
		if err != nil {
			return AdmitDecision{}, err
		}
		if !res.Decision.Allowed {
			// Roll back the budget reservation so a money-denied request doesn't
			// consume the invoker's rolling budget.
			if budgetResID != uuid.Nil && a.budgets != nil {
				_ = a.budgets.Release(ctx, budgetResID)
			}
			return AdmitDecision{Allowed: false, BlockedBy: "money", DenyCode: res.Decision.DenyCode, Windows: tp.Windows, BudgetWindows: budgetWindows}, nil
		}
		return AdmitDecision{Allowed: true, Windows: tp.Windows, Hold: res.Hold, BudgetReservationID: budgetResID, BudgetWindows: budgetWindows}, nil
	}

	return AdmitDecision{Allowed: true, Windows: tp.Windows, BudgetReservationID: budgetResID, BudgetWindows: budgetWindows}, nil
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
