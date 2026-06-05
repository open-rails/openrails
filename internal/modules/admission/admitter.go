package admission

import (
	"context"
	"fmt"
	"time"

	"github.com/open-rails/openrails/internal/db/models"
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
type Admitter struct {
	limiter  *ratelimit.Limiter
	credits  *credits.CreditsService
	policies *TierPolicyStore
}

func NewAdmitter(limiter *ratelimit.Limiter, creditsSvc *credits.CreditsService, policies *TierPolicyStore) *Admitter {
	return &Admitter{limiter: limiter, credits: creditsSvc, policies: policies}
}

// AdmitRequest is one admission decision input.
type AdmitRequest struct {
	Owner identity.OwnerOrgID // the payer (org)
	Actor string              // canonical invoker: user:<id> / oat:<key_id> / <issuer>:<sub>
	Tier  string              // the actor's tier (selects the throughput policy)
	Model string              // endpoint/model (namespaces the throughput counters)

	// Amounts is the throughput consumption for this request, e.g.
	// {"request":1,"token":150}.
	Amounts map[string]int64

	// Money axis (skipped when EstimateCents == 0).
	CreditType    string
	EstimateCents int64
	Source        string    // idempotency namespace (e.g. "usage")
	SourceID      string    // idempotency id (e.g. request id)
	ExpiresAt     time.Time // hold expiry
}

// AdmitDecision is the unified outcome.
type AdmitDecision struct {
	Allowed     bool
	BlockedBy   string // "throughput" | "money" | ""
	BlockedUnit string // throughput: the window unit that blocked
	DenyCode    string // money: the ledger deny code
	RetryAfter  time.Duration
	Windows     []ratelimit.WindowInfo     // for x-ratelimit-* headers
	Hold        *models.CreditTransaction  // the placed money hold when allowed
}

// Admit runs throughput then money and returns the unified decision.
func (a *Admitter) Admit(ctx context.Context, req AdmitRequest) (AdmitDecision, error) {
	// --- throughput axis (cheap Redis op; counts even if money later denies) ---
	pol, err := a.policies.GetThroughputPolicy(ctx, req.Owner, req.Tier)
	if err != nil {
		return AdmitDecision{}, err
	}
	tenantID := tenant.FromContextOrDefault(ctx).UUID()
	base := fmt.Sprintf("%s:%s:%s:%s", tenantID, req.Owner.UUID(), req.Actor, req.Model)
	tp, err := a.limiter.Check(ctx, base, pol, req.Amounts)
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

	// --- money axis (reserve the estimate via the existing ledger gate) ---
	if req.EstimateCents > 0 {
		res, err := a.credits.AuthorizeAndHold(ctx, credits.AuthorizeHoldInput{
			Owner:         req.Owner,
			Invoker:       req.Actor,
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
			return AdmitDecision{
				Allowed:   false,
				BlockedBy: "money",
				DenyCode:  res.Decision.DenyCode,
				Windows:   tp.Windows,
			}, nil
		}
		return AdmitDecision{Allowed: true, Windows: tp.Windows, Hold: res.Hold}, nil
	}

	return AdmitDecision{Allowed: true, Windows: tp.Windows}, nil
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
