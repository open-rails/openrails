// Package admission implements OpenRails service admission for payer money
// capacity, delegated spend windows, and delegated wasted-spend cutoffs.
package admission

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/modules/budgets"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
)

// PayerSpendLimitStore loads + stores per-payer, per-tier admission policies
// (openrails.payer_spend_limits). Account money state stays in money_accounts.
type PayerSpendLimitStore struct {
	db *db.DB
}

func NewPayerSpendLimitStore(database *db.DB) *PayerSpendLimitStore {
	return &PayerSpendLimitStore{db: database}
}

// PayerSpendLimits is a tier's enforceable money policy.
type PayerSpendLimits struct {
	BudgetWindows  []budgets.BudgetWindow
	PolicyCurrency string
	// BadSpendWindows are the #497 per-PAYER direct-credential wasted-spend grace
	// windows for this tier; direct-payer overage is charged at report time.
	BadSpendWindows []models.BudgetWindowPolicy
}

// UpsertPayerSpendLimitsFull sets the full tier money policy.
// A ZERO payer writes the TENANT-WIDE DEFAULT policy for the tier (#477): the
// platform capacity ladder declared once, applied to every payer at that tier
// (selected by GetPayerSpendLimits when the payer has no own override).
func (s *PayerSpendLimitStore) UpsertPayerSpendLimitsFull(ctx context.Context, payer identity.CustomerID, tier string, policy models.TierMoneyPolicy) error {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	tenantID := tid.UUID()
	now := time.Now().UTC()
	return s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		policyJSON, err := json.Marshal(policy)
		if err != nil {
			return fmt.Errorf("admission: encode tier policy: %w", err)
		}
		// Merchant-wide default (#477): no subject row to materialize, NULL subject.
		if payer.IsZero() {
			return s.db.Gen(ctx).UpsertPayerSpendLimitDefault(ctx, gen.UpsertPayerSpendLimitDefaultParams{
				ID:            uuidutil.NewV7(),
				MerchantID:    tenantID,
				Tier:          tier,
				Policy:        policyJSON,
				PolicyVersion: 1,
				CreatedAt:     now,
				UpdatedAt:     now,
			})
		}
		// Per-subject override: materialize the payable customers row so the
		// payer_spend_limits FK (migration 076) is satisfied on first write (#317).
		if _, err := repo.EnsureCustomerID(ctx, s.db.Qx(ctx), tenantID, payer.UUID().String()); err != nil {
			return err
		}
		subjectID := payer.UUID()
		return s.db.Gen(ctx).UpsertPayerSpendLimit(ctx, gen.UpsertPayerSpendLimitParams{
			ID:            uuidutil.NewV7(),
			MerchantID:    tenantID,
			CustomerID:    &subjectID,
			Tier:          tier,
			Policy:        policyJSON,
			PolicyVersion: 1,
			CreatedAt:     now,
			UpdatedAt:     now,
		})
	})
}

// GetPayerSpendLimits returns the enforceable money policy for (payer, tier). A
// missing row yields an empty policy.
func (s *PayerSpendLimitStore) GetPayerSpendLimits(ctx context.Context, payer identity.CustomerID, tier string) (PayerSpendLimits, error) {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return PayerSpendLimits{}, err
	}
	tenantID := tid.UUID()
	row := new(models.TierPolicy)
	found := false
	// GetPayerSpendLimits resolves the payer's own override else the merchant-wide default
	// (NULL subject, #477). The query's subject predicate is `= $2 OR IS NULL`, so
	// passing the payer uuid matches both the override and the default.
	subjectID := payer.UUID()
	err = s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		genRow, e := s.db.Gen(ctx).GetPayerSpendLimits(ctx, gen.GetPayerSpendLimitsParams{
			MerchantID: tenantID, CustomerID: &subjectID, Tier: tier,
		})
		if errors.Is(e, pgx.ErrNoRows) {
			return nil
		}
		if e != nil {
			return e
		}
		if len(genRow.Policy) > 0 {
			if uerr := json.Unmarshal(genRow.Policy, &row.Policy); uerr != nil {
				return fmt.Errorf("admission: decode tier policy: %w", uerr)
			}
		}
		found = true
		return nil
	})
	if err != nil {
		return PayerSpendLimits{}, err
	}
	if !found {
		return PayerSpendLimits{}, nil
	}
	return PayerSpendLimits{
		BudgetWindows:   toBudgetWindows(row.Policy.BudgetWindows),
		PolicyCurrency:  row.Policy.PolicyCurrency,
		BadSpendWindows: row.Policy.BadSpendWindows,
	}, nil
}

