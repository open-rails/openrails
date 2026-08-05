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

// TestRepoCreateRequiresObservedPSPProvenance proves psp_id is provenance and
// nothing else: the repo never invents one from primary routing, and or#893
// turned the old "write it unattributed" outcome into a REFUSAL — an
// unattributed provider charge is not representable. Off-rail channels, which
// have no provider at all, are the one lane that still writes without a PSP.
func TestRepoCreateRequiresObservedPSPProvenance(t *testing.T) {
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
			 VALUES ($1, $2, 999, 'USD', 720, true, $3)`,
			priceID, productID, dbtest.TestMerchantID.UUID())
		require.NoError(t, err)

		// A charge on a REAL rail with no observed PSP is refused, before any
		// write — not stored with NULL provenance.
		pmt := &models.Payment{
			ID:            uuid.New(),
			CustomerID:    customerID,
			PriceID:       priceID,
			Rail:          models.RailNMI,
			TransactionID: "txn-stamp-" + suffix,
			Amount:        999,
			ListAmount:    999,
			Currency:      "USD",
			Status:        "completed",
			MoneyMovement: models.MoneyMovementRail,
			PurchasedAt:   now,
			CreatedAt:     now,
		}
		err = payments.NewPaymentRepo(appDB).Create(ctx, pmt)
		require.ErrorIs(t, err, db.ErrNoPSPInContext, "an unattributed provider charge must refuse, not write NULL")
		_, err = payments.NewPaymentRepo(appDB).GetByID(ctx, pmt.ID)
		require.Error(t, err, "the refused charge left no row")

		// Off-rail money genuinely has no provider account, so it writes without
		// one — the exemption payments_psp_required_on_rail names.
		manual := &models.Payment{
			ID: uuid.New(), CustomerID: customerID, PriceID: priceID,
			Rail:          models.Rail(models.ChannelManual),
			TransactionID: "txn-manual-" + suffix, Amount: 999, ListAmount: 999, Currency: "USD",
			Status: "completed", MoneyMovement: models.MoneyMovementRail, PurchasedAt: now, CreatedAt: now,
		}
		require.NoError(t, payments.NewPaymentRepo(appDB).Create(ctx, manual))
		gotManual, err := payments.NewPaymentRepo(appDB).GetByID(ctx, manual.ID)
		require.NoError(t, err)
		require.Nil(t, gotManual.PspID, "a channel has no PSP to attribute")

		// #641: a context-pinned account (the per-account inbound webhook path)
		// records observed external-account provenance.
		pinnedCtx := db.WithPSPID(ctx, account.ID)
		pmt2 := &models.Payment{
			ID: uuid.New(), CustomerID: customerID, PriceID: priceID, Rail: models.RailNMI,
			TransactionID: "txn-pin-" + suffix, Amount: 999, ListAmount: 999, Currency: "USD",
			Status: "completed", MoneyMovement: models.MoneyMovementRail, PurchasedAt: now, CreatedAt: now,
		}
		require.NoError(t, payments.NewPaymentRepo(appDB).Create(pinnedCtx, pmt2))
		got2, err := payments.NewPaymentRepo(appDB).GetByID(ctx, pmt2.ID)
		require.NoError(t, err)
		require.NotNil(t, got2.PspID)
		require.Equal(t, account.ID, *got2.PspID, "pinned account is observed provenance")
		return nil
	}))
}
