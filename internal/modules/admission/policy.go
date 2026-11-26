// Package admission implements OpenRails service admission for payer money
// capacity, delegated spend windows, and delegated wasted-spend cutoffs.
package admission

import (
	"context"
	"math"
	"slices"

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
// resolved through, so a denial can say WHICH policy refused. A zero
// ResolvedPolicy (no binding at all) sets no window. Its outstanding cap is
// the customer's credit line, which money resolves (AdmissionCapacity).
type ResolvedPolicy struct {
	Name           string
	SpendWindows   []budgets.BudgetWindow
	PolicyCurrency string
	// BadSpendWindows are the #497 per-PAYER wasted-spend grace windows;
	// direct-payer overage is charged at report time.
	BadSpendWindows []models.BudgetWindowPolicy
	// CollectionThresholdAmount / DelinquencyGraceDays / DelinquencyAmountFloor
	// override the merchant-wide invoice policy for payers bound here. Nil defers
	// to the merchant-wide value.
	CollectionThresholdAmount *int64
	DelinquencyGraceDays      *int
	DelinquencyAmountFloor    *int64
}

// Resolve returns the effective policy for (payer, tier): the customer's own
// assignment while the settings still declare it, else its tier's, else the
// default. None yields a zero ResolvedPolicy.
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
		SpendWindows:              toBudgetWindows(body.SpendWindows),
		PolicyCurrency:            body.PolicyCurrency,
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
