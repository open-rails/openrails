package admission

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
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
	gated                              gated
}

// ResolveWant makes the checks a lease makes when it issues a hold, and
// prices one request of the want, under the customer's money lock.
func (a *Admitter) ResolveWant(ctx context.Context, q *gen.Queries, req AdmitRequest, capacity money.AdmissionCapacity, now time.Time) (Want, error) {
	var cards []byte
	if req.Usage != nil {
		if _, err := money.UsageEstimate(*req.Usage); err != nil {
			return Want{}, err
		}
		var err error
		if req.EstimatedAmount, cards, err = a.money.RateUsageEstimate(ctx, q, req.CustomerID, money.NormalizeCurrency(req.Currency), *req.Usage); err != nil {
			return Want{}, err
		}
	}
	return a.resolve(ctx, q, req, capacity, now, cards, nil)
}

// ResolveHold makes a renewed hold's checks again for the invoker
// attributes it was resolved for and its event type ("" for host-stated),
// with the rate cards in force and no cost: the renewal sizes from traffic.
func (a *Admitter) ResolveHold(ctx context.Context, q *gen.Queries, req AdmitRequest, eventType string, capacity money.AdmissionCapacity, now time.Time) (Want, error) {
	req.Usage, req.EstimatedAmount = nil, 0
	var cards []byte
	if eventType != "" {
		var err error
		if cards, err = a.money.PinnedCards(ctx, q, eventType, money.NormalizeCurrency(req.Currency)); err != nil {
			return Want{}, err
		}
	}
	return a.resolve(ctx, q, req, capacity, now, cards, &eventType)
}

// resolve is a want's or a renewal's checks and claims; eventType names a
// renewal's, whose claims cost nothing.
func (a *Admitter) resolve(ctx context.Context, q *gen.Queries, req AdmitRequest, capacity money.AdmissionCapacity, now time.Time, cards []byte, eventType *string) (Want, error) {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return Want{}, err
	}
	out := Want{Cards: cards}
	g, refused, err := a.gates(ctx, q, req)
	if err != nil {
		return Want{}, err
	}
	if refused != nil {
		out.Refusal = refused
		return out, nil
	}
	out.Cost = req.EstimatedAmount
	if eventType != nil {
		out.Limits, err = a.limitClaimsFor(ctx, q, tid.UUID(), req, *eventType)
	} else {
		out.Limits, err = a.limitClaims(ctx, q, tid.UUID(), req)
	}
	if err != nil {
		return Want{}, err
	}
	if out.Spend, err = spendgate.SpendClaims(tid.UUID(), req.CustomerID.UUID(), strings.TrimSpace(req.Currency), g.policy, g.request, out.Cost, now); err != nil {
		return Want{}, err
	}
	out.Available, out.CreditLine, out.Outstanding = payerCapacity(capacity, g.resolved)
	out.capacity, out.resolved, out.gated = capacity, g.resolved, g
	return out, nil
}

// SpendAt are the want's spend windows at an instant, claiming nothing: the
// windows a hold rolled ahead takes.
func (w Want) SpendAt(merchantID, customer uuid.UUID, currency string, at time.Time) ([]limits.Claim, error) {
	return spendgate.SpendClaims(merchantID, customer, strings.TrimSpace(currency), w.gated.policy, w.gated.request, 0, at)
}

// MoneyTerms names the resolved policy and what it makes of the balance:
// invokers whose policies differ (a trust level's outstanding_cap, or its
// spend window of the same key with another cap) hold apart.
func (w Want) MoneyTerms() string {
	return fmt.Sprintf("%q:%s:%d", w.resolved.Name, w.resolved.Kind, w.resolved.OutstandingCapAmount)
}

// BalanceRefusal is the deny code a want that does not fit the balance gets,
// as Admit answers it: insufficient_balance, insufficient_credit with a
// credit line, or outstanding_cap_reached when the policy gates on debt.
func (w Want) BalanceRefusal() billing.DenyCode {
	code := billing.DenyInsufficientBalance
	if w.CreditLine > 0 {
		code = billing.DenyInsufficientCredit
	}
	if w.resolved.GatesOnOutstandingOwed() && w.CreditLine == 0 && w.Available <= 0 && w.Outstanding > 0 {
		code = billing.DenyOutstandingCapReached
	}
	return code
}
