//go:build integration

package reconcile

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/pkg/merchant"
)

// TestRepoCreateStampsOnlyExplicitRailMerchantAccount proves psp_id is
// provenance: repo writes do not invent it from primary routing.
func TestRepoCreateStampsOnlyExplicitRailMerchantAccount(t *testing.T) {
	appDB := startReconcilePostgres(t)
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	dbtest.EnsureTestMerchant(context.Background(), t, appDB.Pool())

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		now := time.Now().UTC()
		suffix := uuid.NewString()[:8]

		account, err := appDB.Gen(ctx).UpsertPSP(ctx, gen.UpsertPSPParams{
			MerchantID:     dbtest.TestMerchantID.UUID(),
			Rail:           "nmi",
			AccountID:      "nmi-account-" + suffix,
			LastVerifiedAt: &now,
		})
		require.NoError(t, err)

		customerID := dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
		productID := uuid.New()
		priceID := uuid.New()
		_, err = appDB.Qx(ctx).Exec(ctx,
			`INSERT INTO openrails.products (id, key, display_name, tier_group, entitlements_spec, merchant_id)
			 VALUES ($1, $2, $2, $3, jsonb_build_object('premium', null), $4)`,
			productID, "stamp-"+suffix, "stamp-"+suffix, dbtest.TestMerchantID.UUID())
		require.NoError(t, err)
		_, err = appDB.Qx(ctx).Exec(ctx,
			`INSERT INTO openrails.prices (id, product_id, amount, currency, access_duration_hours, auto_renew, merchant_id)
			 VALUES ($1, $2, 999, 'usd', 720, true, $3)`,
			priceID, productID, dbtest.TestMerchantID.UUID())
		require.NoError(t, err)

		// Create a payment via the repo WITHOUT setting PspID.
		pmt := &models.Payment{
			ID:            uuid.New(),
			CustomerID:    customerID,
			PriceID:       priceID,
			Rail:          models.RailNMI,
			TransactionID: "txn-stamp-" + suffix,
			Amount:        999,
			ListAmount:    999,
			Currency:      "usd",
			Status:        "completed",
			PurchasedAt:   now,
			CreatedAt:     now,
		}
		require.NoError(t, payments.NewPaymentRepo(appDB).Create(ctx, pmt))

		got, err := payments.NewPaymentRepo(appDB).GetByID(ctx, pmt.ID)
		require.NoError(t, err)
		require.Nil(t, got.PspID, "payment must not invent psp_id from primary routing")

		// #641: a context-pinned account (the per-account inbound webhook path)
		// records observed external-account provenance.
		pinnedCtx := db.WithPSPID(ctx, account.ID)
		pmt2 := &models.Payment{
			ID: uuid.New(), CustomerID: customerID, PriceID: priceID, Rail: models.RailNMI,
			TransactionID: "txn-pin-" + suffix, Amount: 999, ListAmount: 999, Currency: "usd",
			Status: "completed", PurchasedAt: now, CreatedAt: now,
		}
		require.NoError(t, payments.NewPaymentRepo(appDB).Create(pinnedCtx, pmt2))
		got2, err := payments.NewPaymentRepo(appDB).GetByID(ctx, pmt2.ID)
		require.NoError(t, err)
		require.NotNil(t, got2.PspID)
		require.Equal(t, account.ID, *got2.PspID, "pinned account is observed provenance")
		return nil
	}))
}
