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

// TierSpendCapStore loads + stores per-payer, per-tier admission policies
// (openrails.tier_spend_caps). Account money state stays in money_accounts.
type TierSpendCapStore struct {
	db *db.DB
}

func NewTierSpendCapStore(database *db.DB) *TierSpendCapStore {
	return &TierSpendCapStore{db: database}
}

// TierSpendCaps is a tier's enforceable money policy.
type TierSpendCaps struct {
	BudgetWindows  []budgets.BudgetWindow
	PolicyCurrency string
	// BadSpendWindows are the #497 per-PAYER direct-credential wasted-spend grace
	// windows for this tier; direct-payer overage is charged at report time.
	BadSpendWindows []models.BudgetWindowPolicy
}

// UpsertTierSpendCapsFull sets the full tier money policy.
// A ZERO payer writes the TENANT-WIDE DEFAULT policy for the tier (#477): the
// platform capacity ladder declared once, applied to every payer at that tier
// (selected by GetTierSpendCaps when the payer has no own override).
func (s *TierSpendCapStore) UpsertTierSpendCapsFull(ctx context.Context, payer identity.CustomerID, tier string, policy models.TierMoneyPolicy) error {
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
			return s.db.Gen(ctx).UpsertTierSpendCapDefault(ctx, gen.UpsertTierSpendCapDefaultParams{
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
		// tier_spend_caps FK (migration 076) is satisfied on first write (#317).
		if _, err := repo.EnsureCustomerID(ctx, s.db.Qx(ctx), tenantID, payer.UUID().String()); err != nil {
			return err
		}
		subjectID := payer.UUID()
		return s.db.Gen(ctx).UpsertTierSpendCap(ctx, gen.UpsertTierSpendCapParams{
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

// GetTierSpendCaps returns the enforceable money policy for (payer, tier). A
// missing row yields an empty policy.
func (s *TierSpendCapStore) GetTierSpendCaps(ctx context.Context, payer identity.CustomerID, tier string) (TierSpendCaps, error) {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return TierSpendCaps{}, err
	}
	tenantID := tid.UUID()
	row := new(models.TierPolicy)
	found := false
	// GetTierSpendCaps resolves the payer's own override else the merchant-wide default
	// (NULL subject, #477). The query's subject predicate is `= $2 OR IS NULL`, so
	// passing the payer uuid matches both the override and the default.
	subjectID := payer.UUID()
	err = s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		genRow, e := s.db.Gen(ctx).GetTierSpendCaps(ctx, gen.GetTierSpendCapsParams{
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
		return TierSpendCaps{}, err
	}
	if !found {
		return TierSpendCaps{}, nil
	}
	return TierSpendCaps{
		BudgetWindows:   toBudgetWindows(row.Policy.BudgetWindows),
		PolicyCurrency:  row.Policy.PolicyCurrency,
		BadSpendWindows: row.Policy.BadSpendWindows,
	}, nil
}

// ScopedSpendCapStore reads/writes hierarchical money-budget policies (#473) —
// {scope, owner, windows[]} rows in openrails.scoped_spend_caps. The OWNER
// discriminator is the write-authz split: SetSubjectSpendCaps may only write
// owner='subject' rows; SetPlatformSpendCaps writes owner='platform' rows
// (callable only from a platform-admin path); a subject's read MUST NOT expose
// platform-owned rows. The admit path (LoadAll) reads ALL owners to compose.
type ScopedSpendCapStore struct {
	db *db.DB
}

func NewScopedSpendCapStore(database *db.DB) *ScopedSpendCapStore {
	return &ScopedSpendCapStore{db: database}
}

// ScopedSpendCap is one stored scope policy: {scope, owner, scopeKey,
// windows[]}. scopeKey is the immutable role uuid (scope=role) / invoker string
// (scope=invoker) / "" (scope=subject).
type ScopedSpendCap struct {
	Scope    string
	Owner    string
	ScopeKey string
	Windows  []models.BudgetWindowPolicy
}

func scopedSpendCapFromGen(r gen.OpenrailsScopedSpendCap) (ScopedSpendCap, error) {
	p := ScopedSpendCap{Scope: r.Scope, Owner: r.Owner, ScopeKey: r.ScopeKey}
	if len(r.Windows) > 0 {
		if err := json.Unmarshal(r.Windows, &p.Windows); err != nil {
			return ScopedSpendCap{}, fmt.Errorf("admission: decode budget policy windows: %w", err)
		}
	}
	return p, nil
}

// LoadAll returns every budget-scope policy for a subject regardless of owner
// (the admit path composes all of them). Returns nil when none exist.
func (s *ScopedSpendCapStore) LoadAll(ctx context.Context, payer identity.CustomerID) ([]ScopedSpendCap, error) {
	tid, terr := merchant.Require(ctx) // #336/#474: no default merchant
	if terr != nil {
		return nil, terr
	}
	tenantID := tid.UUID()
	var out []ScopedSpendCap
	err := s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		rows, e := s.db.Gen(ctx).ListScopedSpendCaps(ctx, gen.ListScopedSpendCapsParams{
			MerchantID: tenantID, CustomerID: payer.UUID(),
		})
		if e != nil {
			return e
		}
		for _, r := range rows {
			p, derr := scopedSpendCapFromGen(r)
			if derr != nil {
				return derr
			}
			out = append(out, p)
		}
		return nil
	})
	return out, err
}

// LoadByOwner returns a subject's budget-scope policies for one owner only — the
// subject-facing read uses owner="subject" so platform-owned caps are invisible.
func (s *ScopedSpendCapStore) LoadByOwner(ctx context.Context, payer identity.CustomerID, owner string) ([]ScopedSpendCap, error) {
	tid, terr := merchant.Require(ctx) // #336/#474: no default merchant
	if terr != nil {
		return nil, terr
	}
	tenantID := tid.UUID()
	var out []ScopedSpendCap
	err := s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		rows, e := s.db.Gen(ctx).ListScopedSpendCapsByOwner(ctx, gen.ListScopedSpendCapsByOwnerParams{
			MerchantID: tenantID, CustomerID: payer.UUID(), Owner: owner,
		})
		if e != nil {
			return e
		}
		for _, r := range rows {
			p, derr := scopedSpendCapFromGen(r)
			if derr != nil {
				return derr
			}
			out = append(out, p)
		}
		return nil
	})
	return out, err
}

// Upsert writes one budget-scope policy. owner MUST be supplied by the caller's
// authz path (SetSubjectSpendCaps / SetPlatformSpendCaps at the service
// layer); this method does not itself decide authz.
func (s *ScopedSpendCapStore) Upsert(ctx context.Context, payer identity.CustomerID, p ScopedSpendCap) error {
	tid, terr := merchant.Require(ctx) // #336/#474: no default merchant
	if terr != nil {
		return terr
	}
	tenantID := tid.UUID()
	now := time.Now().UTC()
	windowsJSON, err := json.Marshal(p.Windows)
	if err != nil {
		return fmt.Errorf("admission: encode budget policy windows: %w", err)
	}
	return s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		if _, err := repo.EnsureCustomerID(ctx, s.db.Qx(ctx), tenantID, payer.UUID().String()); err != nil {
			return err
		}
		return s.db.Gen(ctx).UpsertScopedSpendCap(ctx, gen.UpsertScopedSpendCapParams{
			ID:            uuidutil.NewV7(),
			MerchantID:    tenantID,
			CustomerID:    payer.UUID(),
			Scope:         budgets.NormalizeScope(p.Scope), // #491: store canonical invoker
			Owner:         p.Owner,
			ScopeKey:      p.ScopeKey,
			Windows:       windowsJSON,
			PolicyVersion: 1,
			CreatedAt:     now,
			UpdatedAt:     now,
		})
	})
}

// Delete removes one budget-scope policy (owner-qualified so a subject path
// cannot delete a platform-owned row).
func (s *ScopedSpendCapStore) Delete(ctx context.Context, payer identity.CustomerID, scope, owner, scopeKey string) error {
	tid, terr := merchant.Require(ctx) // #336/#474: no default merchant
	if terr != nil {
		return terr
	}
	tenantID := tid.UUID()
	return s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		_, err := s.db.Gen(ctx).DeleteScopedSpendCap(ctx, gen.DeleteScopedSpendCapParams{
			MerchantID: tenantID, CustomerID: payer.UUID(),
			Scope: budgets.NormalizeScope(scope), Owner: owner, ScopeKey: scopeKey,
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