// InvokerSpendLimitStore reads/writes per-invoker spend limits (#473/#517) —
// {scope, scope_key, windows[]} rows in openrails.invoker_spend_limits. These are
// the payer's own caps on its delegated invokers/roles (payer-set only). The admit
// path (LoadAll) reads every scope to compose the verdict.
type InvokerSpendLimitStore struct {
	db *db.DB
}

func NewInvokerSpendLimitStore(database *db.DB) *InvokerSpendLimitStore {
	return &InvokerSpendLimitStore{db: database}
}

// InvokerSpendLimit is one stored limit: {scope, scopeKey, windows[]}. scopeKey is
// the immutable role uuid (scope=role) / invoker string (scope=invoker) / tier key
// (scope=invoker_tier).
type InvokerSpendLimit struct {
	Scope    string
	ScopeKey string
	Windows  []models.BudgetWindowPolicy
}

func invokerSpendLimitFromGen(r gen.OpenrailsInvokerSpendLimit) (InvokerSpendLimit, error) {
	p := InvokerSpendLimit{Scope: r.Scope, ScopeKey: r.ScopeKey}
	if len(r.Windows) > 0 {
		if err := json.Unmarshal(r.Windows, &p.Windows); err != nil {
			return InvokerSpendLimit{}, fmt.Errorf("admission: decode invoker spend-limit windows: %w", err)
		}
	}
	return p, nil
}

// LoadAll returns every invoker spend limit for a payer (the admit path composes
// all of them). Returns nil when none exist.
func (s *InvokerSpendLimitStore) LoadAll(ctx context.Context, payer identity.CustomerID) ([]InvokerSpendLimit, error) {
	tid, terr := merchant.Require(ctx) // #336/#474: no default merchant
	if terr != nil {
		return nil, terr
	}
	tenantID := tid.UUID()
	var out []InvokerSpendLimit
	err := s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		rows, e := s.db.Gen(ctx).ListInvokerSpendLimits(ctx, gen.ListInvokerSpendLimitsParams{
			MerchantID: tenantID, CustomerID: payer.UUID(),
		})
		if e != nil {
			return e
		}
		for _, r := range rows {
			p, derr := invokerSpendLimitFromGen(r)
			if derr != nil {
				return derr
			}
			out = append(out, p)
		}
		return nil
	})
	return out, err
}

// Upsert writes one invoker spend limit (payer-set).
func (s *InvokerSpendLimitStore) Upsert(ctx context.Context, payer identity.CustomerID, p InvokerSpendLimit) error {
	tid, terr := merchant.Require(ctx) // #336/#474: no default merchant
	if terr != nil {
		return terr
	}
	tenantID := tid.UUID()
	now := time.Now().UTC()
	windowsJSON, err := json.Marshal(p.Windows)
	if err != nil {
		return fmt.Errorf("admission: encode invoker spend-limit windows: %w", err)
	}
	return s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		if _, err := repo.EnsureCustomerID(ctx, s.db.Qx(ctx), tenantID, payer.UUID().String()); err != nil {
			return err
		}
		return s.db.Gen(ctx).UpsertInvokerSpendLimit(ctx, gen.UpsertInvokerSpendLimitParams{
			ID:            uuidutil.NewV7(),
			MerchantID:    tenantID,
			CustomerID:    payer.UUID(),
			Scope:         budgets.NormalizeScope(p.Scope), // #491: store canonical invoker
			ScopeKey:      p.ScopeKey,
			Windows:       windowsJSON,
			PolicyVersion: 1,
			CreatedAt:     now,
			UpdatedAt:     now,
		})
	})
}

// Delete removes one invoker spend limit.
func (s *InvokerSpendLimitStore) Delete(ctx context.Context, payer identity.CustomerID, scope, scopeKey string) error {
	tid, terr := merchant.Require(ctx) // #336/#474: no default merchant
	if terr != nil {
		return terr
	}
	tenantID := tid.UUID()
	return s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		_, err := s.db.Gen(ctx).DeleteInvokerSpendLimit(ctx, gen.DeleteInvokerSpendLimitParams{
			MerchantID: tenantID, CustomerID: payer.UUID(),
			Scope: budgets.NormalizeScope(scope), ScopeKey: scopeKey,
		})
		return err
	})
}

func toBudgetWindows(ws []models.BudgetWindowPolicy) []budgets.BudgetWindow {
	out := make([]budgets.BudgetWindow, 0, len(ws))
	for _, w := range ws {
		out = append(out, budgets.BudgetWindow{Key: w.Key, WindowSeconds: w.WindowSeconds, Limit: w.Limit, Currency: w.Currency, Cadence: w.Cadence})
	}
	return out
}
