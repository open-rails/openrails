//go:build integration

package reconcile

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	repo "github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

// TestRepoCreateStampsPrimaryProviderAccount proves the #641 stamping chokepoint:
// a payment created via the repo with no explicit provider account is stamped
// with the merchant's PRIMARY enabled account for its rail.
func TestRepoCreateStampsPrimaryProviderAccount(t *testing.T) {
	appDB := startReconcilePostgres(t)
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	dbtest.EnsureTestMerchant(context.Background(), t, appDB.Pool())

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		now := time.Now().UTC()
		suffix := uuid.NewString()[:8]

		// A secondary account exists too, to prove the resolver picks PRIMARY.
		primary, err := appDB.Gen(ctx).UpsertProviderAccount(ctx, gen.UpsertProviderAccountParams{
			MerchantID:     dbtest.TestMerchantID.UUID(),
			ProviderType:   "nmi",
			AccountID:      "nmi-primary-" + suffix,
			LastVerifiedAt: &now,
		})
		require.NoError(t, err)
		require.Equal(t, "primary", primary.Role)
		secondary, err := appDB.Gen(ctx).UpsertProviderAccount(ctx, gen.UpsertProviderAccountParams{
			MerchantID:     dbtest.TestMerchantID.UUID(),
			ProviderType:   "nmi",
			AccountID:      "nmi-secondary-" + suffix,
			LastVerifiedAt: &now,
		})
		require.NoError(t, err)
		require.Equal(t, "secondary", secondary.Role)

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

		// Create a payment via the repo WITHOUT setting ProviderAccountID.
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
		require.NoError(t, repo.NewPaymentRepo(appDB).Create(ctx, pmt))

		got, err := repo.NewPaymentRepo(appDB).GetByID(ctx, pmt.ID)
		require.NoError(t, err)
		require.NotNil(t, got.ProviderAccountID, "payment must be stamped with the primary provider account (#641)")
		require.Equal(t, primary.ID, *got.ProviderAccountID)
		return nil
	}))
}
