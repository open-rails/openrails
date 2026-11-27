// Package admission resolves what a gate's view decides a customer's
// requests against: the payer's money policy and its spend windows, the
// usage limits a request claims, and the checks made whatever a request
// costs (delegated invokers, failed-usage cutoffs, delinquency). Nothing is
// held: gates decide locally.
package admission

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/admission/spendgate"
	"github.com/open-rails/openrails/internal/modules/delinquency"
	"github.com/open-rails/openrails/internal/modules/limits"
	"github.com/open-rails/openrails/internal/modules/money"
)

// Admitter resolves a customer's money policy and limits for gates' views.
type Admitter struct {
	money  *money.MoneyService
	loader *SpendgatePolicyLoader
	now    func() time.Time

	// delinquency is the optional or#878 arrears delinquency gate; nil disables
	// it (the amount cap still applies — the two axes are independent).
	delinquency *delinquency.Service

	// failedCutoff is the optional delegated-invoker failed-usage cutoff (#497);
	// nil disables it.
	failedCutoff FailedUsageCutoff
}

// NewAdmitter resolves merchant policy on now's clock.
func NewAdmitter(moneySvc *money.MoneyService, loader *SpendgatePolicyLoader, now func() time.Time) *Admitter {
	return &Admitter{money: moneySvc, loader: loader, now: now}
}

// WithDelinquency enables the or#878 arrears delinquency admit gate.
func (a *Admitter) WithDelinquency(g *delinquency.Service) *Admitter {
	a.delinquency = g
	return a
}

// FailedUsageCutoff answers whether a delegated invoker's failed usage has
// reached its cutoff.
type FailedUsageCutoff interface {
	CutoffReached(ctx context.Context, customer identity.CustomerID, invoker, currency string) (bool, error)
}

// WithFailedUsageCutoff enables the delegated-invoker failed-usage admit gate (#497).
func (a *Admitter) WithFailedUsageCutoff(c FailedUsageCutoff) *Admitter {
	a.failedCutoff = c
	return a
}

// AdmitRequest is the attributes of a customer's requests that policy
// resolution reads.
type AdmitRequest struct {
	CustomerID  identity.CustomerID // the merchant subject
	Invoker     string              // canonical invoker: user:<id> / apiKey:<key_id> / <issuer>:<sub>
	InvokerType string              // "customer" for the customer's own credential; empty/other = delegated
	TrustLevel  string              // optional payer trust level; empty resolves from OpenRails money state
	Resource    string              // caller-supplied resource string for host-side attribution only
	Roles       []uuid.UUID         // immutable role UUIDs the invoker holds (#473)
	Currency    string
}

// AdmitDecision is a refusal made whatever a request costs.
type AdmitDecision struct {
	BlockedBy string // "budget" | "abuse" | "money"
	DenyCode  billing.DenyCode
	// RetryAfterSeconds is when the refusal clears, when it does.
	RetryAfterSeconds int64
}

// gated is a request past the checks admission makes before it weighs any
// limit: its trust level and the spend windows its policy loads.
type gated struct {
	trust   billing.EffectiveTrustLevel
	request spendgate.Request
	policy  spendgate.Policy
}

