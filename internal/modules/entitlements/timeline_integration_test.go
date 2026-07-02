//go:build integration

package entitlements

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
)

func TestExtendActiveBySubscription_ShiftsFollowingWindowsForward(t *testing.T) {
	ctx := dbtest.WithTestMerchant(context.Background())
	pool := dbtest.SharedPGXPool(t)

	dbi, err := db.NewWithPGXPool(pool, "") // default schema (shared harness)
	require.NoError(t, err)

	r := NewEntitlementService(dbi)

	now := time.Now().UTC().Truncate(time.Second)
	userID := uuid.New().String()
	tenantSubjectID := dbtest.EnsureCustomerIDPgx(ctx, t, pool, userID)
	entName := "premium_timeline_test_" + uuid.New().String()
	subID := uuid.New()
	adminGrantID := uuid.New()

	t0 := now
	t1 := now.Add(30 * 24 * time.Hour)
	t2 := t1.Add(10 * 24 * time.Hour)

	// Create a subscription-sourced entitlement window [t0, t1)
	subEnt := &models.Entitlement{
		ID:          uuid.New(),
		CustomerID:  tenantSubjectID,
		Entitlement: entName,
		StartAt:     t0,
		EndAt:       &t1,
		SourceType:  models.EntitlementSourceSubscription,
		SourceID:    &subID,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	require.NoError(t, r.Insert(ctx, subEnt))

	// Create a scheduled admin window [t1, t2)
	adminEnt := &models.Entitlement{
		ID:          uuid.New(),
		CustomerID:  tenantSubjectID,
		Entitlement: entName,
		StartAt:     t1,
		EndAt:       &t2,
		SourceType:  models.EntitlementSourceAdmin,
		SourceID:    &adminGrantID,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	require.NoError(t, r.Insert(ctx, adminEnt))

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx,
			`DELETE FROM openrails.entitlements WHERE customer_id = $1 AND entitlement = $2`,
			tenantSubjectID, entName)
	})

	// Extend subscription window to t1+5d and expect the admin window to shift by +5d.
	newEnd := t1.Add(5 * 24 * time.Hour)
	require.NoError(t, r.extendActiveBySubscription(ctx, subID, newEnd, now))

	var gotStartAt time.Time
	var gotEndAt *time.Time
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT start_at, end_at FROM openrails.entitlements WHERE id = $1`, adminEnt.ID,
	).Scan(&gotStartAt, &gotEndAt))

	require.Equal(t, t1.Add(5*24*time.Hour), gotStartAt.UTC())
	require.NotNil(t, gotEndAt)
	require.Equal(t, t2.Add(5*24*time.Hour), gotEndAt.UTC())
}

func TestEndActiveByPayment_RevokesFiniteAndDeletesFutureWindows(t *testing.T) {
	ctx := dbtest.WithTestMerchant(context.Background())
	pool := dbtest.SharedPGXPool(t)

	dbi, err := db.NewWithPGXPool(pool, "") // default schema (shared harness)
	require.NoError(t, err)

	r := NewEntitlementService(dbi)

	now := time.Now().UTC().Truncate(time.Second)
	userID := uuid.New().String()
	tenantSubjectID := dbtest.EnsureCustomerIDPgx(ctx, t, pool, userID)
	entName := "premium_payment_revoke_" + uuid.New().String()
	paymentID := uuid.New()

	activeStart := now.Add(-24 * time.Hour)
	activeEnd := now.Add(10 * 24 * time.Hour)
	futureEnd := activeEnd.Add(10 * 24 * time.Hour)

	active := &models.Entitlement{
		ID:          uuid.New(),
		CustomerID:  tenantSubjectID,
		Entitlement: entName,
		StartAt:     activeStart,
		EndAt:       &activeEnd,
		SourceType:  models.EntitlementSourceOneOff,
		SourceID:    &paymentID,
		CreatedAt:   activeStart,
		UpdatedAt:   activeStart,
	}
	require.NoError(t, r.Insert(ctx, active))

	future := &models.Entitlement{
		ID:          uuid.New(),
		CustomerID:  tenantSubjectID,
		Entitlement: entName,
		StartAt:     activeEnd,
		EndAt:       &futureEnd,
		SourceType:  models.EntitlementSourceOneOff,
		SourceID:    &paymentID,
		CreatedAt:   activeStart,
		UpdatedAt:   activeStart,
	}
	require.NoError(t, r.Insert(ctx, future))

	reason := models.EntitlementRevokeRefund
	require.NoError(t, r.endActiveByPayment(ctx, paymentID, now, now, &reason))

	var gotEndAt, gotRevokedAt *time.Time
	var gotRevokeReason *string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT end_at, revoked_at, revoke_reason FROM openrails.entitlements WHERE id = $1`, active.ID,
	).Scan(&gotEndAt, &gotRevokedAt, &gotRevokeReason))
	require.NotNil(t, gotEndAt)
	require.Equal(t, now, gotEndAt.UTC())
	require.NotNil(t, gotRevokedAt)
	require.Equal(t, now, gotRevokedAt.UTC())
	require.NotNil(t, gotRevokeReason)
	require.Equal(t, reason, models.EntitlementRevokeReason(*gotRevokeReason))

	// The future window is soft-deleted; query it without the deleted_at filter.
	var gotDeletedAt *time.Time
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT deleted_at FROM openrails.entitlements WHERE id = $1`, future.ID,
	).Scan(&gotDeletedAt))
	require.NotNil(t, gotDeletedAt)
	require.Equal(t, now, gotDeletedAt.UTC())

	ok, err := r.IsEntitled(ctx, userID, entName, now.Add(time.Second))
	require.NoError(t, err)
	require.False(t, ok)
}

func TestEntitlementRepo_CustomerQueries(t *testing.T) {
	ctx := dbtest.WithTestMerchant(context.Background())
	pool := dbtest.SharedPGXPool(t)

	dbi, err := db.NewWithPGXPool(pool, "") // default schema (shared harness)
	require.NoError(t, err)

	r := NewEntitlementService(dbi)

	now := time.Now().UTC().Truncate(time.Second)
	tenantSubjectID := uuid.New()
	otherCustomerID := uuid.New()
	entName := "premium_tenant_subject_" + uuid.New().String()
	finiteSourceID := uuid.New()
	indefiniteSourceID := uuid.New()

	dbtest.EnsureTestMerchant(ctx, t, pool)
	_, err = pool.Exec(ctx,
		`INSERT INTO openrails.customers (id, merchant_id) VALUES ($1, $2)`,
		tenantSubjectID,
		dbtest.TestMerchantID.UUID(),
	)
	require.NoError(t, err)

	finiteStart := now.Add(-24 * time.Hour)
	finiteEnd := now.Add(24 * time.Hour)
	finite := &models.Entitlement{
		ID:          uuid.New(),
		CustomerID:  tenantSubjectID,
		Entitlement: entName,
		StartAt:     finiteStart,
		EndAt:       &finiteEnd,
		SourceType:  models.EntitlementSourceOneOff,
		SourceID:    &finiteSourceID,
		CreatedAt:   finiteStart,
		UpdatedAt:   finiteStart,
	}
	require.NoError(t, r.Insert(ctx, finite))

	indefiniteName := entName + "_indefinite"
	indefinite := &models.Entitlement{
		ID:          uuid.New(),
		CustomerID:  tenantSubjectID,
		Entitlement: indefiniteName,
		StartAt:     finiteStart,
		SourceType:  models.EntitlementSourceAdmin,
		SourceID:    &indefiniteSourceID,
		CreatedAt:   finiteStart,
		UpdatedAt:   finiteStart,
	}
	require.NoError(t, r.Insert(ctx, indefinite))

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx,
			`DELETE FROM openrails.entitlements WHERE customer_id = $1 AND entitlement = ANY($2)`,
			tenantSubjectID, []string{entName, indefiniteName})
	})

	ok, err := r.IsCustomerEntitled(ctx, tenantSubjectID, entName, now)
	require.NoError(t, err)
	require.True(t, ok)

	ok, err = r.IsCustomerEntitled(ctx, otherCustomerID, entName, now)
	require.NoError(t, err)
	require.False(t, ok)

	ok, err = r.HasActiveIndefiniteByCustomer(ctx, tenantSubjectID, indefiniteName, now)
	require.NoError(t, err)
	require.True(t, ok)

	latest, err := r.LatestFiniteWindowByCustomer(ctx, tenantSubjectID, entName, now)
	require.NoError(t, err)
	require.Equal(t, finite.ID, latest.ID)
}
