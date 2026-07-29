//go:build integration

package controlplane

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
)

// or#827: the host settlement feed publishes on a POSITIVE marker, not on what
// a transaction_id fails to look like.
//
// 0005 enqueued every completed positive payment whose transaction_id was not
// one of three known synthetic prefixes. That denylist is open at the top: #796
// added three more synthetic shapes ('nmi_sale_declined:', 'nmi_sub_declined:',
// 'vaulted_card_sale_declined:') and #733 a fourth ('renewal_declined:'), none
// of them in the trigger. Only their status kept them out. The first synthetic
// shape that is COMPLETED and positive would have told a host that money
// arrived for a payment that never moved a cent.
//
// Proved against a pre-0026 schema before the fix: the first case below
// published 1 settlement event.
func TestPaymentSettlementFeedRequiresDeclaredMoneyMovement(t *testing.T) {
	ctx := context.Background()
	appDSN := dbtest.SharedPostgresDSN(t)
	super := dbtest.SharedSuperuserPGXPool(t)

	appPool, err := pgxpool.New(ctx, appDSN)
	require.NoError(t, err)
	t.Cleanup(appPool.Close)

	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	mID, custID, prodID, priceID := uuid.New(), uuid.New(), uuid.New(), uuid.New()

	exec := func(sql string, args ...any) {
		t.Helper()
		_, err := super.Exec(ctx, sql, args...)
		require.NoError(t, err, sql)
	}
	exec(`INSERT INTO openrails.merchants (id, slug, status) VALUES ($1, $2, 'active')`, mID, "psmark-"+suffix)
	exec(`INSERT INTO openrails.customers (id, merchant_id) VALUES ($1, $2)`, custID, mID)
	exec(`INSERT INTO openrails.products (id, key, display_name, merchant_id) VALUES ($1, $2, 'PS marker', $3)`, prodID, "psmark-p-"+suffix, mID)
	exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, merchant_id, auto_renew) VALUES ($1, $2, 7000000, 'USD', $3, false)`, priceID, prodID, mID)

	// Everything runs as openrails_app under the merchant GUC — the same
	// NOBYPASSRLS path production writes on.
	inMerchantTx := func(fn func(tx gen.DBTX)) {
		t.Helper()
		tx, err := appPool.Begin(ctx)
		require.NoError(t, err)
		defer tx.Rollback(ctx) //nolint:errcheck
		_, err = tx.Exec(ctx, `SELECT set_config('app.merchant_id', $1, true)`, mID.String())
		require.NoError(t, err)
		fn(tx)
		require.NoError(t, tx.Commit(ctx))
	}
	insertPayment := func(payID uuid.UUID, txnID, status, movement string, amount int64) {
		t.Helper()
		inMerchantTx(func(tx gen.DBTX) {
			_, err := tx.Exec(ctx, `INSERT INTO openrails.payments
				(id, merchant_id, customer_id, price_id, rail, transaction_id, amount, list_amount, currency, status, money_movement)
				VALUES ($1, $2, $3, $4, 'nmi', $5, $6, $6, 'USD', $7, $8)`,
				payID, mID, custID, priceID, txnID, amount, status, movement)
			require.NoError(t, err)
		})
	}
	published := func(payID uuid.UUID) int {
		t.Helper()
		var n int
		require.NoError(t, super.QueryRow(ctx,
			`SELECT count(*) FROM openrails.payment_settlement_events WHERE payment_id = $1`, payID).Scan(&n))
		return n
	}

	// 1. THE LEAK. A synthetic completed positive payment whose prefix nobody
	//    added to the denylist. Undeclared money movement => nothing published.
	leak := uuid.New()
	insertPayment(leak, "wallet_credit_anchor:"+suffix, "completed", "none", 7_000_000)
	require.Zero(t, published(leak),
		"a synthetic completed payment must not reach the host feed (0005's denylist published it)")

	// 2. A real charge says so, and is published.
	real1 := uuid.New()
	insertPayment(real1, "rail-txn-"+suffix, "completed", "rail", 7_000_000)
	require.Equal(t, 1, published(real1), "a declared rail charge is the feed's whole content")

	// 3. The NMI subscription attempt anchor: a pending row whose real charge is
	//    recorded on a DIFFERENT payment. Resolving it in place must reach a
	//    terminal status without publishing a second settlement.
	anchor := uuid.New()
	insertPayment(anchor, "nmi_sub_attempt:"+suffix, "pending", "none", 7_000_000)
	inMerchantTx(func(tx gen.DBTX) {
		n, err := gen.New(tx).CompleteProviderAttemptInPlace(ctx, gen.CompleteProviderAttemptInPlaceParams{ID: anchor})
		require.NoError(t, err)
		require.EqualValues(t, 1, n)
	})
	require.Zero(t, published(anchor), "an attempt anchor completed in place publishes nothing")

	// 4. The attempt row that BECOMES the charge takes the rail's own
	//    transaction id, declares movement, and publishes exactly once.
	attempt := uuid.New()
	insertPayment(attempt, "vaulted_card_sale_attempt:"+suffix, "pending", "none", 7_000_000)
	require.Zero(t, published(attempt))
	inMerchantTx(func(tx gen.DBTX) {
		n, err := gen.New(tx).CompleteProviderAttempt(ctx, gen.CompleteProviderAttemptParams{
			ID: attempt, TransactionID: "rail-txn-settled-" + suffix,
		})
		require.NoError(t, err)
		require.EqualValues(t, 1, n)
	})
	require.Equal(t, 1, published(attempt), "the attempt row that took the rail's transaction id settles")

	// 5. A decline and a reversal never publish, whatever they declare.
	declined, reversal := uuid.New(), uuid.New()
	insertPayment(declined, "nmi_sale_declined:"+suffix, "failed", "none", 7_000_000)
	insertPayment(reversal, "rail-refund-"+suffix, "completed", "rail", -7_000_000)
	require.Zero(t, published(declined))
	require.Zero(t, published(reversal))

	// 6. The vocabulary is pinned in the schema: an undeclared or invented
	//    value cannot be stored at all.
	badTx, err := appPool.Begin(ctx)
	require.NoError(t, err)
	defer badTx.Rollback(ctx) //nolint:errcheck
	_, err = badTx.Exec(ctx, `SELECT set_config('app.merchant_id', $1, true)`, mID.String())
	require.NoError(t, err)
	_, err = badTx.Exec(ctx, `INSERT INTO openrails.payments
		(id, merchant_id, customer_id, price_id, rail, transaction_id, amount, list_amount, currency, status, money_movement)
		VALUES ($1, $2, $3, $4, 'nmi', $5, 100, 100, 'USD', 'completed', 'maybe')`,
		uuid.New(), mID, custID, priceID, "bogus-"+suffix)
	require.Error(t, err, "money_movement is CHECK-constrained to rail|none")
}
