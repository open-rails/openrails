//go:build integration

package repo

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/tenant"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
)

func TestExtendActiveBySubscription_ShiftsFollowingWindowsForward(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)

	sqlDB := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	t.Cleanup(func() { _ = sqlDB.Close() })
	bunDB := bun.NewDB(sqlDB, pgdialect.New())
	models.RegisterModels(bunDB)

	ctx := context.Background()
	require.NoError(t, bunDB.PingContext(ctx))

	dbi, err := db.NewWithBun(bunDB)
	require.NoError(t, err)

	r := NewEntitlementRepo(dbi)

	now := time.Now().UTC().Truncate(time.Second)
	userID := uuid.New().String()
	entName := "premium_timeline_test_" + uuid.New().String()
	subID := uuid.New()
	adminGrantID := uuid.New()

	t0 := now
	t1 := now.Add(30 * 24 * time.Hour)
	t2 := t1.Add(10 * 24 * time.Hour)

	// Create a subscription-sourced entitlement window [t0, t1)
	subEnt := &models.Entitlement{
		ID:          uuid.New(),
		UserID:      userID,
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
		UserID:      userID,
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
		_, _ = bunDB.NewDelete().
			Model((*models.Entitlement)(nil)).
			Where("user_id = ? AND entitlement = ?", userID, entName).
			Exec(ctx)
	})

	// Extend subscription window to t1+5d and expect the admin window to shift by +5d.
	newEnd := t1.Add(5 * 24 * time.Hour)
	require.NoError(t, r.ExtendActiveBySubscription(ctx, subID, newEnd, now))

	var gotAdmin models.Entitlement
	require.NoError(t, bunDB.NewSelect().
		Model(&gotAdmin).
		Where("id = ?", adminEnt.ID).
		Limit(1).
		Scan(ctx))

	require.Equal(t, t1.Add(5*24*time.Hour), gotAdmin.StartAt.UTC())
	require.NotNil(t, gotAdmin.EndAt)
	require.Equal(t, t2.Add(5*24*time.Hour), gotAdmin.EndAt.UTC())
}

func TestEndActiveByPayment_RevokesFiniteAndDeletesFutureWindows(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)

	sqlDB := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	t.Cleanup(func() { _ = sqlDB.Close() })
	bunDB := bun.NewDB(sqlDB, pgdialect.New())
	models.RegisterModels(bunDB)

	ctx := context.Background()
	require.NoError(t, bunDB.PingContext(ctx))

	dbi, err := db.NewWithBun(bunDB)
	require.NoError(t, err)

	r := NewEntitlementRepo(dbi)

	now := time.Now().UTC().Truncate(time.Second)
	userID := uuid.New().String()
	entName := "premium_payment_revoke_" + uuid.New().String()
	paymentID := uuid.New()

	activeStart := now.Add(-24 * time.Hour)
	activeEnd := now.Add(10 * 24 * time.Hour)
	futureEnd := activeEnd.Add(10 * 24 * time.Hour)

	active := &models.Entitlement{
		ID:          uuid.New(),
		UserID:      userID,
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
		UserID:      userID,
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
	require.NoError(t, r.EndActiveByPayment(ctx, paymentID, now, now, &reason))

	var gotActive models.Entitlement
	require.NoError(t, bunDB.NewSelect().
		Model(&gotActive).
		Where("id = ?", active.ID).
		Limit(1).
		Scan(ctx))
	require.NotNil(t, gotActive.EndAt)
	require.Equal(t, now, gotActive.EndAt.UTC())
	require.NotNil(t, gotActive.RevokedAt)
	require.Equal(t, now, gotActive.RevokedAt.UTC())
	require.NotNil(t, gotActive.RevokeReason)
	require.Equal(t, reason, *gotActive.RevokeReason)

	var gotFuture models.Entitlement
	require.NoError(t, bunDB.NewSelect().
		Model(&gotFuture).
		Where("id = ?", future.ID).
		WhereAllWithDeleted().
		Limit(1).
		Scan(ctx))
	require.NotNil(t, gotFuture.DeletedAt)
	require.Equal(t, now, gotFuture.DeletedAt.UTC())

	ok, err := r.IsEntitled(ctx, userID, entName, now.Add(time.Second))
	require.NoError(t, err)
	require.False(t, ok)
}

func TestEntitlementRepo_TenantSubjectQueries(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)

	sqlDB := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	t.Cleanup(func() { _ = sqlDB.Close() })
	bunDB := bun.NewDB(sqlDB, pgdialect.New())
	models.RegisterModels(bunDB)

	ctx := context.Background()
	require.NoError(t, bunDB.PingContext(ctx))

	dbi, err := db.NewWithBun(bunDB)
	require.NoError(t, err)

	r := NewEntitlementRepo(dbi)

	now := time.Now().UTC().Truncate(time.Second)
	tenantSubjectID := uuid.New()
	otherTenantSubjectID := uuid.New()
	userID := uuid.New().String()
	entName := "premium_tenant_subject_" + uuid.New().String()
	finiteSourceID := uuid.New()
	indefiniteSourceID := uuid.New()

	_, err = bunDB.NewRaw(
		`INSERT INTO billing.tenant_subjects (id, tenant_id, issuer, subject) VALUES (?, ?, ?, ?)`,
		tenantSubjectID,
		tenant.FromContextOrDefault(ctx).UUID(),
		"https://issuer.example",
		"subject-"+tenantSubjectID.String(),
	).Exec(ctx)
	require.NoError(t, err)

	finiteStart := now.Add(-24 * time.Hour)
	finiteEnd := now.Add(24 * time.Hour)
	finite := &models.Entitlement{
		ID:              uuid.New(),
		TenantSubjectID: tenantSubjectID,
		UserID:          userID,
		Entitlement:     entName,
		StartAt:         finiteStart,
		EndAt:           &finiteEnd,
		SourceType:      models.EntitlementSourceOneOff,
		SourceID:        &finiteSourceID,
		CreatedAt:       finiteStart,
		UpdatedAt:       finiteStart,
	}
	require.NoError(t, r.Insert(ctx, finite))

	indefiniteName := entName + "_indefinite"
	indefinite := &models.Entitlement{
		ID:              uuid.New(),
		TenantSubjectID: tenantSubjectID,
		UserID:          userID,
		Entitlement:     indefiniteName,
		StartAt:         finiteStart,
		SourceType:      models.EntitlementSourceAdmin,
		SourceID:        &indefiniteSourceID,
		CreatedAt:       finiteStart,
		UpdatedAt:       finiteStart,
	}
	require.NoError(t, r.Insert(ctx, indefinite))

	t.Cleanup(func() {
		_, _ = bunDB.NewDelete().
			Model((*models.Entitlement)(nil)).
			Where("user_id = ? AND entitlement IN (?)", userID, bun.In([]string{entName, indefiniteName})).
			Exec(ctx)
	})

	ok, err := r.IsTenantSubjectEntitled(ctx, tenantSubjectID, entName, now)
	require.NoError(t, err)
	require.True(t, ok)

	ok, err = r.IsTenantSubjectEntitled(ctx, otherTenantSubjectID, entName, now)
	require.NoError(t, err)
	require.False(t, ok)

	ok, err = r.HasActiveIndefiniteByTenantSubject(ctx, tenantSubjectID, indefiniteName, now)
	require.NoError(t, err)
	require.True(t, ok)

	latest, err := r.GetLatestFiniteActiveByTenantSubject(ctx, tenantSubjectID, entName, now)
	require.NoError(t, err)
	require.Equal(t, finite.ID, latest.ID)
}
