// Package admission unifies OpenRails' two admission axes into one decision
// (issue #298): the THROUGHPUT limiter (internal/modules/ratelimit, Redis) and
// the MONEY gate (internal/modules/credits, the ledger). A host calls Admit
// before doing work; it denies on whichever axis is hit first.
package admission

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/budgets"
	"github.com/open-rails/openrails/internal/modules/ratelimit"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/tenant"
)

// TierPolicyStore loads + stores per-payer, per-tier throughput policies
// (billing.tier_policies). The money axis stays in credit_account_settings.
type TierPolicyStore struct {
	db *db.DB
}

func NewTierPolicyStore(database *db.DB) *TierPolicyStore { return &TierPolicyStore{db: database} }

// ResolvedPolicy is a tier's enforceable policy: throughput windows, entitled
// endpoints (empty = all allowed), and rolling money-budget windows (#304).
type ResolvedPolicy struct {
	Throughput        ratelimit.Policy
	EntitledEndpoints []string
	BudgetWindows     []budgets.BudgetWindow
}

// UpsertTierPolicy sets the throughput windows for (payer, tier).
func (s *TierPolicyStore) UpsertTierPolicy(ctx context.Context, payer identity.TenantSubjectID, tier string, windows []models.ThroughputWindow) error {
	return s.UpsertTierPolicyFull(ctx, payer, tier, models.ThroughputPolicy{Windows: windows})
}

// UpsertTierPolicyFull sets the full tier policy (windows + entitled endpoints).
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
		_, err := s.db.Q(ctx).NewInsert().Model(row).
			On("CONFLICT (tenant_id, tenant_subject_id, tier) DO UPDATE").
			Set("policy = EXCLUDED.policy").
			Set("updated_at = EXCLUDED.updated_at").
			Exec(ctx)
		return err
	})
}

// GetTierPolicy returns the enforceable policy for (payer, tier). A missing row
// yields an empty policy (no throughput limit, all endpoints allowed).
func (s *TierPolicyStore) GetTierPolicy(ctx context.Context, payer identity.TenantSubjectID, tier string) (ResolvedPolicy, error) {
	tenantID := tenant.FromContextOrDefault(ctx).UUID()
	row := new(models.TierPolicy)
	found := false
	err := s.db.RunInTenantConn(ctx, func(ctx context.Context) error {
		e := s.db.Q(ctx).NewSelect().Model(row).
			Where("tenant_id = ? AND tenant_subject_id = ? AND tier = ?", tenantID, payer.UUID(), tier).
			Limit(1).Scan(ctx)
		if errors.Is(e, sql.ErrNoRows) {
			return nil
		}
		if e != nil {
			return e
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
		EntitledEndpoints: row.Policy.EntitledEndpoints,
		BudgetWindows:     toBudgetWindows(row.Policy.BudgetWindows),
	}, nil
}

func toBudgetWindows(ws []models.BudgetWindowPolicy) []budgets.BudgetWindow {
	out := make([]budgets.BudgetWindow, 0, len(ws))
	for _, w := range ws {
		out = append(out, budgets.BudgetWindow{Key: w.Key, WindowSeconds: w.WindowSeconds, LimitMillicents: w.LimitMillicents})
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
