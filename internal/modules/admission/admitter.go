package admission

import (
	"context"
	"fmt"
	"time"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/abuse"
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
// DefaultTier is assigned to actors with no explicit tier — the new-account low
// default (#300): start at the lowest tier until graduated.
const DefaultTier = "free"

type Admitter struct {
	limiter   *ratelimit.Limiter
	credits   *credits.CreditsService
	policies  *TierPolicyStore
	blocklist *abuse.BlocklistService // optional; nil disables blocklist checks
}

func NewAdmitter(limiter *ratelimit.Limiter, creditsSvc *credits.CreditsService, policies *TierPolicyStore, blocklist *abuse.BlocklistService) *Admitter {
	return &Admitter{limiter: limiter, credits: creditsSvc, policies: policies, blocklist: blocklist}
}

// BlockCheck is one (kind,value) tested against the payment blocklist (#300),
// e.g. {"card_fingerprint","abc"} or {"ip","1.2.3.4"}.
type BlockCheck struct {
	Kind  string
	Value string
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
	Windows     []ratelimit.WindowInfo     // for x-ratelimit-* headers
	Hold        *models.CreditTransaction  // the placed money hold when allowed
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
		suspended, err := a.credits.IsSuspended(ctx, req.Owner, req.CreditType)
		if err != nil {
			return AdmitDecision{}, err
		}
		if suspended {
			return AdmitDecision{Allowed: false, BlockedBy: "suspended"}, nil
		}
	}

	// New-account low default (#300): no explicit tier => lowest tier.
	tier := req.Tier
	if tier == "" {
		tier = DefaultTier
	}

	// --- tier policy + endpoint gating (#298) ---
	pol, err := a.policies.GetTierPolicy(ctx, req.Owner, tier)
	if err != nil {
		return AdmitDecision{}, err
	}
	if len(pol.EntitledEndpoints) > 0 && !contains(pol.EntitledEndpoints, req.Model) {
		return AdmitDecision{Allowed: false, BlockedBy: "endpoint"}, nil
	}

	// --- throughput axis (cheap Redis op; counts even if money later denies) ---
	tenantID := tenant.FromContextOrDefault(ctx).UUID()
	base := fmt.Sprintf("%s:%s:%s:%s", tenantID, req.Owner.UUID(), req.Actor, req.Model)
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

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
