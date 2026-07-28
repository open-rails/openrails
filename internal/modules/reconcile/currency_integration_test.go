//go:build integration

package reconcile

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

func TestMain(m *testing.M) { dbtest.RunMain(m) }

// #830: a Stripe charge that carries no currency must NOT mint a payments row.
// The old code defaulted to a lowercase, unregistered "usd" and persisted it.
func TestEnsureChargePayment_NoCurrencyWritesNoRow(t *testing.T) {
	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t))
	pool := dbi.Pool()
	dbtest.EnsureTestMerchant(ctx, t, pool)
	merchantID := dbtest.TestMerchantID.UUID()

	suffix := uuid.NewString()[:8]
	customerID := dbtest.EnsureCustomerIDPgx(ctx, t, pool, uuid.NewString())
	productID, priceID, subID := uuid.New(), uuid.New(), uuid.New()

	exec := func(sql string, args ...any) {
		t.Helper()
		_, err := pool.Exec(ctx, sql, args...)
		require.NoError(t, err)
	}
	exec(`INSERT INTO openrails.products (id, key, display_name, merchant_id) VALUES ($1, $2, $2, $3)`,
		productID, "cur-prod-"+suffix, merchantID)
	exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, access_duration_hours, auto_renew, merchant_id)
	      VALUES ($1, $2, 999, 'USD', 720, true, $3)`, priceID, productID, merchantID)
	exec(`INSERT INTO openrails.subscriptions
	        (id, price_id, product_id, status, rail, rail_subscription_id, customer_id, merchant_id)
	      VALUES ($1, $2, $3, 'active', 'stripe', $4, $5, $6)`,
		subID, priceID, productID, "sub-"+suffix, customerID, merchantID)
	t.Cleanup(func() {
		bg := context.Background()
		for _, stmt := range []string{
			`DELETE FROM openrails.payments WHERE merchant_id = $1 AND price_id = '` + priceID.String() + `'`,
			`DELETE FROM openrails.subscriptions WHERE id = '` + subID.String() + `'`,
			`DELETE FROM openrails.prices WHERE id = '` + priceID.String() + `'`,
			`DELETE FROM openrails.products WHERE id = '` + productID.String() + `'`,
		} {
			_, _ = pool.Exec(bg, stmt, merchantID)
		}
	})

	paySvc := payments.NewPaymentService(dbi)
	sub := &models.Subscription{ID: subID, PriceID: priceID, MerchantID: merchantID}

	countRows := func() int {
		t.Helper()
		var n int
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT count(*) FROM openrails.payments WHERE merchant_id = $1 AND price_id = $2`,
			merchantID, priceID).Scan(&n))
		return n
	}

	// No currency on the provider payload => no row, no error, nothing invented.
	for _, currency := range []string{"", "   "} {
		txnID := "ch_nocur_" + suffix + currency
		created, err := ensureChargePayment(ctx, paySvc, nil, customerID.String(), sub,
			subscriptions.StripeRemoteCharge{ID: txnID, Amount: 999, Currency: currency, Paid: true, Status: "succeeded"},
			txnID, nil)
		require.NoError(t, err)
		require.False(t, created, "a charge with no currency must not mint a payments row")
	}
	require.Zero(t, countRows())

	// Control: the same charge WITH a currency does create the row, so the skip
	// above is attributable to the missing currency and not to the fixture.
	txnID := "ch_cur_" + suffix
	created, err := ensureChargePayment(ctx, paySvc, nil, customerID.String(), sub,
		subscriptions.StripeRemoteCharge{ID: txnID, Amount: 999, Currency: "USD", Paid: true, Status: "succeeded"},
		txnID, nil)
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, 1, countRows())

	var stored string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT currency FROM openrails.payments WHERE merchant_id = $1 AND transaction_id = $2`,
		merchantID, txnID).Scan(&stored))
	require.Equal(t, "USD", stored)
}
