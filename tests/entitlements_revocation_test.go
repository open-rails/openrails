//go:build integration

package tests

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/stretchr/testify/require"
)

func TestEntitlements_RevokeExistingEntitlement_DropsAccessImmediately(t *testing.T) {
	suite := setupTestSuite(t)
	rt := suite.App.Runtime
	require.NotNil(t, rt)
	require.NotNil(t, rt.EntitlementService)

	ctx := context.Background()
	baseNow := time.Now().UTC().Truncate(time.Second)
	t0 := baseNow.Add(-30 * 24 * time.Hour)
	clock := suite.SetMockClock(t0)
	require.IsType(t, &clockwork.FakeClock{}, clock)

	userID := uuid.New().String()
	subID := uuid.New()
	notBefore := clock.Now().UTC()
	endAt := clock.Now().UTC().Add(30 * 24 * time.Hour)

	_, err := rt.EntitlementService.PushNewEntitlement(ctx, entitlements.PushNewEntitlementParams{
		UserID:      userID,
		Entitlement: "premium",
		NotBefore:   &notBefore,
		EndAt:       &endAt,
		SourceType:  models.EntitlementSourceSubscription,
		SourceID:    subID,
	})
	require.NoError(t, err)

	ok, err := rt.EntitlementService.IsEntitled(ctx, userID, "premium", clock.Now().UTC().Add(time.Second))
	require.NoError(t, err)
	require.True(t, ok)

	st := models.EntitlementSourceSubscription
	sid := subID
	require.NoError(t, rt.EntitlementService.RevokeExistingEntitlement(ctx, entitlements.RevokeExistingEntitlementParams{
		UserID:      userID,
		Entitlement: "premium",
		SourceType:  &st,
		SourceID:    &sid,
		Reason:      models.EntitlementRevokeChargeback,
	}))

	ok, err = rt.EntitlementService.IsEntitled(ctx, userID, "premium", clock.Now().UTC().Add(time.Second))
	require.NoError(t, err)
	require.False(t, ok)
}
