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
	"github.com/open-rails/openrails/pkg/merchant"
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
	// MaxConcurrentHeldMicros / MaxSingleChargeMicros are the #487 generic
	// $-denominated per-tier admit limits (0 = uncapped).
	MaxConcurrentHeldMicros int64
	MaxSingleChargeMicros   int64
	// BadSpendWindows are the #488 per-PAYER $-valued wasted-spend budget windows
	// for this tier (deny abuse_rate_limited at admit when over).
	BadSpendWindows []models.BudgetWindowPolicy
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
func (s *TierPolicyStore) UpsertTierPolicy(ctx context.Context, payer identity.CustomerID, tier string, windows []models.ThroughputWindow) error {
	return s.UpsertTierPolicyFull(ctx, payer, tier, models.ThroughputPolicy{Windows: windows})
}

// UpsertTierPolicyFull sets the full tier policy (windows + entitled resources).
// A ZERO payer writes the TENANT-WIDE DEFAULT policy for the tier (#477): the
// platform capacity ladder declared once, applied to every payer at that tier
// (selected by GetTierPolicy when the payer has no own override).
func (s *TierPolicyStore) UpsertTierPolicyFull(ctx context.Context, payer identity.CustomerID, tier string, policy models.ThroughputPolicy) error {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	tenantID := tid.UUID()
	now := time.Now().UTC()
	return s.db.RunInTenantConn(ctx, func(ctx context.Context) error {
		policyJSON, err := json.Marshal(policy)
		if err != nil {
			return fmt.Errorf("admission: encode tier policy: %w", err)
		}
		// Tenant-wide default (#477): no subject row to materialize, NULL subject.
		if payer.IsZero() {
			return s.db.Gen(ctx).UpsertTierPolicyDefault(ctx, gen.UpsertTierPolicyDefaultParams{
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
		// tier_policies FK (migration 076) is satisfied on first write (#317).
		if _, err := repo.EnsureCustomerID(ctx, s.db.Qx(ctx), tenantID, payer.UUID().String()); err != nil {
			return err
		}
		subjectID := payer.UUID()
		return s.db.Gen(ctx).UpsertTierPolicy(ctx, gen.UpsertTierPolicyParams{
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

// GetTierPolicy returns the enforceable policy for (payer, tier). A missing row
// yields an empty policy (no throughput limit, all resources allowed).
func (s *TierPolicyStore) GetTierPolicy(ctx context.Context, payer identity.CustomerID, tier string) (ResolvedPolicy, error) {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return ResolvedPolicy{}, err
	}
	tenantID := tid.UUID()
	row := new(models.TierPolicy)
	found := false
	// GetTierPolicy resolves the payer's own override else the tenant-wide default
	// (NULL subject, #477). The query's subject predicate is `= $2 OR IS NULL`, so
	// passing the payer uuid matches both the override and the default.
	subjectID := payer.UUID()
	err = s.db.RunInTenantConn(ctx, func(ctx context.Context) error {
		genRow, e := s.db.Gen(ctx).GetTierPolicy(ctx, gen.GetTierPolicyParams{
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
		Throughput:              toRatelimitPolicy(row.Policy),
		ReleaseThroughput:       releaseTP,
		QueueLimits:             row.Policy.QueueLimits,
		EntitledResources:       row.Policy.EntitledResources,
		BudgetWindows:           toBudgetWindows(row.Policy.BudgetWindows),
		MaxConcurrentHeldMicros: row.Policy.MaxConcurrentHeldMicros,
		MaxSingleChargeMicros:   row.Policy.MaxSingleChargeMicros,
		BadSpendWindows:         row.Policy.BadSpendWindows,
	}, nil
}

// UpsertActorWastedWindows stores the FLAT per-actor wasted-spend windows (#492):
// a zero payer writes the tenant-wide default backstop, a non-zero payer a
// per-customer override. windows is persisted as a JSONB array of
// {key, window_seconds, limit_micros}.
func (s *TierPolicyStore) UpsertActorWastedWindows(ctx context.Context, payer identity.CustomerID, windows []models.BudgetWindowPolicy) error {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	tenantID := tid.UUID()
	now := time.Now().UTC()
	windowsJSON, err := json.Marshal(windows)
	if err != nil {
		return fmt.Errorf("admission: encode actor wasted windows: %w", err)
	}
	return s.db.RunInTenantConn(ctx, func(ctx context.Context) error {
		if payer.IsZero() {
			return s.db.Gen(ctx).UpsertActorWastedWindowsDefault(ctx, gen.UpsertActorWastedWindowsDefaultParams{
				ID: uuidutil.NewV7(), MerchantID: tenantID, Windows: windowsJSON,
				CreatedAt: now, UpdatedAt: now,
			})
		}
		// Per-customer override needs the customers FK satisfied.
		if _, err := repo.EnsureCustomerID(ctx, s.db.Qx(ctx), tenantID, payer.UUID().String()); err != nil {
			return err
		}
		subjectID := payer.UUID()
		return s.db.Gen(ctx).UpsertActorWastedWindowsCustomer(ctx, gen.UpsertActorWastedWindowsCustomerParams{
			ID: uuidutil.NewV7(), MerchantID: tenantID, CustomerID: &subjectID, Windows: windowsJSON,
			CreatedAt: now, UpdatedAt: now,
		})
	})
}

// GetActorWastedWindows returns the EFFECTIVE flat per-actor wasted-spend windows
// for a payer (the customer's override if present, else the tenant-wide default),
// or nil when none is stored (#492). The caller falls back to a hardcoded default
// on nil.
func (s *TierPolicyStore) GetActorWastedWindows(ctx context.Context, payer identity.CustomerID) ([]models.BudgetWindowPolicy, error) {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	tenantID := tid.UUID()
	// `= $2 OR IS NULL` matches both the customer override and the tenant default.
	subjectID := payer.UUID()
	var out []models.BudgetWindowPolicy
	err = s.db.RunInTenantConn(ctx, func(ctx context.Context) error {
		row, e := s.db.Gen(ctx).GetEffectiveActorWastedWindows(ctx, gen.GetEffectiveActorWastedWindowsParams{
			MerchantID: tenantID, CustomerID: &subjectID,
		})
		if errors.Is(e, pgx.ErrNoRows) {
			return nil
		}
		if e != nil {
			return e
		}
		if len(row.Windows) > 0 {
			if uerr := json.Unmarshal(row.Windows, &out); uerr != nil {
				return fmt.Errorf("admission: decode actor wasted windows: %w", uerr)
			}
		}
		return nil
	})
	return out, err
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
// windows[]}. scopeKey is the immutable role uuid (scope=role) / invoker string
// (scope=invoker) / "" (scope=subject).
type BudgetScopePolicy struct {
	Scope    string
	Owner    string
	ScopeKey string
	Windows  []models.BudgetWindowPolicy
}

func budgetPolicyFromGen(r gen.OpenrailsBudgetPolicy) (BudgetScopePolicy, error) {
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
func (s *BudgetPolicyStore) LoadAll(ctx context.Context, payer identity.CustomerID) ([]BudgetScopePolicy, error) {
	tid, terr := merchant.Require(ctx) // #336/#474: no default tenant
	if terr != nil {
		return nil, terr
	}
	tenantID := tid.UUID()
	var out []BudgetScopePolicy
	err := s.db.RunInTenantConn(ctx, func(ctx context.Context) error {
		rows, e := s.db.Gen(ctx).ListBudgetPolicies(ctx, gen.ListBudgetPoliciesParams{
			MerchantID: tenantID, CustomerID: payer.UUID(),
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
func (s *BudgetPolicyStore) LoadByOwner(ctx context.Context, payer identity.CustomerID, owner string) ([]BudgetScopePolicy, error) {
	tid, terr := merchant.Require(ctx) // #336/#474: no default tenant
	if terr != nil {
		return nil, terr
	}
	tenantID := tid.UUID()
	var out []BudgetScopePolicy
	err := s.db.RunInTenantConn(ctx, func(ctx context.Context) error {
		rows, e := s.db.Gen(ctx).ListBudgetPoliciesByOwner(ctx, gen.ListBudgetPoliciesByOwnerParams{
			MerchantID: tenantID, CustomerID: payer.UUID(), Owner: owner,
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
func (s *BudgetPolicyStore) Upsert(ctx context.Context, payer identity.CustomerID, p BudgetScopePolicy) error {
	tid, terr := merchant.Require(ctx) // #336/#474: no default tenant
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
		if _, err := repo.EnsureCustomerID(ctx, s.db.Qx(ctx), tenantID, payer.UUID().String()); err != nil {
			return err
		}
		return s.db.Gen(ctx).UpsertBudgetPolicy(ctx, gen.UpsertBudgetPolicyParams{
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
func (s *BudgetPolicyStore) Delete(ctx context.Context, payer identity.CustomerID, scope, owner, scopeKey string) error {
	tid, terr := merchant.Require(ctx) // #336/#474: no default tenant
	if terr != nil {
		return terr
	}
	tenantID := tid.UUID()
	return s.db.RunInTenantConn(ctx, func(ctx context.Context) error {
		_, err := s.db.Gen(ctx).DeleteBudgetPolicy(ctx, gen.DeleteBudgetPolicyParams{
			MerchantID: tenantID, CustomerID: payer.UUID(),
			Scope: budgets.NormalizeScope(scope), Owner: owner, ScopeKey: scopeKey,
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
