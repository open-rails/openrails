// Package admission implements OpenRails service admission for payer money
// capacity, delegated spend windows, and delegated wasted-spend cutoffs.
package admission

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/budgets"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// BillingPolicyStore is the or#897 policy registry: named policies plus the
// bindings that say which one applies to whom. It REPLACES payer_spend_limits,
// which was the same machine with one implicit meaning — a window cap — and no
// way to say that a different quantity is the one being capped.
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

// UpsertPolicy declares (or redeclares) one named policy. The body must already
// have passed merchantconfig.NormalizeBillingPolicy — both transports call it.
func (s *BillingPolicyStore) UpsertPolicy(ctx context.Context, name string, policy models.BillingPolicy) error {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	policyJSON, err := json.Marshal(policy)
	if err != nil {
		return fmt.Errorf("admission: encode billing policy: %w", err)
	}
	return s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		return s.db.Gen(ctx).UpsertBillingPolicy(ctx, gen.UpsertBillingPolicyParams{
			ID:         uuidutil.NewV7(),
			MerchantID: tid.UUID(),
			Name:       name,
			Policy:     policyJSON,
			CreatedAt:  now,
			UpdatedAt:  now,
		})
	})
}

// ListPolicies returns every declared policy, keyed by name.
func (s *BillingPolicyStore) ListPolicies(ctx context.Context) (map[string]models.BillingPolicy, error) {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]models.BillingPolicy{}
	err = s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		rows, err := s.db.Gen(ctx).ListBillingPolicies(ctx, tid.UUID())
		if err != nil {
			return err
		}
		for _, r := range rows {
			var body models.BillingPolicy
			if len(r.Policy) > 0 {
				if uerr := json.Unmarshal(r.Policy, &body); uerr != nil {
					return fmt.Errorf("admission: decode billing policy %q: %w", r.Name, uerr)
				}
			}
			out[r.Name] = body
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// BindPolicy points one rung at a policy name. A non-zero payer binds the
// per-customer rung; a non-empty tier binds the per-tier rung; neither binds the
// merchant default. Binding moves no money — it is the merchant's runtime lever.
func (s *BillingPolicyStore) BindPolicy(ctx context.Context, payer identity.CustomerID, tier, policyName string) error {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	tenantID := tid.UUID()
	now := time.Now().UTC()
	tier = strings.TrimSpace(tier)
	if !payer.IsZero() && tier != "" {
		return fmt.Errorf("admission: bind to a customer OR a tier, not both")
	}
	return s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		q := s.db.Gen(ctx)
		switch {
		case !payer.IsZero():
			// Materialize the payable customers row so the binding FK is satisfied
			// on first write (same reason the retired table needed it, #317).
			if _, err := db.EnsureCustomerID(ctx, s.db.Qx(ctx), tenantID, payer.UUID().String()); err != nil {
				return err
			}
			subjectID := payer.UUID()
			return q.UpsertBillingPolicyBindingCustomer(ctx, gen.UpsertBillingPolicyBindingCustomerParams{
				ID: uuidutil.NewV7(), MerchantID: tenantID, CustomerID: &subjectID,
				PolicyName: policyName, CreatedAt: now, UpdatedAt: now,
			})
		case tier != "":
			return q.UpsertBillingPolicyBindingTier(ctx, gen.UpsertBillingPolicyBindingTierParams{
				ID: uuidutil.NewV7(), MerchantID: tenantID, Tier: &tier,
				PolicyName: policyName, CreatedAt: now, UpdatedAt: now,
			})
		default:
			return q.UpsertBillingPolicyBindingDefault(ctx, gen.UpsertBillingPolicyBindingDefaultParams{
				ID: uuidutil.NewV7(), MerchantID: tenantID,
				PolicyName: policyName, CreatedAt: now, UpdatedAt: now,
			})
		}
	})
}

// ListDeclarativeBindings returns the DECLARATIVE rungs — the merchant default
// and the per-tier bindings. Per-customer bindings are excluded on purpose: they
// are runtime segmentation state (one row per bound customer), so listing them
// would scale with records on file rather than with configuration.
func (s *BillingPolicyStore) ListDeclarativeBindings(ctx context.Context) ([]models.BillingPolicyBinding, error) {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	var out []models.BillingPolicyBinding
	err = s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		rows, err := s.db.Gen(ctx).ListDeclarativeBillingPolicyBindings(ctx, tid.UUID())
		if err != nil {
			return err
		}
		for _, r := range rows {
			b := models.BillingPolicyBinding{
				ID: r.ID, MerchantID: r.MerchantID, CustomerID: r.CustomerID,
				PolicyName: r.PolicyName, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
			}
			if r.Tier != nil {
				b.Tier = *r.Tier
			}
			out = append(out, b)
		}
		return nil
	})
	return out, err
}

// Resolve returns the effective policy for (payer, tier). No binding yields a
// zero ResolvedPolicy, which the admission path reads as outstanding-cap
// semantics over the payer's own arrears credit limit.
func (s *BillingPolicyStore) Resolve(ctx context.Context, payer identity.CustomerID, tier string) (ResolvedPolicy, error) {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return ResolvedPolicy{}, err
	}
	tenantID := tid.UUID()
	subjectID := payer.UUID()
	var out ResolvedPolicy
	// The predicates are `= $n OR IS NULL`, so one read considers all three rungs
	// and the ORDER BY picks the most specific.
	err = s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		row, e := s.db.Gen(ctx).ResolveBillingPolicy(ctx, gen.ResolveBillingPolicyParams{
			MerchantID: tenantID, CustomerID: &subjectID, Tier: &tier,
		})
		if errors.Is(e, pgx.ErrNoRows) {
			return nil
		}
		if e != nil {
			return e
		}
		var body models.BillingPolicy
		if len(row.Policy) > 0 {
			if uerr := json.Unmarshal(row.Policy, &body); uerr != nil {
				return fmt.Errorf("admission: decode billing policy %q: %w", row.PolicyName, uerr)
			}
		}
		out = ResolvedPolicy{
			Name:                      row.PolicyName,
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
		return nil
	})
	if err != nil {
		return ResolvedPolicy{}, err
	}
	return out, nil
}

func toBudgetWindows(ws []models.BudgetWindowPolicy) []budgets.BudgetWindow {
	out := make([]budgets.BudgetWindow, 0, len(ws))
	for _, w := range ws {
		out = append(out, budgets.BudgetWindow{Key: w.Key, WindowSeconds: w.WindowSeconds, Limit: w.Limit, Currency: w.Currency})
	}
	return out
}
