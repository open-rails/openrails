//go:build integration

package reconcile

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Retained failed-payment history is time-ordered and merchant-isolated.
func TestPGHistorySource(t *testing.T) {
	appDB := startReconcilePostgres(t)
	merchantA := dbtest.TestMerchantID
	ctxA := merchant.WithID(context.Background(), merchantA)

	merchantB := merchant.ID(uuid.New())
	ctxB := merchant.WithID(context.Background(), merchantB)

	sfx := uuid.NewString()[:8]
	t1 := time.Date(2019, 5, 1, 12, 0, 0, 0, time.UTC) // older failed payment
	t2 := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) // failed payment

	prodA, priceA := uuid.New(), uuid.New()
	prodB, priceB := uuid.New(), uuid.New()
	payFailedA, payCompletedA, payFailedB := uuid.New(), uuid.New(), uuid.New()
	payOlderA, payOtherRailA := uuid.New(), uuid.New()
	var custA, custB uuid.UUID

	seed := func(ctx context.Context, mid uuid.UUID, prodID, priceID uuid.UUID, tag string) {
		exec := func(sql string, args ...any) {
			_, err := appDB.Qx(ctx).Exec(ctx, sql, args...)
			require.NoError(t, err)
		}
		exec(`INSERT INTO billing.products (id, key, display_name, entitlements_spec, merchant_id) VALUES ($1,$2,$2,'{}'::jsonb,$3)`,
			prodID, "hist-"+tag+"-"+sfx, mid)
		exec(`INSERT INTO billing.prices (id, product_id, amount, currency, access_duration_hours, auto_renew, merchant_id) VALUES ($1,$2,9990000,'USD',720,true,$3)`,
			priceID, prodID, mid)
	}

	require.NoError(t, appDB.RunInMerchantConn(ctxA, func(ctx context.Context) error {
		custA = dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
		seed(ctx, merchantA.UUID(), prodA, priceA, "a")
		pspA := dbtest.EnsureTestPSP(ctx, t, appDB.Qx(ctx), merchantA.UUID(), "nmi")
		exec := func(sql string, args ...any) {
			_, err := appDB.Qx(ctx).Exec(ctx, sql, args...)
			require.NoError(t, err)
		}
		// Older payment evidence and an off-rail control remain ordinary receipts.
		exec(`INSERT INTO billing.payments (id, merchant_id, customer_id, price_id, rail, transaction_id, amount, list_amount, currency, status, purchased_at, psp_id)
		      VALUES ($1,$2,$3,$4,'nmi',$5,9990000,9990000,'USD','failed',$6,$7)`,
			payOlderA, merchantA.UUID(), custA, priceA, "older-txn-"+sfx, t1, pspA)
		pspOther := dbtest.EnsureTestPSP(ctx, t, appDB.Qx(ctx), merchantA.UUID(), "ccbill")
		exec(`INSERT INTO billing.payments (id, merchant_id, customer_id, price_id, rail, transaction_id, amount, list_amount, currency, status, purchased_at, psp_id)
		      VALUES ($1,$2,$3,$4,'ccbill',$5,9990000,9990000,'USD','failed',$6,$7)`,
			payOtherRailA, merchantA.UUID(), custA, priceA, "offrail-txn-"+sfx, t1, pspOther)
		// Failed payment = go-forward dunning evidence.
		exec(`INSERT INTO billing.payments (id, merchant_id, customer_id, price_id, rail, transaction_id, amount, list_amount, currency, status, purchased_at, psp_id)
		      VALUES ($1,$2,$3,$4,'nmi',$5,9990000,9990000,'USD','failed',$6,$7)`,
			payFailedA, merchantA.UUID(), custA, priceA, "fail-txn-"+sfx, t2, pspA)
		// Completed payment: not dunning evidence.
		exec(`INSERT INTO billing.payments (id, merchant_id, customer_id, price_id, rail, transaction_id, amount, list_amount, currency, status, purchased_at, psp_id)
		      VALUES ($1,$2,$3,$4,'nmi',$5,9990000,9990000,'USD','completed',$6,$7)`,
			payCompletedA, merchantA.UUID(), custA, priceA, "ok-txn-"+sfx, t2, pspA)
		return nil
	}))

	// Merchant B: same-shaped evidence that must never leak into A's report.
	_, err := appDB.Pool().Exec(context.Background(),
		`INSERT INTO billing.merchants (id, slug, status) VALUES ($1,$2,'active')`, merchantB.UUID(), "hist-b-"+sfx)
	require.NoError(t, err)
	require.NoError(t, appDB.RunInMerchantConn(ctxB, func(ctx context.Context) error {
		exec := func(sql string, args ...any) {
			_, err := appDB.Qx(ctx).Exec(ctx, sql, args...)
			require.NoError(t, err)
		}
		custB = uuid.New()
		exec(`INSERT INTO billing.customers (id, merchant_id) VALUES ($1, $2)`, custB, merchantB.UUID())
		seed(ctx, merchantB.UUID(), prodB, priceB, "b")
		pspB := dbtest.EnsureTestPSP(ctx, t, appDB.Qx(ctx), merchantB.UUID(), "nmi")
		exec(`INSERT INTO billing.payments (id, merchant_id, customer_id, price_id, rail, transaction_id, amount, list_amount, currency, status, purchased_at, psp_id)
		      VALUES ($1,$2,$3,$4,'nmi',$5,9990000,9990000,'USD','failed',$6,$7)`,
			payFailedB, merchantB.UUID(), custB, priceB, "fail-txn-b-"+sfx, t2, pspB)
		return nil
	}))

	t.Cleanup(func() {
		for _, c := range []struct {
			ctx context.Context
			mid uuid.UUID
		}{{ctxA, merchantA.UUID()}, {ctxB, merchantB.UUID()}} {
			_ = appDB.RunInMerchantConn(c.ctx, func(ctx context.Context) error {
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM billing.payments WHERE id=ANY($1)`, []uuid.UUID{payFailedA, payCompletedA, payFailedB, payOlderA, payOtherRailA})
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM billing.prices WHERE id=ANY($1)`, []uuid.UUID{priceA, priceB})
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM billing.products WHERE id=ANY($1)`, []uuid.UUID{prodA, prodB})
				return nil
			})
		}
		_, _ = appDB.Pool().Exec(context.Background(), `DELETE FROM billing.merchants WHERE id=$1`, merchantB.UUID())
	})

	src := NewPGHistorySource(appDB)
	require.True(t, src.Configured())

	t.Run("returns failed payments oldest-first", func(t *testing.T) {
		var events []HistoryEvent
		require.NoError(t, appDB.RunInMerchantConn(ctxA, func(ctx context.Context) error {
			var err error
			events, err = src.ListEvents(ctx, []string{"nmi"}, time.Time{}, time.Time{})
			return err
		}))
		require.Len(t, events, 2)

		older := events[0]
		require.Equal(t, "payments", older.Table)
		require.Equal(t, "charge_failure", older.EventType)
		require.Equal(t, "nmi", older.Rail)
		require.Empty(t, older.RailSubscriptionID)
		require.Equal(t, "older-txn-"+sfx, older.RailTransactionID)
		require.Equal(t, "failed", older.Status)
		require.NotNil(t, older.AmountMicros)
		require.Equal(t, int64(9_990_000), *older.AmountMicros)
		require.True(t, older.OccurredAt.Equal(t1))

		failed := events[1]
		require.Equal(t, "payments", failed.Table)
		require.Equal(t, "charge_failure", failed.EventType)
		require.Equal(t, "fail-txn-"+sfx, failed.RailTransactionID)
		require.Equal(t, "failed", failed.Status)
		require.True(t, failed.OccurredAt.Equal(t2))
	})

	t.Run("window bounds apply", func(t *testing.T) {
		var events []HistoryEvent
		require.NoError(t, appDB.RunInMerchantConn(ctxA, func(ctx context.Context) error {
			var err error
			events, err = src.ListEvents(ctx, []string{"nmi"}, t1.Add(time.Hour), time.Time{})
			return err
		}))
		require.Len(t, events, 1)
		require.Equal(t, "payments", events[0].Table)
	})

	t.Run("merchant isolation", func(t *testing.T) {
		var events []HistoryEvent
		require.NoError(t, appDB.RunInMerchantConn(ctxB, func(ctx context.Context) error {
			var err error
			events, err = src.ListEvents(ctx, []string{"nmi"}, time.Time{}, time.Time{})
			return err
		}))
		require.Len(t, events, 1)
		for _, ev := range events {
			require.NotContains(t, ev.RailTransactionID, "fail-txn-"+sfx)
			require.NotEqual(t, "older-txn-"+sfx, ev.RailTransactionID)
		}
	})
}
