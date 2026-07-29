//go:build integration

package entitlements

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/stretchr/testify/require"
)

func TestPushNewEntitlement_CoveredFiniteGrantReturnsExistingWindow(t *testing.T) {
	dsn := dbtest.MerchantPinnedDSN(t, dbtest.TestMerchantID.UUID())

	// The entitlement Service is RLS/merchant-scoped (MerchantTx); provide the
	// test merchant on the ctx (#511: entitlement creation now also goes through
	// the merchant-scoped grant ledger).
	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenAppDB(t, dsn)
	dbtest.EnsureTestMerchant(ctx, t, dbi.Pool())

	now := time.Now().UTC().Truncate(time.Second)
	svc := NewEntitlementService(dbi, clockwork.NewFakeClockAt(now))

	userID := uuid.New().String()
	tenantSubjectID := dbtest.EnsureCustomerIDPgx(ctx, t, dbi.Pool(), userID)
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

	var count int
	require.NoError(t, dbi.Pool().QueryRow(ctx,
		`SELECT count(*) FROM openrails.entitlements
		 WHERE customer_id = $1 AND entitlement = $2
		   AND revoked_at IS NULL AND deleted_at IS NULL`,
		tenantSubjectID, entName,
	).Scan(&count))
	require.Equal(t, 1, count)
}