// gates resolves the request's trust level and policy and makes the checks
// that refuse it whatever it costs: the delegated failed-usage cutoff,
// delinquency and the delegated-invoker refusal. A lease makes them when it
// issues a hold.
func (a *Admitter) gates(ctx context.Context, q *gen.Queries, req AdmitRequest) (gated, *AdmitDecision, error) {
	trust, err := a.trustLevel(ctx, req)
	if err != nil {
		return gated{}, nil, err
	}
	trusted := gated{trust: trust}

	// Delegated-invoker failed-usage cutoff (#497): direct payer credentials are
	// not cut off here (their failures past grace are charged when recorded).
	if a.failedCutoff != nil && !identity.IsDirectPayerInvoker(req.InvokerType) && req.Invoker != "" {
		over, werr := a.failedCutoff.CutoffReached(ctx, req.CustomerID, req.Invoker, money.NormalizeCurrency(req.Currency))
		if werr != nil {
			return gated{}, nil, werr
		}
		if over {
			return trusted, &AdmitDecision{BlockedBy: "abuse", DenyCode: billing.DenyFailureRateLimited}, nil
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
			return gated{}, nil, derr
		}
		if delinquent {
			return trusted, &AdmitDecision{BlockedBy: "money", DenyCode: billing.DenyDelinquentUnpaidInvoice}, nil
		}
	}

	// or#897: the merchant chose which policy binds to this payer; OpenRails
	// measures and enforces its spend windows from the transaction's locked
	// merchant settings snapshot.
	sgReq, policy, err := a.policyOf(ctx, req, trust.TrustLevel)
	if err != nil {
		return gated{}, nil, err
	}

	// Only the customer's own credential spends its balance: nothing grants a
	// delegated invoker spend.
	if !identity.IsDirectPayerInvoker(req.InvokerType) {
		return trusted, &AdmitDecision{BlockedBy: "budget", DenyCode: billing.DenyDelegatedSpendNotAllowed}, nil
	}

	return gated{trust: trust, request: sgReq, policy: policy}, nil, nil
}

// trustLevel is the request's trust level: its own, else the customer's
// effective one (explicit, else by the merchant's tier rules, else free).
func (a *Admitter) trustLevel(ctx context.Context, req AdmitRequest) (billing.EffectiveTrustLevel, error) {
	if req.TrustLevel != "" {
		return billing.EffectiveTrustLevel{Currency: catalog.Currency(money.NormalizeCurrency(req.Currency)), TrustLevel: req.TrustLevel, Source: billing.TrustLevelExplicit}, nil
	}
	return a.money.TrustLevel(ctx, req.CustomerID, req.Currency)
}

// policyOf is the spend windows of the billing policy bound to the request's
// customer and trust level, in the request's currency.
func (a *Admitter) policyOf(ctx context.Context, req AdmitRequest, trustLevel string) (spendgate.Request, spendgate.Policy, error) {
	sgReq := spendgate.Request{Invoker: req.Invoker, TrustLevel: trustLevel, Roles: roleStrings(req.Roles), Measure: req.Resource}
	resolved, err := a.loader.ResolvePolicy(ctx, req.CustomerID, trustLevel)
	if err != nil {
		return sgReq, spendgate.Policy{}, err
	}
	policy, err := a.loader.Load(ctx, req.Currency, resolved)
	return sgReq, policy, err
}

// limitClaimsFor are the claims on the declared limits a request of
// eventType counts toward, with no cost.
func (a *Admitter) limitClaimsFor(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, req AdmitRequest, eventType string) ([]limits.Claim, error) {
	defs, err := limits.Definitions(ctx, q, merchantID)
	if err != nil || len(defs) == 0 {
		return nil, err
	}
	meters, err := limits.Meters(ctx, q, merchantID)
	if err != nil {
		return nil, err
	}
	reader := limits.Customer{Q: q, MerchantID: merchantID, CustomerID: req.CustomerID.UUID(), Now: a.now(),
		Period: func(ctx context.Context, unit string, at time.Time) (time.Time, time.Time, error) {
			return a.money.PricingPeriod(ctx, q, merchantID, req.CustomerID.UUID(), unit, at)
		}}
	return reader.ClaimsFor(ctx, defs, meters, money.NormalizeCurrency(req.Currency), eventType)
}

// roleStrings maps the invoker's role UUIDs to the strings the spendgate role
// scope matches on (a role scope key is the role uuid string).
func roleStrings(roles []uuid.UUID) []string {
	out := make([]string, 0, len(roles))
	for _, r := range roles {
		if r != uuid.Nil {
			out = append(out, r.String())
		}
	}
	return out
}
