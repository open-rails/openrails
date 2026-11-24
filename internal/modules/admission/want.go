package admission

import (
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
	"github.com/open-rails/openrails/internal/modules/money"
)

// Bind is the admitter on a transaction's database, as Admit runs it.
func (a *Admitter) Bind(d *db.DB, now func() time.Time) *Admitter {
	bound := *a
	bound.money = a.money.WithDatabase(d)
	bound.loader = NewSpendgatePolicyLoader(NewBillingPolicyStore(d), a.loader.fx)
	if a.rates != nil {
		rates := *a.rates
		rates.db = d
		rates.now = now
		bound.rates = &rates
	}
	return &bound
}

// Set is what a gate's view resolves for one customer and the invoker
// attributes of its requests: a refusal the checks made whatever a request
// costs, or the spend windows and declared limits a request claims (costing
// nothing here), what the customer may spend and its policy's credit line.
type Set struct {
	Refusal *AdmitDecision
	Spend   []limits.Claim
	Limits  []limits.Claim
	// Available is what the customer may spend after every live hold, and
	// CreditLine the overdraft the policy allows on it.
	Available, CreditLine, Outstanding int64
	resolved                           ResolvedPolicy
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
		return Set{Refusal: refused}, nil
	}
	out := Set{resolved: g.resolved}
	if out.Limits, err = a.limitClaimsFor(ctx, q, tid.UUID(), req, eventType); err != nil {
		return Set{}, err
	}
	if out.Spend, err = spendgate.SpendClaims(tid.UUID(), req.CustomerID.UUID(), strings.TrimSpace(req.Currency), g.policy, g.request, 0, now); err != nil {
		return Set{}, err
	}
	out.Available, out.CreditLine, out.Outstanding = payerCapacity(capacity, g.resolved)
	return out, nil
}

// BalanceRefusal is the deny code a request that does not fit the balance
// gets, as Admit answers it: insufficient_balance, insufficient_credit with a
// credit line, or outstanding_cap_reached when the policy gates on debt.
func (s Set) BalanceRefusal() billing.DenyCode {
	code := billing.DenyInsufficientBalance
	if s.CreditLine > 0 {
		code = billing.DenyInsufficientCredit
	}
	if s.resolved.GatesOnOutstandingOwed() && s.CreditLine == 0 && s.Available <= 0 && s.Outstanding > 0 {
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

// SpendPolicy resolves req's billing policy; a delegated invoker spends
// nothing of the customer's, so none.
func (a *Admitter) SpendPolicy(ctx context.Context, req AdmitRequest) (*SpendPolicy, error) {
	if !identity.IsDirectPayerInvoker(req.InvokerType) {
		return nil, nil
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	sgReq, _, _, policy, err := a.policyOf(ctx, req, a.trustLevel(ctx, req))
	if err != nil {
		return nil, err
	}
	return &SpendPolicy{merchant: tid.UUID(), customer: req.CustomerID.UUID(), currency: strings.TrimSpace(req.Currency), policy: policy, request: sgReq}, nil
}

// Windows are the policy's spend windows current at an instant; none for a
// nil policy.
func (p *SpendPolicy) Windows(at time.Time) ([]limits.Claim, error) {
	if p == nil {
		return nil, nil
	}
	return spendgate.SpendClaims(p.merchant, p.customer, p.currency, p.policy, p.request, 0, at)
}
