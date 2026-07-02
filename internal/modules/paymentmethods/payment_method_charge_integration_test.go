//go:build integration

package paymentmethods

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
)

// #589: LatestChargeByMethodIDs derives per-method last-charge health TRANSITIVELY
// via subscriptions -> payments (payments carry no direct payment_method_id yet).
// This proves the derivation against a real DB: the most recent payment (by
// purchased_at) wins, its status maps to the outcome, and methods with no charge
// history are absent from the map. No mocks — real Postgres via testcontainers.
func TestLatestChargeByMethodIDs(t *testing.T) {
	ctx := dbtest.WithTestMerchant(context.Background())
	pool := dbtest.SharedPGXPool(t)
	merchantID := dbtest.TestMerchantID.UUID()

	dbi, err := db.NewWithPGXPool(pool, "")
	require.NoError(t, err)

	userID := uuid.New().String()
	customerID := dbtest.EnsureCustomerIDPgx(ctx, t, pool, userID)

	pmRepo := NewPaymentMethodRepo(dbi)
	now := time.Now().UTC().Truncate(time.Second)

	// A method with charge history, and a second method with none.
	charged := &models.PaymentMethod{
		ID: uuid.New(), CustomerID: customerID, Rail: models.RailNMI,
		RailCustomerRef: "vault-charged-" + uuid.NewString(), CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, pmRepo.Create(ctx, charged))
	uncharged := &models.PaymentMethod{
		ID: uuid.New(), CustomerID: customerID, Rail: models.RailNMI,
		RailCustomerRef: "vault-uncharged-" + uuid.NewString(), CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, pmRepo.Create(ctx, uncharged))

	productID := uuid.New()
	priceID := uuid.New()
	subID := uuid.New()
	_, err = pool.Exec(ctx,
		`INSERT INTO openrails.products (id, merchant_id, key, display_name) VALUES ($1,$2,$3,$3)`,
		productID, merchantID, "prod-"+uuid.NewString()[:8])
	require.NoError(t, err)
	_, err = pool.Exec(ctx,
		`INSERT INTO openrails.prices (id, product_id, merchant_id, amount, currency) VALUES ($1,$2,$3,$4,$5)`,
		priceID, productID, merchantID, 1000, "USD")
	require.NoError(t, err)
	_, err = pool.Exec(ctx,
		`INSERT INTO openrails.subscriptions (id, product_id, price_id, payment_method_id, rail, merchant_id, customer_id)
		 VALUES ($1,$2,$3,$4,'mobius',$5,$6)`,
		subID, productID, priceID, charged.ID, merchantID, customerID)
	require.NoError(t, err)

	// Older successful charge, then a newer failed charge: the newer one must win.
	insertPayment := func(txID string, status string, purchasedAt time.Time) {
		_, e := pool.Exec(ctx,
			`INSERT INTO openrails.payments
			   (id, price_id, rail, transaction_id, amount, list_amount, currency, status, subscription_id, merchant_id, customer_id, purchased_at)
			 VALUES ($1,$2,'mobius',$3,$4,$4,'USD',$5,$6,$7,$8,$9)`,
			uuid.New(), priceID, txID, 1000, status, subID, merchantID, customerID, purchasedAt)
		require.NoError(t, e)
	}
	insertPayment("tx-old-"+uuid.NewString()[:8], "completed", now.Add(-48*time.Hour))
	insertPayment("tx-new-"+uuid.NewString()[:8], "failed", now.Add(-24*time.Hour))

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.payments WHERE subscription_id = $1`, subID)
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.subscriptions WHERE id = $1`, subID)
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.payment_methods WHERE id = ANY($1)`, []uuid.UUID{charged.ID, uncharged.ID})
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.prices WHERE id = $1`, priceID)
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.products WHERE id = $1`, productID)
	})

	got, err := pmRepo.LatestChargeByMethodIDs(ctx, []uuid.UUID{charged.ID, uncharged.ID})
	require.NoError(t, err)

	charge, ok := got[charged.ID]
	require.True(t, ok, "charged method must have derived charge history")
	require.Equal(t, "failed", charge.Status, "the most recent charge (failed) wins")
	require.WithinDuration(t, now.Add(-24*time.Hour), charge.LastChargedAt, time.Second)

	_, ok = got[uncharged.ID]
	require.False(t, ok, "method with no charge history must be absent from the map")
}

// #588: the two-slot model lets one customer vault (rail_customer_ref) hold MORE
// THAN ONE stored instrument (rail_method_ref) — the NMI multi-billing case the old
// vault_id-only uniqueness forbade. Same (customer_ref, method_ref) pair still dedupes.
func TestMultiInstrumentPerVaultUniqueness(t *testing.T) {
	ctx := dbtest.WithTestMerchant(context.Background())
	pool := dbtest.SharedPGXPool(t)
	dbi, err := db.NewWithPGXPool(pool, "")
	require.NoError(t, err)

	customerID := dbtest.EnsureCustomerIDPgx(ctx, t, pool, uuid.New().String())
	now := time.Now().UTC().Truncate(time.Second)
	r := NewPaymentMethodRepo(dbi)

	vault := "cv-" + uuid.NewString()
	mk := func(methodRef string) *models.PaymentMethod {
		return &models.PaymentMethod{
			ID: uuid.New(), CustomerID: customerID, Rail: models.RailNMI,
			RailCustomerRef: vault, RailMethodRef: methodRef, CreatedAt: now, UpdatedAt: now,
		}
	}
	first := mk("bill-1")
	second := mk("bill-2")
	dup := mk("bill-1")
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.payment_methods WHERE id = ANY($1)`,
			[]uuid.UUID{first.ID, second.ID, dup.ID})
	})

	require.NoError(t, r.Create(ctx, first), "first instrument on the vault")
	require.NoError(t, r.Create(ctx, second), "second instrument on the SAME vault must be allowed")
	require.Error(t, r.Create(ctx, dup), "an exact (vault, instrument) duplicate must still be rejected")
}
