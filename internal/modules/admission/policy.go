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
	// Throughput is the DEFAULT throughput window list (used when a release has
	// no per-release override).
	Throughput ratelimit.Policy
	// ReleaseThroughput holds per-(endpoint-release) throughput window VALUES
	// (#472 G1); keyed by the release uuid. ThroughputForRelease resolves a
	// release to its windows (override > default).
	ReleaseThroughput map[string]ratelimit.Policy
	// QueueLimits are the batch/queue reservation-pool caps (#472 G2).
	QueueLimits       []models.QueueLimitPolicy
	EntitledResources []string
	BudgetWindows     []budgets.BudgetWindow
}

// ThroughputForRelease returns the throughput policy for an endpoint-release:
// its per-release override when present, else the tier-global default (#472 G1).
func (p ResolvedPolicy) ThroughputForRelease(release string) ratelimit.Policy {
	if rp, ok := p.ReleaseThroughput[release]; ok {
		return rp
	}
	return p.Throughput
}

// UpsertTierPolicy sets the throughput windows for (payer, tier).
func (s *TierPolicyStore) UpsertTierPolicy(ctx context.Context, payer identity.TenantSubjectID, tier string, windows []models.ThroughputWindow) error {
	return s.UpsertTierPolicyFull(ctx, payer, tier, models.ThroughputPolicy{Windows: windows})
}

