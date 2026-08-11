//go:build integration

package entitlements

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/stretchr/testify/require"
)

// or#912 effective-tier resolution: given a customer's ACTIVE entitlements and
// a tier group, return THE winner — highest tier_rank, deterministic, never a
// conflict error. The mid-upgrade overlap case (two active tier entitlements
// at once) is the case the consumer's previous hand-rolled resolution 500'd on.

func seedTierProduct(ctx context.Context, t *testing.T, pool *pgxpool.Pool, key, displayName, group string, rank int, archived bool, entitlementKeys ...string) uuid.UUID {
	t.Helper()
	spec := make(map[string]*int, len(entitlementKeys))
	for _, k := range entitlementKeys {
		spec[k] = nil
	}
	specJSON, err := json.Marshal(spec)
	require.NoError(t, err)
	id := uuid.New()
	_, err = pool.Exec(ctx, `
		INSERT INTO openrails.products (id, merchant_id, key, display_name, entitlements_spec, tier_group, tier_rank, archived)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		id, dbtest.TestMerchantID.UUID(), key, displayName, specJSON, group, rank, archived)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM openrails.products WHERE id = $1`, id) })
	return id
}

func seedActiveWindow(ctx context.Context, t *testing.T, svc *EntitlementService, customerID uuid.UUID, entitlement string, start time.Time, end *time.Time) *models.Entitlement {
	t.Helper()
	sid := uuid.New()
	ent := &models.Entitlement{
		CustomerID:  customerID,
		Entitlement: entitlement,
		StartAt:     start,
		EndAt:       end,
		SourceID:    &sid,
		SourceType:  models.EntitlementSourceAdmin,
	}
	require.NoError(t, svc.Insert(ctx, ent))
	return ent
}

func TestOr912_EffectiveTierResolution(t *testing.T) {
	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	pool := dbi.Pool()
	dbtest.EnsureTestMerchant(ctx, t, pool)
	svc := NewEntitlementService(dbi)

	suffix := uuid.NewString()[:8]
	group := "grp_" + suffix
	entFree := "free_" + suffix
	entT1 := "tier_1_" + suffix
	entT2 := "tier_2_" + suffix
	entOther := "other_" + suffix

	seedTierProduct(ctx, t, pool, "free_"+suffix, "Free", group, 0, false, entFree)
	seedTierProduct(ctx, t, pool, "basic_"+suffix, "Basic", group, 1, false, entT1)
	proID := seedTierProduct(ctx, t, pool, "pro_"+suffix, "Pro", group, 2, false, entT2)
	// An ARCHIVED product declaring the same entitlement at a higher rank must
	// never win — archived products are out of the resolution set entirely.
	seedTierProduct(ctx, t, pool, "legacy_"+suffix, "Legacy", group, 99, true, entT2)
	// A product in ANOTHER group declaring an entitlement this customer holds
	// must not leak into this group's resolution.
	seedTierProduct(ctx, t, pool, "elsewhere_"+suffix, "Elsewhere", "othergrp_"+suffix, 50, false, entOther)

	now := time.Now().UTC().Truncate(time.Second)

	t.Run("no active entitlements resolves to no tier, not an error", func(t *testing.T) {
		userID := uuid.NewString()
		dbtest.EnsureCustomerIDPgx(ctx, t, pool, userID)
		tier, err := svc.ResolveEffectiveTier(ctx, userID, group, now)
		require.NoError(t, err)
		require.Nil(t, tier)
	})

	t.Run("mid-upgrade overlap: two active tier entitlements, higher rank wins", func(t *testing.T) {
		userID := uuid.NewString()
		customerID := dbtest.EnsureCustomerIDPgx(ctx, t, pool, userID)
		// The old subscription's window is still open while the upgrade's window
		// has already started — BOTH are active right now. The old consumer-side
		// design answered this state with a conflict 500.
		oldEnd := now.Add(24 * time.Hour)
		seedActiveWindow(ctx, t, svc, customerID, entT1, now.Add(-30*24*time.Hour), &oldEnd)
		seedActiveWindow(ctx, t, svc, customerID, entT2, now.Add(-time.Hour), nil)

		tier, err := svc.ResolveEffectiveTier(ctx, userID, group, now)
		require.NoError(t, err)
		require.NotNil(t, tier)
		require.Equal(t, entT2, tier.Entitlement)
		require.Equal(t, "pro_"+suffix, tier.ProductKey)
		require.Equal(t, "Pro", tier.ProductDisplayName)
		require.Equal(t, 2, tier.TierRank)
		require.Equal(t, proID, tier.ProductID)
		require.Equal(t, group, tier.TierGroup)
	})

	t.Run("expired and revoked windows do not count", func(t *testing.T) {
		userID := uuid.NewString()
		customerID := dbtest.EnsureCustomerIDPgx(ctx, t, pool, userID)
		expiredEnd := now.Add(-time.Hour)
		seedActiveWindow(ctx, t, svc, customerID, entT2, now.Add(-48*time.Hour), &expiredEnd)
		revoked := seedActiveWindow(ctx, t, svc, customerID, entFree, now.Add(-time.Hour), nil)
		require.NoError(t, svc.RevokeExistingEntitlement(ctx, RevokeExistingEntitlementParams{
			EntitlementID: &revoked.ID, Reason: models.EntitlementRevokeAdmin,
		}))
		seedActiveWindow(ctx, t, svc, customerID, entT1, now.Add(-time.Hour), nil)

		tier, err := svc.ResolveEffectiveTier(ctx, userID, group, now)
		require.NoError(t, err)
		require.NotNil(t, tier)
		require.Equal(t, entT1, tier.Entitlement)
		require.Equal(t, 1, tier.TierRank)
	})

	t.Run("entitlements from other groups are ignored", func(t *testing.T) {
		userID := uuid.NewString()
		customerID := dbtest.EnsureCustomerIDPgx(ctx, t, pool, userID)
		seedActiveWindow(ctx, t, svc, customerID, entOther, now.Add(-time.Hour), nil)
		tier, err := svc.ResolveEffectiveTier(ctx, userID, group, now)
		require.NoError(t, err)
		require.Nil(t, tier)
	})

	t.Run("rank tie breaks deterministically on product key", func(t *testing.T) {
		tieGroup := "tiegrp_" + suffix
		entA := "tie_a_" + suffix
		entB := "tie_b_" + suffix
		seedTierProduct(ctx, t, pool, "zeta_"+suffix, "Zeta", tieGroup, 7, false, entB)
		seedTierProduct(ctx, t, pool, "alpha_"+suffix, "Alpha", tieGroup, 7, false, entA)

		userID := uuid.NewString()
		customerID := dbtest.EnsureCustomerIDPgx(ctx, t, pool, userID)
		seedActiveWindow(ctx, t, svc, customerID, entA, now.Add(-time.Hour), nil)
		seedActiveWindow(ctx, t, svc, customerID, entB, now.Add(-time.Hour), nil)

		for i := 0; i < 3; i++ {
			tier, err := svc.ResolveEffectiveTier(ctx, userID, tieGroup, now)
			require.NoError(t, err)
			require.NotNil(t, tier)
			require.Equal(t, "alpha_"+suffix, tier.ProductKey, "tie must break on product key ASC, every time")
			require.Equal(t, entA, tier.Entitlement)
		}
	})

	t.Run("group is required", func(t *testing.T) {
		_, err := svc.ResolveEffectiveTier(ctx, uuid.NewString(), "  ", now)
		require.Error(t, err)
	})
}
