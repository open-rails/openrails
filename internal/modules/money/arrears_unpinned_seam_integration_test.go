//go:build integration

package money_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
)

// or#868 B2: AccrueOwed and the metered-rating watermark opened `s.db.RunInTx`
// under the comment "Privileged (no-GUC) transaction with explicit merchant_id
// predicates". There is no privileged pool. They worked ONLY where an HTTP
// request had already pinned a merchant connection (MerchantDBConnMW) and
// pgxBegin inherited its session GUC. Off that path — pkg/service.FinalizeInvoice,
// MoneyService.SweepUsage, and both embedded seams, which hosts call directly —
// the transaction carried no app.merchant_id and the INSERTs were denied 42501,
// so metered/arrears billing was inoperable on the embedded seam.
//
// Everything below runs the money service on an UNPINNED handle: no middleware,
// no caller-supplied pin, exactly the embedded seam's posture.
func TestArrearsAccruesOnAnUnpinnedHandle(t *testing.T) {
	ctx := context.Background()
	unpinned := dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t))
	merchantID := dbtest.TestMerchantID.UUID()
	pool := dbtest.SharedMerchantPool(t, merchantID)
	dbtest.EnsureTestMerchant(ctx, t, pool)
	mctx := dbtest.WithTestMerchant(ctx)

	payer := identity.CustomerID(dbtest.EnsureCustomerIDPgx(ctx, t, pool, uuid.NewString()))
	svc := money.NewMoneyService(unpinned)

	t.Run("failing_before: the bare RunInTx the watermark used is denied 42501", func(t *testing.T) {
		err := unpinned.RunInTx(mctx, func(ctx context.Context, tx pgx.Tx) error {
			_, e := tx.Exec(ctx, `
INSERT INTO openrails.metered_rating_watermarks
    (merchant_id, customer_id, currency, source, period_from, rated_through, accrued_amount, created_at, updated_at)
VALUES ($1, $2, 'USD', 'or868-b2-probe', now(), now(), 0, now(), now())`,
				merchantID, payer.UUID())
			return e
		})
		require.Error(t, err, "a no-GUC transaction cannot write a policied table; that is the whole finding")
		require.Contains(t, err.Error(), "row-level security policy")
	})

	t.Run("AccrueOwed accrues without a caller-supplied pin", func(t *testing.T) {
		trx, err := svc.AccrueOwed(mctx, payer, money.DefaultCurrency, "usage", "or868-b2-"+uuid.NewString(), 1_500_000)
		require.NoError(t, err, "the embedded seam's arrears accrual must not depend on HTTP middleware")
		require.NotNil(t, trx)

		owed, err := svc.GetOutstandingOwed(mctx, payer, money.DefaultCurrency)
		require.NoError(t, err)
		require.Equal(t, int64(1_500_000), owed)
	})

	t.Run("the metered-rating watermark accrues through FinalizeInvoice", func(t *testing.T) {
		meter := "or868-b2-meter-" + uuid.NewString()[:8]
		productID := uuid.New()
		t.Cleanup(func() {
			_, _ = pool.Exec(ctx, "DELETE FROM openrails.catalog_rate_cards WHERE product_id = $1", productID)
			_, _ = pool.Exec(ctx, "DELETE FROM openrails.catalog_meters WHERE merchant_id = $1 AND key = $2", merchantID, meter)
			_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
		})

		// Catalog + usage are seeded the way a request would write them (pinned);
		// only the SWEEP runs unpinned, so this isolates B2 exactly.
		_, err := pool.Exec(ctx, `
INSERT INTO openrails.products (id, key, display_name, merchant_id) VALUES ($1, $2, $2, $3)`,
			productID, "or868-b2-product-"+uuid.NewString()[:8], merchantID)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, `
INSERT INTO openrails.catalog_meters (merchant_id, key, event_type, value_property, aggregation, unit, group_by)
VALUES ($1, $2, $3, 'units', 'sum', 'unit', '{}'::jsonb)`, merchantID, meter, "or868.b2."+meter)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, `
INSERT INTO openrails.catalog_rate_cards (merchant_id, product_id, ordinal, meter_key, payment_term, price)
VALUES ($1, $2, 1, $3, 'in_arrears',
        '{"model":"per_unit","currency":"USD","per_unit":{"unit_amount":1000}}'::jsonb)`,
			merchantID, productID, meter)
		require.NoError(t, err)

		pinnedSvc := money.NewMoneyService(dbtest.OpenMerchantDB(t, merchantID))
		_, err = pinnedSvc.RecordUsage(mctx, money.RecordUsageParams{
			Payer:      &payer,
			Invoker:    payer.UUID().String(),
			Currency:   money.DefaultCurrency,
			EventType:  "or868.b2." + meter,
			Dimensions: map[string]int64{"units": 7},
			Source:     "or868-b2",
			SourceID:   uuid.NewString(),
			OccurredAt: time.Now(),
		})
		require.NoError(t, err)

		// The statement that used to fail: "new row violates row-level security
		// policy for table metered_rating_watermarks (SQLSTATE 42501)".
		inv, err := svc.FinalizeInvoice(mctx, payer, money.DefaultCurrency,
			time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
		require.NoError(t, err, "the embedded seam's metered sweep must not depend on HTTP middleware")
		require.Equal(t, "open", inv.Status)

		var watermarks int
		require.NoError(t, pool.QueryRow(ctx, `
SELECT count(*) FROM openrails.metered_rating_watermarks
 WHERE customer_id = $1 AND source = $2`, payer.UUID(), "metered:"+meter).Scan(&watermarks))
		require.Equal(t, 1, watermarks, "the watermark row is what makes metered billing exactly-once")
	})
}
