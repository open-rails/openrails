package admission

import (
	"cmp"
	"context"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/admission/spendgate"
	"github.com/open-rails/openrails/internal/modules/limits"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/modules/money"
)

// Bind is the admitter on a transaction's database, as Admit runs it.
func (a *Admitter) Bind(d *db.DB) *Admitter {
	bound := *a
	bound.money = a.money.WithDatabase(d)
	bound.loader = NewSpendgatePolicyLoader(NewBillingPolicyStore(d), a.loader.fx)
	return &bound
}

// Set is what a gate's view resolves for one customer and the invoker
// attributes of its requests: a refusal the checks made whatever a request
// costs, or the spend windows and declared limits a request claims (costing
// nothing here), what the customer may spend and its policy's credit line.
type Set struct {
	// Trust is the trust level the request's policy was resolved at.
	Trust   billing.EffectiveTrustLevel
	Refusal *AdmitDecision
	Spend   []limits.Claim
	Limits  []limits.Claim
	// Available is what the customer may spend after every live hold, and
	// CreditLine the overdraft the policy allows on it.
	Available, CreditLine, Outstanding int64
}

// ResolveSet makes the checks Admit makes before it weighs any cost, for
// requests of eventType ("" when they state an amount), and names what they
// claim. It reads; it takes no lock and holds nothing.
func (a *Admitter) ResolveSet(ctx context.Context, q *gen.Queries, req AdmitRequest, eventType string, capacity money.AdmissionCapacity, now time.Time) (Set, error) {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return Set{}, err
	}
	g, refused, err := a.gates(ctx, q, req)
	if err != nil {
		return Set{}, err
	}
	if refused != nil {
		return Set{Trust: g.trust, Refusal: refused}, nil
	}
	out := Set{Trust: g.trust}
	if out.Limits, err = a.limitClaimsFor(ctx, q, tid.UUID(), req, eventType); err != nil {
		return Set{}, err
	}
	if out.Spend, err = spendgate.SpendClaims(tid.UUID(), req.CustomerID.UUID(), strings.TrimSpace(req.Currency), g.policy, g.request, 0, now); err != nil {
		return Set{}, err
	}
	out.Available, out.CreditLine, out.Outstanding = payerCapacity(capacity)
	return out, nil
}

// BalanceRefusal is the deny code a request that does not fit the balance
// gets, as Admit answers it: insufficient_balance, insufficient_credit with a
// credit line, or outstanding_cap_reached when what it owes took the line.
func (s Set) BalanceRefusal() billing.DenyCode {
	code := billing.DenyInsufficientBalance
	if s.CreditLine > 0 {
		code = billing.DenyInsufficientCredit
	}
	if s.CreditLine == 0 && s.Available <= 0 && s.Outstanding > 0 {
		code = billing.DenyOutstandingCapReached
	}
	return code
}

// SpendPolicy is the billing policy usage of one customer's invoker
// attributes counts toward, resolved once: its spend windows at any instant
// follow without another read.
type SpendPolicy struct {
	merchant, customer uuid.UUID
	currency           string
	policy             spendgate.Policy
	request            spendgate.Request
}

// SpendPolicies resolves each request's billing policy, reading the
// merchant's settings, the customers' own policies and their trust levels
// once for all: as a view resolves it, and none for a delegated invoker,
// which spends nothing of the customer's. errs[i] is reqs[i]'s own failure.
func (a *Admitter) SpendPolicies(ctx context.Context, reqs []AdmitRequest) ([]*SpendPolicy, []error, error) {
	policies, errs := make([]*SpendPolicy, len(reqs)), make([]error, len(reqs))
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, nil, err
	}
	var payers []uuid.UUID
	for _, req := range reqs {
		if identity.IsDirectPayerInvoker(req.InvokerType) {
			payers = append(payers, req.CustomerID.UUID())
		}
	}
	if len(payers) == 0 {
		return policies, errs, nil
	}
	r, rerr := a.loader.policies.resolver(ctx, payers)
	// Trust levels matter only where a policy is declared.
	tiers := map[string]map[uuid.UUID]billing.EffectiveTrustLevel{}
	if rerr == nil && r.declares() {
		byCurrency := map[string][]uuid.UUID{}
		for _, req := range reqs {
			if identity.IsDirectPayerInvoker(req.InvokerType) && req.TrustLevel == "" {
				cur := money.NormalizeCurrency(req.Currency)
				byCurrency[cur] = append(byCurrency[cur], req.CustomerID.UUID())
			}
		}
		for cur, customers := range byCurrency {
			if tiers[cur], err = a.money.TrustLevelsIn(ctx, customers, cur); err != nil {
				return nil, nil, err
			}
		}
	}
	for i, req := range reqs {
		if !identity.IsDirectPayerInvoker(req.InvokerType) {
			continue
		}
		if rerr != nil {
			errs[i] = rerr
			continue
		}
		trustLevel := req.TrustLevel
		if trustLevel == "" {
			trustLevel = cmp.Or(tiers[money.NormalizeCurrency(req.Currency)][req.CustomerID.UUID()].TrustLevel, merchantconfig.DefaultTrustLevel)
		}
		policy, err := a.loader.Load(ctx, req.Currency, r.resolve(req.CustomerID.UUID(), trustLevel))
		if err != nil {
			errs[i] = err
			continue
		}
		policies[i] = &SpendPolicy{merchant: tid.UUID(), customer: req.CustomerID.UUID(), currency: strings.TrimSpace(req.Currency), policy: policy,
			request: spendgate.Request{Invoker: req.Invoker, TrustLevel: trustLevel, Roles: roleStrings(req.Roles), Measure: req.Resource}}
	}
	return policies, errs, nil
}

// Windows are the policy's spend windows current at an instant; none for a
// nil policy.
func (p *SpendPolicy) Windows(at time.Time) ([]limits.Claim, error) {
	if p == nil {
		return nil, nil
	}
	return spendgate.SpendClaims(p.merchant, p.customer, p.currency, p.policy, p.request, 0, at)
}
