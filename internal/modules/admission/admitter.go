// Package admission implements OpenRails service admission: the payer money
// affordability and spend-cap gate, usage limits, and the delegated
// wasted-spend cutoff.
//
// Admission and settlement share request holds under the payer money lock.
// Redis is used only by independent rate and abuse signals, never as financial truth.
package admission

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/admission/spendgate"
	"github.com/open-rails/openrails/internal/modules/delinquency"
	"github.com/open-rails/openrails/internal/modules/limits"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// DefaultTrustLevel is assigned to actors with no explicit trust level — the
// new-account low default (#300): start at the lowest level until graduated.
const DefaultTrustLevel = "free"

// Admitter is the service-admit money gate.
type Admitter struct {
	money  *money.MoneyService
	gate   *spendgate.Gate
	loader *SpendgatePolicyLoader

	// delinquency is the optional or#878 arrears delinquency gate; nil disables
	// it (the amount cap still applies — the two axes are independent).
	delinquency *delinquency.Service

	// failedCutoff is the optional delegated-invoker failed-usage cutoff (#497);
	// nil disables it.
	failedCutoff FailedUsageCutoff

	// rates is the optional or#897 accrual-rate meter; nil makes an
	// accrual_rate_cap policy unenforceable, so Admit refuses rather than
	// silently admitting past a quota nobody is measuring.
	rates *AccrualRateMeter
}

// WithAccrualRateMeter enables the or#897 accrual_rate_cap quota.
func (a *Admitter) WithAccrualRateMeter(m *AccrualRateMeter) *Admitter {
	a.rates = m
	return a
}

