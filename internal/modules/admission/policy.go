// Package admission implements OpenRails service admission for payer money
// capacity, delegated spend windows, and delegated wasted-spend cutoffs.
package admission

import (
	"context"
	"math"
	"slices"
	"time"

	"github.com/google/uuid"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/budgets"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
)

// BillingPolicyStore resolves a customer's billing policy (or#897): the named
// policies, the tier map and the default are the merchant's settings; the
// customer's own assignment is customers.billing_policy.
type BillingPolicyStore struct {
	db *db.DB
}

func NewBillingPolicyStore(database *db.DB) *BillingPolicyStore {
	return &BillingPolicyStore{db: database}
}

// ResolvedPolicy is the effective policy for one (payer, tier) plus the name it
// resolved through, so a denial can say WHICH policy refused.
//
// A zero ResolvedPolicy (no binding at all) is the merchant that has declared
// nothing: Kind is empty and the admission path falls back to the payer's own
// arrears credit limit under outstanding-cap semantics — the conservative
// reading, since it is the one that can refuse.
type ResolvedPolicy struct {
	Name string
	Kind models.BillingPolicyKind
	// OutstandingCapAmount is the declared credit line on unpaid arrears
	// (kind=outstanding_cap). Zero defers to the payer's own arrears limit.
	OutstandingCapAmount int64
	SpendWindows         []budgets.BudgetWindow
	PolicyCurrency       string
	// AccrualRateCapPerHour / AccrualRateWindowSeconds are the kind=accrual_rate_cap
	// quota: the ceiling on measured accrual in micros PER HOUR, and the lookback
	// the measurement smooths over.
	AccrualRateCapPerHour    int64
	AccrualRateWindowSeconds int64
	// BadSpendWindows are the #497 per-PAYER wasted-spend grace windows;
	// direct-payer overage is charged at report time.
	BadSpendWindows []models.BudgetWindowPolicy
	// CollectionThresholdAmount / DelinquencyGraceDays / DelinquencyAmountFloor
	// override the merchant-wide invoice policy for payers bound here. Nil defers
	// to the merchant-wide value — these are orthogonal to Kind, because when a
	// debt is chased is a different question from what may be owed.
	CollectionThresholdAmount *int64
	DelinquencyGraceDays      *int
	DelinquencyAmountFloor    *int64
}

// GatesOnOutstandingOwed reports whether unpaid arrears reduce this payer's
// admission headroom. This ONE branch is the whole difference between or#897's
// two seed businesses: the API business's $200 line is a ceiling on DEBT, while
// the cloud kinds cap NEW spend or the accrual RATE and let prior debt drive
// delinquency instead.
func (p ResolvedPolicy) GatesOnOutstandingOwed() bool {
	switch p.Kind {
	case models.BillingPolicyWindowSpendCap, models.BillingPolicyAccrualRateCap:
		return false
	default:
		return true
	}
}

// RateWindow is the effective accrual-rate lookback.
func (p ResolvedPolicy) RateWindow() time.Duration {
	seconds := p.AccrualRateWindowSeconds
	if seconds <= 0 {
		seconds = models.DefaultAccrualRateWindowSeconds
	}
	return time.Duration(seconds) * time.Second
}

// Resolve returns the effective policy for (payer, tier): the customer's own
// assignment while the settings still declare it, else its tier's, else the
// default. None yields a zero ResolvedPolicy, which the admission path reads as
// outstanding-cap semantics over the payer's own arrears credit limit.
func (s *BillingPolicyStore) Resolve(ctx context.Context, payer identity.CustomerID, tier string) (ResolvedPolicy, error) {
	r, err := s.resolver(ctx, []uuid.UUID{payer.UUID()})
	if err != nil {
		return ResolvedPolicy{}, err
	}
	return r.resolve(payer.UUID(), tier), nil
}

// policyResolver resolves payers' policies from one read of the merchant's
// settings and of the payers' own assignments.
type policyResolver struct {
	settings merchantconfig.Settings
	assigned map[uuid.UUID]string
}

func (s *BillingPolicyStore) resolver(ctx context.Context, payers []uuid.UUID) (*policyResolver, error) {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	settings, err := merchantconfig.NewStore(s.db).Settings(ctx)
	if err != nil {
		return nil, err
	}
	r := &policyResolver{settings: settings, assigned: map[uuid.UUID]string{}}
	ids := slices.DeleteFunc(slices.Clone(payers), func(id uuid.UUID) bool { return id == uuid.Nil })
	if len(settings.Policies) == 0 || len(ids) == 0 {
		return r, nil
	}
	err = s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		rows, err := s.db.Gen(ctx).ListCustomerBillingPolicyAssignments(ctx, gen.ListCustomerBillingPolicyAssignmentsParams{MerchantID: tid.UUID(),
			CustomerIds: ids, RowLimit: int32(min(len(ids), math.MaxInt32))}) // #nosec G115 -- clamped to int32
		for _, row := range rows {
			r.assigned[row.CustomerID] = row.PolicyName
		}
		return err
	})
	return r, err
}

// declares reports whether the merchant declares any policy.
func (r *policyResolver) declares() bool { return len(r.settings.Policies) > 0 }

func (r *policyResolver) resolve(payer uuid.UUID, tier string) ResolvedPolicy {
	name, body, ok, _ := r.settings.Policy(r.assigned[payer], tier)
	if !ok {
		return ResolvedPolicy{}
	}
	return ResolvedPolicy{
		Name:                      name,
		Kind:                      body.Kind,
		OutstandingCapAmount:      body.OutstandingCapAmount,
		SpendWindows:              toBudgetWindows(body.SpendWindows),
		PolicyCurrency:            body.PolicyCurrency,
		AccrualRateCapPerHour:     body.AccrualRateCapPerHour,
		AccrualRateWindowSeconds:  body.AccrualRateWindowSeconds,
		BadSpendWindows:           body.BadSpendWindows,
		CollectionThresholdAmount: body.CollectionThresholdAmount,
		DelinquencyGraceDays:      body.DelinquencyGraceDays,
		DelinquencyAmountFloor:    body.DelinquencyAmountFloor,
	}
}

func toBudgetWindows(ws []models.BudgetWindowPolicy) []budgets.BudgetWindow {
	out := make([]budgets.BudgetWindow, 0, len(ws))
	for _, w := range ws {
		out = append(out, budgets.BudgetWindow{Key: w.Key, WindowSeconds: w.WindowSeconds, Limit: w.Limit, Currency: w.Currency})
	}
	return out
}
