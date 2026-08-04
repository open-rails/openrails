//go:build integration

package controlplane

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

// #827: the payment-settlements host feed must be merchant-scoped end to end —
// the controlplane functions by explicit predicate (the controlplane pool is
// privileged) and payment_settlement_events by fail-closed RLS for the
// openrails_app (NOBYPASSRLS) role.
func TestPaymentSettlementsCrossMerchantIsolation(t *testing.T) {
	ctx := context.Background()
	_, appDSN := dbtest.SharedRLSPostgres(t)
	// Fixture seeding crosses merchants in one statement, and the control plane
	// itself reads across them — both need the privileged role, not the
	// RLS-forced app role SharedPGXPool now hands out (or#782).
	super := dbtest.SharedSuperuserPGXPool(t)

	appPool, err := pgxpool.New(ctx, appDSN)
	require.NoError(t, err)
	t.Cleanup(appPool.Close)

	cfg := &config.Config{
		Env:  "test",
		DB:   &config.DBConfig{},
		Auth: &config.AuthConfig{Issuer: "https://openrails.test", MintDisabled: true},
	}
	cp, err := New(ctx, cfg, super)
	require.NoError(t, err)

	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	mA, mB := uuid.New(), uuid.New()
	custA, custB := uuid.New(), uuid.New()
	prodA, prodB := uuid.New(), uuid.New()
	priceA, priceB := uuid.New(), uuid.New()
	payA, payB := uuid.New(), uuid.New()

	exec := func(sql string, args ...any) {
		t.Helper()
		_, err := super.Exec(ctx, sql, args...)
		require.NoError(t, err, sql)
	}
	exec(`INSERT INTO openrails.merchants (id, slug, status) VALUES ($1, $2, 'active'), ($3, $4, 'active')`,
		mA, "psiso-a-"+suffix, mB, "psiso-b-"+suffix)
	exec(`INSERT INTO openrails.customers (id, merchant_id) VALUES ($1, $2), ($3, $4)`, custA, mA, custB, mB)
	exec(`INSERT INTO openrails.products (id, key, display_name, merchant_id) VALUES ($1, $2, 'PS A', $3), ($4, $5, 'PS B', $6)`,
		prodA, "psiso-pa-"+suffix, mA, prodB, "psiso-pb-"+suffix, mB)
	exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, merchant_id, auto_renew) VALUES ($1, $2, 7000000, 'USD', $3, false), ($4, $5, 9000000, 'USD', $6, false)`,
		priceA, prodA, mA, priceB, prodB, mB)

	// Insert completed payments as openrails_app under each merchant's GUC: the
	// enqueue trigger's INSERT must pass the new fail-closed WITH CHECK.
	insertPayment := func(mid, payID, custID, priceID uuid.UUID, amount int64) {
		t.Helper()
		tx, err := appPool.Begin(ctx)
		require.NoError(t, err)
		defer tx.Rollback(ctx) //nolint:errcheck
		_, err = tx.Exec(ctx, `SELECT set_config('app.merchant_id', $1, true)`, mid.String())
		require.NoError(t, err)
		_, err = tx.Exec(ctx, `INSERT INTO openrails.payments
			(id, merchant_id, customer_id, price_id, rail, transaction_id, amount, list_amount, currency, status, money_movement)
			VALUES ($1, $2, $3, $4, 'nmi', $5, $6, $6, 'USD', 'completed', 'rail')`,
			payID, mid, custID, priceID, "psiso-"+suffix+"-"+payID.String()[:8], amount)
		require.NoError(t, err)
		require.NoError(t, tx.Commit(ctx))
	}
	insertPayment(mA, payA, custA, priceA, 7_000_000)
	insertPayment(mB, payB, custB, priceB, 9_000_000)

	idA, idB := merchant.ID(mA), merchant.ID(mB)

	// Each merchant sees exactly its own event.
	listA, err := cp.ListPendingPaymentSettlements(ctx, idA, 10)
	require.NoError(t, err)
	require.Len(t, listA, 1)
	require.Equal(t, payA, listA[0].PaymentID)
	require.Equal(t, idA, listA[0].MerchantID)
	require.EqualValues(t, 7_000_000, listA[0].Amount)

	listB, err := cp.ListPendingPaymentSettlements(ctx, idB, 10)
	require.NoError(t, err)
	require.Len(t, listB, 1)
	require.Equal(t, payB, listB[0].PaymentID)

	// A cannot ack B's event; B's feed is undisturbed.
	err = cp.AcknowledgePaymentSettlement(ctx, idA, listB[0].ID)
	require.ErrorIs(t, err, ErrPaymentSettlementNotFound)
	listB, err = cp.ListPendingPaymentSettlements(ctx, idB, 10)
	require.NoError(t, err)
	require.Len(t, listB, 1, "cross-merchant ack must not deliver B's event")

	// A zero merchant id is refused outright.
	_, err = cp.ListPendingPaymentSettlements(ctx, merchant.ID{}, 10)
	require.Error(t, err)
	require.Error(t, cp.AcknowledgePaymentSettlement(ctx, merchant.ID{}, listB[0].ID))

	// RLS proof on the NOBYPASSRLS app role: under A's GUC only A's rows are
	// visible and B's row cannot be acked; with no GUC the table is fail-closed.
	appTx := func(mid *uuid.UUID, fn func(tx gen.DBTX)) {
		t.Helper()
		tx, err := appPool.Begin(ctx)
		require.NoError(t, err)
		defer tx.Rollback(ctx) //nolint:errcheck
		if mid != nil {
			_, err = tx.Exec(ctx, `SELECT set_config('app.merchant_id', $1, true)`, mid.String())
			require.NoError(t, err)
		}
		fn(tx)
		require.NoError(t, tx.Commit(ctx))
	}
	appTx(&mA, func(tx gen.DBTX) {
		var n int
		require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM openrails.payment_settlement_events WHERE merchant_id = $1`, mB).Scan(&n))
		require.Zero(t, n, "app role under A's GUC must not see B's events")
		require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM openrails.payment_settlement_events`).Scan(&n))
		require.Equal(t, 1, n, "app role under A's GUC sees exactly A's event")
		tag, err := tx.Exec(ctx, `UPDATE openrails.payment_settlement_events SET delivered_at = now() WHERE id = $1`, listB[0].ID)
		require.NoError(t, err)
		require.Zero(t, tag.RowsAffected(), "app role under A's GUC must not ack B's event")
	})
	appTx(nil, func(tx gen.DBTX) {
		var n int
		require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM openrails.payment_settlement_events`).Scan(&n))
		require.Zero(t, n, "no merchant GUC must fail closed")
	})

	// B acks its own event; ack is idempotent; feed drains.
	require.NoError(t, cp.AcknowledgePaymentSettlement(ctx, idB, listB[0].ID))
	require.NoError(t, cp.AcknowledgePaymentSettlement(ctx, idB, listB[0].ID))
	listB, err = cp.ListPendingPaymentSettlements(ctx, idB, 10)
	require.NoError(t, err)
	require.Empty(t, listB)

	// Pruning (#827): only acked rows older than the cutoff go; A's pending
	// event survives, and RLS scopes the delete to B.
	_, err = super.Exec(ctx, `UPDATE openrails.payment_settlement_events SET delivered_at = now() - interval '31 days' WHERE payment_id = $1`, payB)
	require.NoError(t, err)
	appTx(&mB, func(tx gen.DBTX) {
		n, err := gen.New(tx).DeleteDeliveredPaymentSettlementsBefore(ctx, gen.DeleteDeliveredPaymentSettlementsBeforeParams{
			MerchantID: mB,
			Cutoff:     time.Now().Add(-30 * 24 * time.Hour),
		})
		require.NoError(t, err)
		require.EqualValues(t, 1, n)
	})
	listA, err = cp.ListPendingPaymentSettlements(ctx, idA, 10)
	require.NoError(t, err)
	require.Len(t, listA, 1, "pruning acked rows must not touch pending events")
}