// NewAdmitter builds admission over durable SQL reservations and merchant policy.
func NewAdmitter(moneySvc *money.MoneyService, gate *spendgate.Gate, loader *SpendgatePolicyLoader) *Admitter {
	return &Admitter{money: moneySvc, gate: gate, loader: loader}
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

// AdmitRequest is one admission decision input.
type AdmitRequest struct {
	CustomerID  identity.CustomerID // the merchant subject
	Invoker     string              // canonical invoker: user:<id> / apiKey:<key_id> / <issuer>:<sub>
	InvokerType string              // "customer" for the customer's own credential; empty/other = delegated
	TrustLevel  string              // optional payer trust level; empty resolves from OpenRails money state
	Resource    string              // caller-supplied resource string for host-side attribution only
	Roles       []uuid.UUID         // immutable role UUIDs the invoker holds (#473)

	Currency        string
	EstimatedAmount int64
	// Usage is the usage estimate the rate cards price into the hold, in place
	// of EstimatedAmount.
	Usage *billing.UsageEstimate
	// AccrualRateDeltaPerHour is the or#897 PROSPECTIVE rate this request would
	// add, in micros per hour — "the VM I am about to start burns $2/hour". Only
	// the host knows it, because only the host knows what it is about to deploy.
	// Zero means "this request adds no ongoing rate", which is the right answer
	// for one-shot work and leaves an accrual_rate_cap payer gated on what is
	// already running.
	AccrualRateDeltaPerHour int64
	Source                  string    // idempotency namespace (e.g. "usage")
	SourceID                string    // idempotency id (request id) — the hold key
	ExpiresAt               time.Time // the owner's declared job deadline; REQUIRED when EstimatedAmount > 0 and no lease owns the hold
	// LeaseID is the gate lease that owns the hold: it lives while the lease
	// does.
	LeaseID *uuid.UUID
}

// AdmitDecision is the unified outcome.
type AdmitDecision struct {
	Allowed bool
	// EstimatedAmount is what the admission holds: the host's estimate, or
	// what its usage estimate was priced at.
	EstimatedAmount int64
	BlockedBy       string // "budget" | "abuse" | "money" | "limit" | ""
	DenyCode        billing.DenyCode
	// Limit and ResetsAt name a limit_reached refusal's limit and when it
	// resets.
	Limit    string
	ResetsAt *time.Time
	// AvailableAmount is the payer's spendable balance (settled − money-window
	// reservations) the affordability gate was evaluated against; HeldAmount is the
	// total in-flight reservation after this admit. StartCapacity = Available − Held.
	AvailableAmount int64
	HeldAmount      int64
	// RetryAfterSeconds is when the blocking budget window next resets (0 unless
	// a window was the binding gate). Hosts stamp it as the 429 Retry-After.
	RetryAfterSeconds int64
	HoldExpiresAt     *time.Time
	Replayed          bool
	State             string
}

// Admit resolves the trust level, enforces the delegated wasted-spend cutoff,
// then checks affordability and spend windows and records the reservation in
// one transaction under the payer money lock.
func (a *Admitter) Admit(ctx context.Context, req AdmitRequest) (AdmitDecision, error) {
	if a == nil || a.money == nil || a.gate == nil || a.loader == nil {
		return AdmitDecision{}, fmt.Errorf("admission service is not initialized")
	}
	if err := spendgate.ValidateRequest(req.SourceID, req.EstimatedAmount, req.AccrualRateDeltaPerHour); err != nil {
		return AdmitDecision{}, err
	}
	req.SourceID = strings.TrimSpace(req.SourceID)
	var result AdmitDecision
	err := a.money.WithLockedAdmissionCapacity(ctx, req.CustomerID, req.Currency, func(ctx context.Context, d *db.DB, capacity money.AdmissionCapacity) error {
		bound := *a
		bound.money = a.money.WithDatabase(d)
		bound.loader = NewSpendgatePolicyLoader(NewBillingPolicyStore(d), a.loader.fx)
		if a.rates != nil {
			rates := *a.rates
			rates.db = d
			rates.now = a.gate.Now
			bound.rates = &rates
		}
		var err error
		result, err = bound.admitLocked(ctx, d.Gen(ctx), req, capacity)
		return err
	})
	return result, err
}

func (a *Admitter) admitLocked(ctx context.Context, q *gen.Queries, req AdmitRequest, capacity money.AdmissionCapacity) (AdmitDecision, error) {
	tid, err := merchant.Require(ctx) // #336: no default merchant
	if err != nil {
		return AdmitDecision{}, err
	}

	terms := spendgate.Terms{Invoker: req.Invoker, InvokerType: req.InvokerType, TrustLevel: req.TrustLevel,
		Roles: roleStrings(req.Roles), Resource: req.Resource, Source: req.Source, AccrualRateDeltaPerHour: req.AccrualRateDeltaPerHour}
	var estimate []byte
	if req.Usage != nil {
		if estimate, err = money.UsageEstimate(*req.Usage); err != nil {
			return AdmitDecision{}, err
		}
	}
	replay, err := a.gate.CheckIdentity(ctx, q, spendgate.AdmitInput{Customer: req.CustomerID.UUID(), Currency: req.Currency,
		RequestID: req.SourceID, Cost: req.EstimatedAmount, ExpiresAt: req.ExpiresAt, Terms: terms, EstimateUsage: estimate, LeaseID: req.LeaseID})
	if err != nil {
		var conflict *spendgate.Conflict
		if errors.As(err, &conflict) {
			return AdmitDecision{}, fmt.Errorf("%w: %w", money.ErrIdempotencyKeyReused, err)
		}
		return AdmitDecision{}, err
	}
	if replay != nil {
		return AdmitDecision{Allowed: true, EstimatedAmount: replay.EstimatedAmount, AvailableAmount: replay.AvailableAmount,
			HoldExpiresAt: replay.ExpiresAt, Replayed: true, State: replay.State}, nil
	}
	if req.LeaseID != nil {
		lease, err := q.GetLease(ctx, gen.GetLeaseParams{MerchantID: tid.UUID(), ID: *req.LeaseID})
		if errors.Is(err, pgx.ErrNoRows) || err == nil && lease.ClosedAt != nil {
			return AdmitDecision{}, apperr.New(http.StatusConflict, billing.CodeLeaseNotHeld, "the lease is closed or never synced").WithParam("lease_id")
		}
		if err != nil {
			return AdmitDecision{}, err
		}
	}

	// A usage estimate is priced at the customer's current period quantities
	// by the cards it pins.
	var pinned []byte
	if req.Usage != nil {
		if req.EstimatedAmount, pinned, err = a.money.RateUsageEstimate(ctx, q, req.CustomerID, money.NormalizeCurrency(req.Currency), *req.Usage); err != nil {
			return AdmitDecision{}, err
		}
	}

	g, refused, err := a.gates(ctx, q, req)
	if err != nil || refused != nil {
		if refused != nil {
			return *refused, nil
		}
		return AdmitDecision{}, err
	}
	resolved, policy, sgReq := g.resolved, g.policy, g.request

	// Usage limits the request counts toward: those in its currency claim its
	// money estimate, named units what their counts make of its usage.
	claims, err := a.limitClaims(ctx, q, tid.UUID(), req)
	if err != nil {
		return AdmitDecision{}, err
	}

	available, creditLine, outstanding := payerCapacity(capacity, resolved)
	input := spendgate.AdmitInput{
		Customer: req.CustomerID.UUID(), Currency: req.Currency, RequestID: req.SourceID,
		Cost: req.EstimatedAmount, ExpiresAt: req.ExpiresAt,
		AccountBalance: available, CreditLimit: creditLine, Policy: policy, Request: sgReq,
		EstimateUsage: estimate, RateCards: pinned,
		Terms: spendgate.Terms{Invoker: req.Invoker, InvokerType: req.InvokerType, TrustLevel: req.TrustLevel,
			Roles: roleStrings(req.Roles), Resource: req.Resource, Source: req.Source, AccrualRateDeltaPerHour: req.AccrualRateDeltaPerHour},
		Limits:  claims,
		LeaseID: req.LeaseID,
	}
	dec, err := a.gate.Admit(ctx, q, input)
	if err != nil {
		var conflict *spendgate.Conflict
		if errors.As(err, &conflict) {
			return AdmitDecision{}, fmt.Errorf("%w: %w", money.ErrIdempotencyKeyReused, err)
		}
		if errors.Is(err, spendgate.ErrExpired) {
			return AdmitDecision{}, ErrHoldDeadlinePassed
		}
		if errors.Is(err, spendgate.ErrDeadlineRequired) {
			return AdmitDecision{}, ErrHoldDeadlineRequired
		}
		return AdmitDecision{}, err
	}
	if dec.Allowed {
		return AdmitDecision{Allowed: true, EstimatedAmount: dec.EstimatedAmount, AvailableAmount: dec.AvailableAmount, HoldExpiresAt: dec.ExpiresAt, Replayed: dec.Replayed, State: dec.State}, nil
	}
	if dec.BlockedLimit != nil {
		out := AdmitDecision{BlockedBy: "limit", DenyCode: billing.DenyLimitReached, EstimatedAmount: req.EstimatedAmount, AvailableAmount: available,
			Limit: dec.BlockedLimit.Key, ResetsAt: dec.BlockedLimit.ResetsAt}
		if out.ResetsAt != nil {
			out.RetryAfterSeconds = ceilSeconds(out.ResetsAt.Sub(a.gate.Now()))
		}
		return out, nil
	}
	code, blocked := billing.DenyBudgetExceeded, "budget"
	if dec.BlockedBalance {
		if capacity.HardCreditLimit && outstanding > 0 && canFitAfterDebtSettlement(capacity, resolved, req.EstimatedAmount) {
			if err := a.money.RequestUsageCollection(ctx, q, req.CustomerID, req.Currency); err != nil {
				return AdmitDecision{}, err
			}
		}
		code, blocked = billing.DenyInsufficientBalance, "money"
		if creditLine > 0 {
			code = billing.DenyInsufficientCredit
		}
		if resolved.GatesOnOutstandingOwed() && creditLine == 0 && available <= 0 && outstanding > 0 {
			code = billing.DenyOutstandingCapReached
		}
	}
	return AdmitDecision{BlockedBy: blocked, DenyCode: code, EstimatedAmount: req.EstimatedAmount, AvailableAmount: available,
		RetryAfterSeconds: ceilSeconds(dec.RetryAfter)}, nil
}

// gated is a request past the checks admission makes before it weighs any
// limit: its trust level, its resolved policy and the spend windows it loads.
type gated struct {
	trustLevel string
	request    spendgate.Request
	// resolved is the policy with its amounts in the request's currency.
	resolved ResolvedPolicy
	policy   spendgate.Policy
}

// gates resolves the request's trust level and policy and makes the checks
// that refuse it whatever it costs: the delegated failed-usage cutoff,
// delinquency, the delegated-invoker refusal and the accrual-rate cap. A
// lease makes them when it issues a hold.
func (a *Admitter) gates(ctx context.Context, q *gen.Queries, req AdmitRequest) (gated, *AdmitDecision, error) {
	trustLevel := a.trustLevel(ctx, req)

	// Delegated-invoker failed-usage cutoff (#497): direct payer credentials are
	// not cut off here (their failures past grace are charged when recorded).
	if a.failedCutoff != nil && !identity.IsDirectPayerInvoker(req.InvokerType) && req.Invoker != "" {
		over, werr := a.failedCutoff.CutoffReached(ctx, req.CustomerID, req.Invoker, money.NormalizeCurrency(req.Currency))
		if werr != nil {
			return gated{}, nil, werr
		}
		if over {
			return gated{}, &AdmitDecision{Allowed: false, BlockedBy: "abuse", DenyCode: billing.DenyFailureRateLimited}, nil
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
			return gated{}, &AdmitDecision{Allowed: false, BlockedBy: "money", DenyCode: billing.DenyDelinquentUnpaidOrder}, nil
		}
	}

	// or#897: the merchant's bound billing policy is resolved FIRST, because its
	// KIND decides whether prior debt reduces this payer's headroom at all. The
	// merchant chose which policy binds to this payer; OpenRails measures and
	// enforces it from the transaction's locked merchant settings snapshot.
	sgReq, _, resolved, policy, err := a.policyOf(ctx, req, trustLevel)
	if err != nil {
		return gated{}, nil, err
	}

	// Only the customer's own credential spends its balance: nothing grants a
	// delegated invoker spend.
	if !identity.IsDirectPayerInvoker(req.InvokerType) {
		return gated{}, &AdmitDecision{Allowed: false, BlockedBy: "budget", DenyCode: billing.DenyDelegatedSpendNotAllowed}, nil
	}

	// or#897 accrual rate cap — the cloud quota. Asked BEFORE affordability
	// because it is a different question: not "can this payer pay for one more
	// request" but "would what it is about to deploy push it past the rate it is
	// allowed to burn at". Measured from reported usage over the policy's
	// lookback; the host supplies what it is about to add.
	if resolved.Kind == models.BillingPolicyAccrualRateCap {
		if a.rates == nil {
			// Fail CLOSED, loudly. A quota with no meter behind it is not a
			// relaxed quota, it is a lie: the merchant declared a ceiling and
			// would never learn it was not enforced.
			return gated{}, nil, fmt.Errorf("admission: policy %q is accrual_rate_cap but no accrual-rate meter is wired", resolved.Name)
		}
		measured, rerr := a.rates.MeasuredRatePerHour(ctx, req.CustomerID, req.Currency, resolved.RateWindow())
		if rerr != nil {
			return gated{}, nil, rerr
		}
		if req.AccrualRateDeltaPerHour > resolved.AccrualRateCapPerHour || measured > resolved.AccrualRateCapPerHour-req.AccrualRateDeltaPerHour {
			return gated{}, &AdmitDecision{Allowed: false, BlockedBy: "budget", DenyCode: billing.DenyAccrualRateCapReached}, nil
		}
	}

	return gated{trustLevel: trustLevel, request: sgReq, resolved: resolved, policy: policy}, nil, nil
}

// trustLevel is the request's trust level: explicit, else graduated, else
// the lowest default.
func (a *Admitter) trustLevel(ctx context.Context, req AdmitRequest) string {
	if req.TrustLevel != "" {
		return req.TrustLevel
	}
	if t, err := a.money.GetTrustLevel(ctx, req.CustomerID, req.Currency); err == nil && t != "" {
		return t
	}
	return DefaultTrustLevel
}

// policyOf is the billing policy bound to the request's customer and trust
// level, in the request's currency, with its spend windows loaded.
func (a *Admitter) policyOf(ctx context.Context, req AdmitRequest, trustLevel string) (spendgate.Request, ResolvedPolicy, ResolvedPolicy, spendgate.Policy, error) {
	sgReq := spendgate.Request{Invoker: req.Invoker, TrustLevel: trustLevel, Roles: roleStrings(req.Roles), Measure: req.Resource}
	declared, err := a.loader.ResolvePolicy(ctx, req.CustomerID, trustLevel)
	if err != nil {
		return sgReq, ResolvedPolicy{}, ResolvedPolicy{}, spendgate.Policy{}, err
	}
	resolved, err := a.loader.InCurrency(ctx, declared, req.Currency)
	if err != nil {
		return sgReq, declared, ResolvedPolicy{}, spendgate.Policy{}, err
	}
	policy, err := a.loader.Load(ctx, req.Currency, resolved)
	return sgReq, declared, resolved, policy, err
}

func ceilSeconds(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	seconds := int64(d / time.Second)
	if d%time.Second > 0 {
		seconds++
	}
	return seconds
}

// limitClaims are the request's claims on the merchant's declared limits.
func (a *Admitter) limitClaims(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, req AdmitRequest) ([]limits.Claim, error) {
	defs, err := limits.Definitions(ctx, q, merchantID)
	if err != nil || len(defs) == 0 {
		return nil, err
	}
	meters, err := limits.Meters(ctx, q, merchantID)
	if err != nil {
		return nil, err
	}
	var usage *limits.Usage
	if req.Usage != nil {
		usage = &limits.Usage{EventType: strings.TrimSpace(req.Usage.EventType), Dimensions: req.Usage.Dimensions, Metadata: req.Usage.Metadata}
	}
	reader := limits.Customer{Q: q, MerchantID: merchantID, CustomerID: req.CustomerID.UUID(), Now: a.gate.Now(),
		Period: func(ctx context.Context, unit string, at time.Time) (time.Time, time.Time, error) {
			return a.money.PricingPeriod(ctx, q, merchantID, req.CustomerID.UUID(), unit, at)
		}}
	return reader.Claims(ctx, defs, meters, money.NormalizeCurrency(req.Currency), req.EstimatedAmount, usage)
}

// limitClaimsFor are a renewed hold's claims on the declared limits a
// request of eventType counts toward, with no cost.
func (a *Admitter) limitClaimsFor(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, req AdmitRequest, eventType string) ([]limits.Claim, error) {
	defs, err := limits.Definitions(ctx, q, merchantID)
	if err != nil || len(defs) == 0 {
		return nil, err
	}
	meters, err := limits.Meters(ctx, q, merchantID)
	if err != nil {
		return nil, err
	}
	reader := limits.Customer{Q: q, MerchantID: merchantID, CustomerID: req.CustomerID.UUID(), Now: a.gate.Now(),
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

// ErrHoldDeadlineRequired: a request that places a money hold must say how
// long the job owning it will run (xs-007 row 33). The hold used to default
// to one hour, after which `held` was recomputed without it and the still-
// running job's spend was admitted twice over. There is no default here: the
// caller's deadline is the only source for "abandoned", and a job that runs
// past it re-declares through ExtendHold.
var ErrHoldDeadlineRequired = errors.New("admission: expires_at is required when estimated_amount places a hold")

// ErrHoldDeadlinePassed: the declared deadline is already in the past.
var ErrHoldDeadlinePassed = errors.New("admission: expires_at is already in the past")
