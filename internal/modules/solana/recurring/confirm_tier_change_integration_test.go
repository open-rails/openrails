//go:build integration

package recurring

import (
	"context"
	"errors"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/solana/solanasubs"
	submod "github.com/open-rails/openrails/internal/modules/subscriptions"
)

type failOnceTierChangeStore struct {
	repo *solanasubs.SolanaSubscriptionRepo
	err  error
}

func (s *failOnceTierChangeStore) GetBySubscriptionID(ctx context.Context, id uuid.UUID) (*models.SolanaSubscription, error) {
	return s.repo.GetBySubscriptionID(ctx, id)
}

func (s *failOnceTierChangeStore) GetBySubscriptionPDA(ctx context.Context, pda string) (*models.SolanaSubscription, error) {
	return s.repo.GetBySubscriptionPDA(ctx, pda)
}

func (s *failOnceTierChangeStore) UpsertTx(ctx context.Context, txDB *db.DB, row *models.SolanaSubscription) error {
	if s.err != nil {
		err := s.err
		s.err = nil
		return err
	}
	return s.repo.UpsertTx(ctx, txDB, row)
}

func (s *failOnceTierChangeStore) SetStatusTx(ctx context.Context, txDB *db.DB, id uuid.UUID, status string) error {
	return s.repo.SetStatusTx(ctx, txDB, id, status)
}

