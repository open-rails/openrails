package admission

import (
	"context"
	"strings"
	"time"

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

// Want is what a lease's want for one customer resolves to: a refusal the
// checks made, or one request's cost (Cost, priced by Cards) and the limits
// it claims, spend windows first, each claiming that request's cost.
type Want struct {
	Refusal *AdmitDecision
	Cost    int64
	Cards   []byte
	// Spend are the request's spend windows; Limits its declared limits.
	Spend  []limits.Claim
	Limits []limits.Claim
	// Available is what the customer may spend after every live hold, and
	// CreditLine the overdraft the policy allows on it.
	Available, CreditLine, Outstanding int64
	capacity                           money.AdmissionCapacity
	resolved                           ResolvedPolicy
}

// ResolveWant makes the checks a lease makes when it issues a hold, and
// prices one request of the want, under the customer's money lock.
func (a *Admitter) ResolveWant(ctx context.Context, q *gen.Queries, req AdmitRequest, capacity money.AdmissionCapacity, now time.Time) (Want, error) {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return Want{}, err
	}
	var out Want
	if req.Usage != nil {
		if _, err := money.UsageEstimate(*req.Usage); err != nil {
			return Want{}, err
		}
		if req.EstimatedAmount, out.Cards, err = a.money.RateUsageEstimate(ctx, q, req.CustomerID, money.NormalizeCurrency(req.Currency), *req.Usage); err != nil {
			return Want{}, err
		}
	}
	g, refused, err := a.gates(ctx, q, req)
	if err != nil {
		return Want{}, err
	}
	if refused != nil {
		out.Refusal = refused
		return out, nil
	}
	out.Cost = req.EstimatedAmount
	if out.Limits, err = a.limitClaims(ctx, q, tid.UUID(), req); err != nil {
		return Want{}, err
	}
	if out.Spend, err = spendgate.SpendClaims(tid.UUID(), req.CustomerID.UUID(), strings.TrimSpace(req.Currency), g.policy, g.request, out.Cost, now); err != nil {
		return Want{}, err
	}
	out.Available, out.CreditLine, out.Outstanding = payerCapacity(capacity, g.resolved)
	out.capacity, out.resolved = capacity, g.resolved
	return out, nil
}

// BalanceRefusal is the deny code a want that does not fit the balance gets,
// as Admit answers it: insufficient_balance, insufficient_credit with a
// credit line, or outstanding_cap_reached when the policy gates on debt.
func (w Want) BalanceRefusal() string {
	code := money.DenyInsufficientBalance
	if w.CreditLine > 0 {
		code = money.DenyInsufficientCredit
	}
	if w.resolved.GatesOnOutstandingOwed() && w.CreditLine == 0 && w.Available <= 0 && w.Outstanding > 0 {
		code = DenyOutstandingCap
	}
	return code
}
