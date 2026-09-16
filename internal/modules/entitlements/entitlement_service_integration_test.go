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

func TestPushNewEntitlement_CoveredSourceSurvivesOtherSourceRefund(t *testing.T) {

	// The entitlement Service is RLS/merchant-scoped (MerchantTx); provide the
	// test merchant on the ctx (#511: entitlement creation now also goes through
	// the merchant-scoped grant ledger).
	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
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
	require.NotEqual(t, first.ID, covered.ID)

	var count int
	require.NoError(t, dbi.Pool().QueryRow(ctx,
		`SELECT count(*) FROM openrails.entitlements
		 WHERE customer_id = $1 AND entitlement = $2
		   AND revoked_at IS NULL AND deleted_at IS NULL`,
		tenantSubjectID, entName,
	).Scan(&count))
	require.Equal(t, 2, count)
	// Replaying either source must keep its own identity and avoid duplicates.
	replay, err := svc.PushNewEntitlement(ctx, PushNewEntitlementParams{UserID: userID, Entitlement: entName,
		NotBefore: &now, EndAt: &coveredEnd, SourceType: models.EntitlementSourceSubscription, SourceID: coveredSourceID})
	require.NoError(t, err)
	require.Equal(t, covered.ID, replay.ID)
	require.NoError(t, svc.EndActiveByPayment(ctx, firstSourceID, models.EntitlementRevokeRefund))
	entitled, err := svc.IsEntitled(ctx, userID, entName, now.Add(time.Hour))
	require.NoError(t, err)
	require.True(t, entitled)
	entitled, err = svc.IsEntitled(ctx, userID, entName, coveredEnd)
	require.NoError(t, err)
	require.False(t, entitled)
}

func TestPushNewEntitlement_NewPaidPeriodAfterSourceRevocation(t *testing.T) {
	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	pool := dbi.Pool()
	mid := dbtest.TestMerchantID.UUID()
	now := time.Now().UTC().Truncate(time.Second)
	svc := NewEntitlementService(dbi, clockwork.NewFakeClockAt(now))
	user := uuid.NewString()
	customer := dbtest.EnsureCustomerIDPgx(ctx, t, pool, user)
	product, price, sub := uuid.New(), uuid.New(), uuid.New()
	psp := dbtest.EnsureTestPSP(ctx, t, pool, mid, "stripe")
	_, err := pool.Exec(ctx, `INSERT INTO openrails.products(id,merchant_id,key,display_name) VALUES($1,$2,$3,'Renewal')`, product, mid, uuid.NewString())
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO openrails.prices(id,merchant_id,product_id,amount,currency,auto_renew,access_duration_hours) VALUES($1,$2,$3,100,'USD',true,24)`, price, mid, product)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO openrails.subscriptions(id,merchant_id,customer_id,product_id,price_id,rail,psp_id,status) VALUES($1,$2,$3,$4,$5,'stripe',$6,'active')`, sub, mid, customer, product, price, psp)
	require.NoError(t, err)
	end := now.Add(24 * time.Hour)
	req := PushNewEntitlementParams{UserID: user, Entitlement: "renewal-source", NotBefore: &now, EndAt: &end, SourceType: models.EntitlementSourceSubscription, SourceID: sub}
	first, err := svc.PushNewEntitlement(ctx, req)
	require.NoError(t, err)
	require.Nil(t, first.EndAt)
	require.NoError(t, svc.RevokeSourcesForSubscription(ctx, user, sub, models.EntitlementRevokeRefund, models.EntitlementSourceSubscription))
	replay, err := svc.PushNewEntitlement(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, replay.RevokedAt)
	req.NotBefore = &end
	nextEnd := end.Add(24 * time.Hour)
	req.EndAt = &nextEnd
	restored, err := svc.PushNewEntitlement(ctx, req)
	require.NoError(t, err)
	require.NotEqual(t, first.ID, restored.ID)
	require.Nil(t, restored.RevokedAt)
	replay, err = svc.PushNewEntitlement(ctx, req)
	require.NoError(t, err)
	require.Equal(t, restored.ID, replay.ID)
	active, err := svc.IsEntitled(ctx, user, req.Entitlement, end.Add(time.Hour))
	require.NoError(t, err)
	require.True(t, active)
}