// UpsertTierPolicyFull sets the full tier policy (windows + entitled resources).
func (s *TierPolicyStore) UpsertTierPolicyFull(ctx context.Context, payer identity.TenantSubjectID, tier string, policy models.ThroughputPolicy) error {
	tid, err := tenant.Require(ctx)
	if err != nil {
		return err
	}
	tenantID := tid.UUID()
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
	tid, err := tenant.Require(ctx)
	if err != nil {
		return ResolvedPolicy{}, err
	}
	tenantID := tid.UUID()
	row := new(models.TierPolicy)
	found := false
	err = s.db.RunInTenantConn(ctx, func(ctx context.Context) error {
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
	var releaseTP map[string]ratelimit.Policy
	if len(row.Policy.ReleaseWindows) > 0 {
		releaseTP = make(map[string]ratelimit.Policy, len(row.Policy.ReleaseWindows))
		for rel, ws := range row.Policy.ReleaseWindows {
			releaseTP[rel] = toRatelimitPolicy(models.ThroughputPolicy{Windows: ws})
		}
	}
	return ResolvedPolicy{
		Throughput:        toRatelimitPolicy(row.Policy),
		ReleaseThroughput: releaseTP,
		QueueLimits:       row.Policy.QueueLimits,
		EntitledResources: row.Policy.EntitledResources,
		BudgetWindows:     toBudgetWindows(row.Policy.BudgetWindows),
	}, nil
}

// BudgetPolicyStore reads/writes hierarchical money-budget policies (#473) —
// {scope, owner, windows[]} rows in openrails.budget_policies. The OWNER
// discriminator is the write-authz split: SetSubjectBudgetPolicy may only write
// owner='subject' rows; SetPlatformBudgetPolicy writes owner='platform' rows
// (callable only from a platform-admin path); a subject's read MUST NOT expose
// platform-owned rows. The admit path (LoadAll) reads ALL owners to compose.
type BudgetPolicyStore struct {
	db *db.DB
}

func NewBudgetPolicyStore(database *db.DB) *BudgetPolicyStore {
	return &BudgetPolicyStore{db: database}
}

// BudgetScopePolicy is one stored scope policy: {scope, owner, scopeKey,
// windows[]}. scopeKey is the immutable role uuid (scope=role) / actor string
// (scope=actor) / "" (scope=subject).
type BudgetScopePolicy struct {
	Scope    string
	Owner    string
	ScopeKey string
	Windows  []models.BudgetWindowPolicy
}

func budgetPolicyFromGen(r gen.BillingBudgetPolicy) (BudgetScopePolicy, error) {
	p := BudgetScopePolicy{Scope: r.Scope, Owner: r.Owner, ScopeKey: r.ScopeKey}
	if len(r.Windows) > 0 {
		if err := json.Unmarshal(r.Windows, &p.Windows); err != nil {
			return BudgetScopePolicy{}, fmt.Errorf("admission: decode budget policy windows: %w", err)
		}
	}
	return p, nil
}

// LoadAll returns every budget-scope policy for a subject regardless of owner
// (the admit path composes all of them). Returns nil when none exist.
func (s *BudgetPolicyStore) LoadAll(ctx context.Context, payer identity.TenantSubjectID) ([]BudgetScopePolicy, error) {
	tid, terr := tenant.Require(ctx) // #336/#474: no default tenant
	if terr != nil {
		return nil, terr
	}
	tenantID := tid.UUID()
	var out []BudgetScopePolicy
	err := s.db.RunInTenantConn(ctx, func(ctx context.Context) error {
		rows, e := s.db.Gen(ctx).ListBudgetPolicies(ctx, gen.ListBudgetPoliciesParams{
			TenantID: tenantID, TenantSubjectID: payer.UUID(),
		})
		if e != nil {
			return e
		}
		for _, r := range rows {
			p, derr := budgetPolicyFromGen(r)
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
func (s *BudgetPolicyStore) LoadByOwner(ctx context.Context, payer identity.TenantSubjectID, owner string) ([]BudgetScopePolicy, error) {
	tid, terr := tenant.Require(ctx) // #336/#474: no default tenant
	if terr != nil {
		return nil, terr
	}
	tenantID := tid.UUID()
	var out []BudgetScopePolicy
	err := s.db.RunInTenantConn(ctx, func(ctx context.Context) error {
		rows, e := s.db.Gen(ctx).ListBudgetPoliciesByOwner(ctx, gen.ListBudgetPoliciesByOwnerParams{
			TenantID: tenantID, TenantSubjectID: payer.UUID(), Owner: owner,
		})
		if e != nil {
			return e
		}
		for _, r := range rows {
			p, derr := budgetPolicyFromGen(r)
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
// authz path (SetSubjectBudgetPolicy / SetPlatformBudgetPolicy at the service
// layer); this method does not itself decide authz.
func (s *BudgetPolicyStore) Upsert(ctx context.Context, payer identity.TenantSubjectID, p BudgetScopePolicy) error {
	tid, terr := tenant.Require(ctx) // #336/#474: no default tenant
	if terr != nil {
		return terr
	}
	tenantID := tid.UUID()
	now := time.Now().UTC()
	windowsJSON, err := json.Marshal(p.Windows)
	if err != nil {
		return fmt.Errorf("admission: encode budget policy windows: %w", err)
	}
	return s.db.RunInTenantConn(ctx, func(ctx context.Context) error {
		if _, err := repo.EnsureTenantSubjectID(ctx, s.db.Qx(ctx), tenantID, payer.UUID().String()); err != nil {
			return err
		}
		return s.db.Gen(ctx).UpsertBudgetPolicy(ctx, gen.UpsertBudgetPolicyParams{
			ID:              uuidutil.NewV7(),
			TenantID:        tenantID,
			TenantSubjectID: payer.UUID(),
			Scope:           p.Scope,
			Owner:           p.Owner,
			ScopeKey:        p.ScopeKey,
			Windows:         windowsJSON,
			PolicyVersion:   1,
			CreatedAt:       now,
			UpdatedAt:       now,
		})
	})
}

// Delete removes one budget-scope policy (owner-qualified so a subject path
// cannot delete a platform-owned row).
func (s *BudgetPolicyStore) Delete(ctx context.Context, payer identity.TenantSubjectID, scope, owner, scopeKey string) error {
	tid, terr := tenant.Require(ctx) // #336/#474: no default tenant
	if terr != nil {
		return terr
	}
	tenantID := tid.UUID()
	return s.db.RunInTenantConn(ctx, func(ctx context.Context) error {
		_, err := s.db.Gen(ctx).DeleteBudgetPolicy(ctx, gen.DeleteBudgetPolicyParams{
			TenantID: tenantID, TenantSubjectID: payer.UUID(),
			Scope: scope, Owner: owner, ScopeKey: scopeKey,
		})
		return err
	})
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
