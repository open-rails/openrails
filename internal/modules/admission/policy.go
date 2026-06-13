// Package admission unifies OpenRails' two admission axes into one decision
// (issue #298): the THROUGHPUT limiter (internal/modules/ratelimit, Redis) and
// the MONEY gate (internal/modules/credits, the ledger). A host calls Admit
// before doing work; it denies on whichever axis is hit first.
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
	"github.com/open-rails/openrails/internal/modules/ratelimit"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/tenant"
)

// TierPolicyStore loads + stores per-payer, per-tier throughput policies
// (openrails.tier_policies). The money axis stays in credit_account_settings.
type TierPolicyStore struct {
	db *db.DB
}

func NewTierPolicyStore(database *db.DB) *TierPolicyStore { return &TierPolicyStore{db: database} }

// ResolvedPolicy is a tier's enforceable policy: throughput windows, entitled
// resources (empty = all allowed), and fixed money-budget windows (#304, #337).
type ResolvedPolicy struct {
	Throughput        ratelimit.Policy
	EntitledResources []string
	BudgetWindows     []budgets.BudgetWindow
}

// UpsertTierPolicy sets the throughput windows for (payer, tier).
func (s *TierPolicyStore) UpsertTierPolicy(ctx context.Context, payer identity.TenantSubjectID, tier string, windows []models.ThroughputWindow) error {
	return s.UpsertTierPolicyFull(ctx, payer, tier, models.ThroughputPolicy{Windows: windows})
}

// UpsertTierPolicyFull sets the full tier policy (windows + entitled resources).
func (s *TierPolicyStore) UpsertTierPolicyFull(ctx context.Context, payer identity.TenantSubjectID, tier string, policy models.ThroughputPolicy) error {
	tenantID := tenant.FromContextOrDefault(ctx).UUID()
	now := time.Now().UTC()
	row := &models.TierPolicy{
		ID:              uuidutil.NewV7(),
		TenantID:        tenantID,
		TenantSubjectID: payer.UUID(),
		Tier:            tier,
		Policy:          policy,
		PolicyVersion:   1,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	return s.db.RunInTenantConn(ctx, func(ctx context.Context) error {
		// Materialize the payable tenant_subjects row so the tier_policies FK
		// (migration 076) is satisfied on a subject's first policy write (#317).
		if _, err := repo.EnsureTenantSubjectID(ctx, s.db.Qx(ctx), tenantID, payer.UUID().String()); err != nil {
			return err
		}
		policyJSON, err := json.Marshal(row.Policy)
		if err != nil {
			return fmt.Errorf("admission: encode tier policy: %w", err)
		}
		return s.db.Gen(ctx).UpsertTierPolicy(ctx, gen.UpsertTierPolicyParams{
			ID:              row.ID,
			TenantID:        row.TenantID,
			TenantSubjectID: row.TenantSubjectID,
			Tier:            row.Tier,
			Policy:          policyJSON,
			PolicyVersion:   int64(row.PolicyVersion),
			CreatedAt:       row.CreatedAt,
			UpdatedAt:       row.UpdatedAt,
		})
	})
}

// GetTierPolicy returns the enforceable policy for (payer, tier). A missing row
// yields an empty policy (no throughput limit, all resources allowed).
func (s *TierPolicyStore) GetTierPolicy(ctx context.Context, payer identity.TenantSubjectID, tier string) (ResolvedPolicy, error) {
	tenantID := tenant.FromContextOrDefault(ctx).UUID()
	row := new(models.TierPolicy)
	found := false
	err := s.db.RunInTenantConn(ctx, func(ctx context.Context) error {
		genRow, e := s.db.Gen(ctx).GetTierPolicy(ctx, gen.GetTierPolicyParams{
			TenantID: tenantID, TenantSubjectID: payer.UUID(), Tier: tier,
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
		return ResolvedPolicy{}, err
	}
	if !found {
		return ResolvedPolicy{}, nil
	}
	return ResolvedPolicy{
		Throughput:        toRatelimitPolicy(row.Policy),
		EntitledResources: row.Policy.EntitledResources,
		BudgetWindows:     toBudgetWindows(row.Policy.BudgetWindows),
	}, nil
}

func toBudgetWindows(ws []models.BudgetWindowPolicy) []budgets.BudgetWindow {
	out := make([]budgets.BudgetWindow, 0, len(ws))
	for _, w := range ws {
		out = append(out, budgets.BudgetWindow{Key: w.Key, WindowSeconds: w.WindowSeconds, LimitMicros: w.LimitMicros, Cadence: w.Cadence})
	}
	return out
}

func toRatelimitPolicy(p models.ThroughputPolicy) ratelimit.Policy {
	out := ratelimit.Policy{Windows: make([]ratelimit.Limit, 0, len(p.Windows))}
	for _, w := range p.Windows {
		out.Windows = append(out.Windows, ratelimit.Limit{
			Unit:   w.Unit,
			Window: time.Duration(w.WindowSeconds) * time.Second,
			Max:    w.Max,
		})
	}
	return out
}