// #955: a failure after the new membership is created must roll the entire
// local tier-change mirror back. A retry then completes with exactly one live
// membership in the tier group and matching old/new Solana mirror statuses.
func TestConfirmTierChange_RollsBackPartialMirrorAndRetries(t *testing.T) {
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	merchantID := dbtest.TestMerchantID.UUID()
	now := time.Now().UTC().Truncate(time.Second)
	suffix := uuid.NewString()[:8]
	oldProductID, newProductID := uuid.New(), uuid.New()
	oldPriceID, newPriceID := uuid.New(), uuid.New()
	oldSubID := uuid.New()
	userID := uuid.NewString()
	customerID := dbtest.EnsureCustomerIDPgx(context.Background(), t, dbi.Pool(), userID)
	pspID := dbtest.EnsureTestPSP(context.Background(), t, dbi.Pool(), merchantID, string(models.RailSolana))
	ctx := db.WithPSPID(dbtest.WithTestMerchant(context.Background()), pspID)

	exec := func(query string, args ...any) {
		t.Helper()
		_, err := dbi.Pool().Exec(ctx, query, args...)
		require.NoError(t, err)
	}
	tierGroup := "tier-change-" + suffix
	exec(`INSERT INTO openrails.products (id, key, display_name, tier_group, entitlements_spec, merchant_id)
	      VALUES ($1,$2,$2,$3,'{}'::jsonb,$4), ($5,$6,$6,$3,'{}'::jsonb,$4)`,
		oldProductID, "old-"+suffix, tierGroup, merchantID, newProductID, "new-"+suffix)
	cycleHours := int32(720)
	exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, access_duration_hours, auto_renew, merchant_id)
	      VALUES ($1,$2,1000000,'USD',$3,true,$4), ($5,$6,2000000,'USD',$3,true,$4)`,
		oldPriceID, oldProductID, cycleHours, merchantID, newPriceID, newProductID)
	periodStart, periodEnd := now.Add(-24*time.Hour), now.Add(29*24*time.Hour)
	exec(`INSERT INTO openrails.subscriptions
	        (id, price_id, product_id, status, rail, rail_subscription_id,
	         current_period_starts_at, current_period_ends_at, started_at,
	         entitlements_spec_snapshot, customer_id, merchant_id, psp_id)
	      VALUES ($1,$2,$3,'active','solana',$4,$5,$6,$5,'{}'::jsonb,$7,$8,$9)`,
		oldSubID, oldPriceID, oldProductID, "old-pda-"+suffix, periodStart, periodEnd, customerID, merchantID, pspID)

	merchantKey, err := solanago.NewRandomPrivateKey()
	require.NoError(t, err)
	subscriberKey, err := solanago.NewRandomPrivateKey()
	require.NoError(t, err)
	repo := solanasubs.NewSolanaSubscriptionRepo(dbi)
	oldMirror := &models.SolanaSubscription{
		ID:                       uuid.New(),
		MerchantID:               merchantID,
		SubscriptionID:           oldSubID,
		SubscriberWallet:         subscriberKey.PublicKey().String(),
		AuthorityPDA:             subscriberKey.PublicKey().String(),
		SubscriptionPDA:          "old-pda-" + suffix,
		PlanPDA:                  merchantKey.PublicKey().String(),
		MerchantAddress:          merchantKey.PublicKey().String(),
		Mint:                     subscriberKey.PublicKey().String(),
		PlanCreatedAtFingerprint: now.Unix(),
		NextPullAt:               periodEnd,
		Status:                   models.SolanaSubscriptionActive,
	}
	require.NoError(t, repo.Upsert(ctx, oldMirror))

	t.Cleanup(func() {
		_, _ = dbi.Pool().Exec(ctx, `DELETE FROM openrails.notification_queue WHERE customer_id=$1`, customerID)
		_, _ = dbi.Pool().Exec(ctx, `DELETE FROM openrails.entitlements WHERE customer_id=$1`, customerID)
		_, _ = dbi.Pool().Exec(ctx, `DELETE FROM openrails.grants WHERE customer_id=$1`, customerID)
		_, _ = dbi.Pool().Exec(ctx, `DELETE FROM openrails.payments WHERE customer_id=$1`, customerID)
		_, _ = dbi.Pool().Exec(ctx, `DELETE FROM openrails.solana_subscriptions WHERE subscription_id IN (SELECT id FROM openrails.subscriptions WHERE customer_id=$1)`, customerID)
		_, _ = dbi.Pool().Exec(ctx, `DELETE FROM openrails.subscriptions WHERE customer_id=$1`, customerID)
		_, _ = dbi.Pool().Exec(ctx, `DELETE FROM openrails.prices WHERE id=ANY($1)`, []uuid.UUID{oldPriceID, newPriceID})
		_, _ = dbi.Pool().Exec(ctx, `DELETE FROM openrails.products WHERE id=ANY($1)`, []uuid.UUID{oldProductID, newProductID})
	})

	productSvc := catalog.NewProductService(dbi)
	priceSvc := catalog.NewPriceService(dbi)
	entitlementSvc := entitlements.NewEntitlementService(dbi)
	lifecycle := submod.NewSubscriptionLifecycleService(dbi, productSvc, priceSvc, entitlementSvc, submod.NewNotificationService(dbi, nil), nil)
	injected := errors.New("injected mirror upsert failure")
	store := &failOnceTierChangeStore{repo: repo, err: injected}
	svc := NewConfirmTierChangeService(&fakeConfirmRPC{outcome: okOutcome()}, lifecycle, store, dbi, "mainnet")
	svc.now = func() time.Time { return now }
	newPDA := "new-pda-" + suffix
	in := ConfirmTierChangeInput{
		Signature:            solanago.Signature{}.String(),
		OldSubscriptionID:    oldSubID,
		UserID:               userID,
		NewPriceID:           newPriceID,
		NewSubscriptionPDA:   newPDA,
		NewPlanID:            2,
		NewMintSymbol:        "USDC",
		NewAmountBaseUnits:   2_000_000,
		NewPeriodHours:       720,
		NewPlanCreatedAt:     now.Unix(),
		NewFiatAmount:        2_000_000,
		NewCurrency:          "USD",
		IsUpgrade:            true,
		FirstChargeBaseUnits: 1_000_000,
	}

	_, err = svc.Confirm(ctx, in)
	require.ErrorIs(t, err, injected)
	assertTierChangeState(t, ctx, dbi, customerID, tierGroup, oldSubID, uuid.Nil, 1, models.StatusActive)
	var queued int
	require.NoError(t, dbi.Pool().QueryRow(ctx, `SELECT count(*) FROM openrails.notification_queue WHERE customer_id=$1`, customerID).Scan(&queued))
	require.Zero(t, queued, "a rolled-back tier change must not leave or dispatch notification work")
	_, err = repo.GetBySubscriptionPDA(ctx, newPDA)
	require.Error(t, err, "the failed transaction must not leave the new mirror's idempotency key")
	oldAfterFailure, err := repo.GetBySubscriptionID(ctx, oldSubID)
	require.NoError(t, err)
	require.Equal(t, models.SolanaSubscriptionActive, oldAfterFailure.Status)

	result, err := svc.Confirm(ctx, in)
	require.NoError(t, err)
	require.False(t, result.AlreadyConfirmed)
	assertTierChangeState(t, ctx, dbi, customerID, tierGroup, oldSubID, result.NewSubscription.ID, 1, models.StatusCancelled)
	newMirror, err := repo.GetBySubscriptionPDA(ctx, newPDA)
	require.NoError(t, err)
	require.Equal(t, result.NewSubscription.ID, newMirror.SubscriptionID)
	require.Equal(t, models.SolanaSubscriptionActive, newMirror.Status)
	oldAfterRetry, err := repo.GetBySubscriptionID(ctx, oldSubID)
	require.NoError(t, err)
	require.Equal(t, models.SolanaSubscriptionCancelled, oldAfterRetry.Status)
}

func assertTierChangeState(t *testing.T, ctx context.Context, dbi *db.DB, customerID uuid.UUID, tierGroup string, oldSubID, newSubID uuid.UUID, wantLive int, wantOldStatus models.SubscriptionStatus) {
	t.Helper()
	var live int
	require.NoError(t, dbi.Pool().QueryRow(ctx,
		`SELECT count(*) FROM openrails.subscriptions
		 WHERE customer_id=$1 AND tier_group=$2 AND deleted_at IS NULL
		   AND status IN ('active','pending','past_due')`, customerID, tierGroup).Scan(&live))
	require.Equal(t, wantLive, live)
	var oldStatus models.SubscriptionStatus
	require.NoError(t, dbi.Pool().QueryRow(ctx, `SELECT status FROM openrails.subscriptions WHERE id=$1`, oldSubID).Scan(&oldStatus))
	require.Equal(t, wantOldStatus, oldStatus)
	if newSubID != uuid.Nil {
		var newStatus models.SubscriptionStatus
		require.NoError(t, dbi.Pool().QueryRow(ctx, `SELECT status FROM openrails.subscriptions WHERE id=$1`, newSubID).Scan(&newStatus))
		require.Equal(t, models.StatusActive, newStatus)
	}
}
