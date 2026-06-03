//go:build integration

package entitlements

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
)

func TestPushNewEntitlement_CoveredFiniteGrantReturnsExistingWindow(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)

	sqlDB := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	t.Cleanup(func() { _ = sqlDB.Close() })
	bunDB := bun.NewDB(sqlDB, pgdialect.New())
	models.RegisterModels(bunDB)

	ctx := context.Background()
	require.NoError(t, bunDB.PingContext(ctx))

	dbi, err := db.NewWithBun(bunDB)
	require.NoError(t, err)

	now := time.Now().UTC().Truncate(time.Second)
	svc := NewEntitlementService(dbi, clockwork.NewFakeClockAt(now))

	userID := uuid.New().String()
	entName := "premium_covered_finite_" + uuid.New().String()
	firstSourceID := uuid.New()
	coveredSourceID := uuid.New()

	firstEnd := now.Add(30 * 24 * time.Hour)
	first, err := svc.PushNewEntitlement(ctx, PushNewEntitlementParams{
		UserID:      userID,
		Entitlement: entName,
		NotBefore:   &now,
		EndAt:       &firstEnd,
		SourceType:  models.EntitlementSourceOneOff,
		SourceID:    firstSourceID,
	})
	require.NoError(t, err)
	require.NotNil(t, first)

	coveredEnd := now.Add(10 * 24 * time.Hour)
	covered, err := svc.PushNewEntitlement(ctx, PushNewEntitlementParams{
		UserID:      userID,
		Entitlement: entName,
		NotBefore:   &now,
		EndAt:       &coveredEnd,
		SourceType:  models.EntitlementSourceSubscription,
		SourceID:    coveredSourceID,
	})
	require.NoError(t, err)
	require.NotNil(t, covered)
	require.Equal(t, first.ID, covered.ID)

	count, err := bunDB.NewSelect().
		Model((*models.Entitlement)(nil)).
		Where("user_id = ?", userID).
		Where("entitlement = ?", entName).
		Where("revoked_at IS NULL").
		Where("deleted_at IS NULL").
		Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, count)
}
