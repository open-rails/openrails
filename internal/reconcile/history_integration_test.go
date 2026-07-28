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

// #735: the PG-backed reconcile history source merges imported legacy dunning
// rows with failed payments, time-ordered, merchant-isolated (RLS + explicit
// merchant filter). It replaced the ClickHouse DunningHistoryService.
func TestPGHistorySource(t *testing.T) {
	appDB := startReconcilePostgres(t)
	merchantA := dbtest.TestMerchantID
	ctxA := merchant.WithID(context.Background(), merchantA)

	merchantB := merchant.ID(uuid.New())
	ctxB := merchant.WithID(context.Background(), merchantB)

	sfx := uuid.NewString()[:8]
	t1 := time.Date(2019, 5, 1, 12, 0, 0, 0, time.UTC) // imported legacy event
	t2 := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) // failed payment

	prodA, priceA := uuid.New(), uuid.New()
	prodB, priceB := uuid.New(), uuid.New()
	payFailedA, payCompletedA, payFailedB := uuid.New(), uuid.New(), uuid.New()
	var custA, custB uuid.UUID

	seed := func(ctx context.Context, mid uuid.UUID, prodID, priceID uuid.UUID, tag string) {
		exec := func(sql string, args ...any) {
			_, err := appDB.Qx(ctx).Exec(ctx, sql, args...)
			require.NoError(t, err)
		}
		exec(`INSERT INTO openrails.products (id, key, display_name, entitlements_spec, merchant_id) VALUES ($1,$2,$2,'{}'::jsonb,$3)`,
			prodID, "hist-"+tag+"-"+sfx, mid)
		exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, access_duration_hours, auto_renew, merchant_id) VALUES ($1,$2,9990000,'usd',720,true,$3)`,
			priceID, prodID, mid)
	}

	require.NoError(t, appDB.RunInMerchantConn(ctxA, func(ctx context.Context) error {
		custA = dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
		seed(ctx, merchantA.UUID(), prodA, priceA, "a")
		exec := func(sql string, args ...any) {
			_, err := appDB.Qx(ctx).Exec(ctx, sql, args...)
			require.NoError(t, err)
		}
		// Imported legacy dunning row with correlation keys in detail.
		exec(`INSERT INTO openrails.imported_dunning_history (merchant_id, event_type, rail, occurred_at, source, detail)
		      VALUES ($1,'charge_failure','nmi',$2,'doujins_users_logs',
		              '{"rail_subscription_id":"legacy-sub-`+sfx+`","rail_transaction_id":"legacy-txn-`+sfx+`","status":"declined","amount_micros":9990000}'::jsonb)`,
			merchantA.UUID(), t1)
		// Off-rail imported row: must not surface for a ["nmi"] query.
		exec(`INSERT INTO openrails.imported_dunning_history (merchant_id, event_type, rail, occurred_at, source)
		      VALUES ($1,'charge_failure','ccbill',$2,'doujins_users_logs')`, merchantA.UUID(), t1)
		// Failed payment = go-forward dunning evidence.
		exec(`INSERT INTO openrails.payments (id, merchant_id, customer_id, price_id, rail, transaction_id, amount, list_amount, currency, status, purchased_at)
		      VALUES ($1,$2,$3,$4,'nmi',$5,9990000,9990000,'usd','failed',$6)`,
			payFailedA, merchantA.UUID(), custA, priceA, "fail-txn-"+sfx, t2)
		// Completed payment: not dunning evidence.
		exec(`INSERT INTO openrails.payments (id, merchant_id, customer_id, price_id, rail, transaction_id, amount, list_amount, currency, status, purchased_at)
		      VALUES ($1,$2,$3,$4,'nmi',$5,9990000,9990000,'usd','completed',$6)`,
			payCompletedA, merchantA.UUID(), custA, priceA, "ok-txn-"+sfx, t2)
		return nil
	}))

	// Merchant B: same-shaped evidence that must never leak into A's report.
	_, err := appDB.Pool().Exec(context.Background(),
		`INSERT INTO openrails.merchants (id, slug, status) VALUES ($1,$2,'active')`, merchantB.UUID(), "hist-b-"+sfx)
	require.NoError(t, err)
	require.NoError(t, appDB.RunInMerchantConn(ctxB, func(ctx context.Context) error {
		exec := func(sql string, args ...any) {
			_, err := appDB.Qx(ctx).Exec(ctx, sql, args...)
			require.NoError(t, err)
		}
		custB = uuid.New()
		exec(`INSERT INTO openrails.customers (id, merchant_id, subject) VALUES ($1,$2,$3)`, custB, merchantB.UUID(), custB.String())
		seed(ctx, merchantB.UUID(), prodB, priceB, "b")
		exec(`INSERT INTO openrails.imported_dunning_history (merchant_id, event_type, rail, occurred_at, source)
		      VALUES ($1,'charge_failure','nmi',$2,'mobius_schedulers')`, merchantB.UUID(), t1)
		exec(`INSERT INTO openrails.payments (id, merchant_id, customer_id, price_id, rail, transaction_id, amount, list_amount, currency, status, purchased_at)
		      VALUES ($1,$2,$3,$4,'nmi',$5,9990000,9990000,'usd','failed',$6)`,
			payFailedB, merchantB.UUID(), custB, priceB, "fail-txn-b-"+sfx, t2)
		return nil
	}))

	t.Cleanup(func() {
		for _, c := range []struct {
			ctx context.Context
			mid uuid.UUID
		}{{ctxA, merchantA.UUID()}, {ctxB, merchantB.UUID()}} {
			_ = appDB.RunInMerchantConn(c.ctx, func(ctx context.Context) error {
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.imported_dunning_history WHERE merchant_id=$1`, c.mid)
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.payments WHERE id=ANY($1)`, []uuid.UUID{payFailedA, payCompletedA, payFailedB})
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.prices WHERE id=ANY($1)`, []uuid.UUID{priceA, priceB})
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.products WHERE id=ANY($1)`, []uuid.UUID{prodA, prodB})
				return nil
			})
		}
		_, _ = appDB.Pool().Exec(context.Background(), `DELETE FROM openrails.merchants WHERE id=$1`, merchantB.UUID())
	})

	src := NewPGHistorySource(appDB)
	require.True(t, src.Configured())

	t.Run("merges imported rows and failed payments oldest-first", func(t *testing.T) {
		var events []HistoryEvent
		require.NoError(t, appDB.RunInMerchantConn(ctxA, func(ctx context.Context) error {
			var err error
			events, err = src.ListEvents(ctx, []string{"nmi"}, time.Time{}, time.Time{})
			return err
		}))
		require.Len(t, events, 2)

		imported := events[0]
		require.Equal(t, "imported_dunning_history", imported.Table)
		require.Equal(t, "charge_failure", imported.EventType)
		require.Equal(t, "nmi", imported.Rail)
		require.Equal(t, "legacy-sub-"+sfx, imported.RailSubscriptionID)
		require.Equal(t, "legacy-txn-"+sfx, imported.RailTransactionID)
		require.Equal(t, "declined", imported.Status)
		require.NotNil(t, imported.AmountMicros)
		require.Equal(t, int64(9_990_000), *imported.AmountMicros)
		require.True(t, imported.OccurredAt.Equal(t1))

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
		require.Len(t, events, 2)
		for _, ev := range events {
			require.NotContains(t, ev.RailTransactionID, "fail-txn-"+sfx)
			require.NotEqual(t, "legacy-sub-"+sfx, ev.RailSubscriptionID)
		}
	})
}
