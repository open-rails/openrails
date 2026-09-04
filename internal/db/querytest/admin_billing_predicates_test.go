//go:build integration

package querytest

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Use a bypassing pool with NO merchant GUC: both the negative isolation and
// positive background-worker path must depend on the explicit context UUID.
func TestAdminBillingPredicatesWithoutRLS(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.SharedSuperuserPGXPool(t)
	var bypass bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&bypass))
	require.True(t, bypass)
	var pinnedMerchant *string
	require.NoError(t, pool.QueryRow(ctx, `SELECT NULLIF(current_setting('app.merchant_id', true), '')`).Scan(&pinnedMerchant))
	require.Nil(t, pinnedMerchant, "the positive cases must not depend on a merchant GUC")
	database, err := db.NewWithPGXPool(pool, config.DefaultSchema)
	require.NoError(t, err)
	now := time.Now().UTC().Add(-time.Hour)
	type fixture struct{ merchant, customer, sub, payment, method, reprice, batch uuid.UUID }
	seed := func() fixture {
		customerID := uuid.New()
		f := fixture{merchant: uuid.New(), customer: customerID, sub: uuid.New(), payment: uuid.New(), method: uuid.New()}
		exec := func(query string, args ...any) {
			t.Helper()
			_, e := pool.Exec(ctx, query, args...)
			require.NoError(t, e)
		}
		exec(`INSERT INTO openrails.merchants (id,slug,status) VALUES ($1,$2,'active')`, f.merchant, "sec18-"+f.merchant.String())
		dbtest.EnsureCustomerIDPgxFor(ctx, t, pool, f.merchant, customerID.String())
		product, low, high := uuid.New(), uuid.New(), uuid.New()
		exec(`INSERT INTO openrails.products(id,merchant_id,key,display_name) VALUES($1,$2,'scope-product','Scope product')`, product, f.merchant)
		for i, id := range []uuid.UUID{low, high} {
			exec(`INSERT INTO openrails.prices(id,merchant_id,product_id,key,amount,currency,access_duration_hours,auto_renew) VALUES($1,$2,$3,$4,$5,'USD',720,true)`, id, f.merchant, product, id.String(), int64(i+1)*1000000)
		}
		psp := dbtest.EnsureTestPSP(ctx, t, pool, f.merchant, "nmi")
		exec(`INSERT INTO openrails.subscriptions(id,merchant_id,customer_id,product_id,price_id,status,rail,psp_id,started_at,current_period_starts_at,current_period_ends_at) VALUES($1,$2,$3,$4,$5,'active','nmi',$6,$7,$7,$8)`, f.sub, f.merchant, customerID, product, low, psp, now, now.Add(30*24*time.Hour))
		exec(`INSERT INTO openrails.payments(id,merchant_id,customer_id,price_id,subscription_id,rail,psp_id,transaction_id,amount,list_amount,currency,status,money_movement,purchased_at) VALUES($1,$2,$3,$4,$5,'nmi',$6,$7,1000000,1000000,'USD','completed','rail',$8)`, f.payment, f.merchant, customerID, low, f.sub, psp, f.payment.String(), now)
		exec(`INSERT INTO openrails.payment_methods(id,merchant_id,customer_id,rail,psp_id,rail_customer_ref,rail_method_ref,initial_transaction_id) VALUES($1,$2,$3,'nmi',$4,$5::text,$5::text,$5::text)`, f.method, f.merchant, customerID, psp, f.method.String())
		exec(`INSERT INTO openrails.entitlements(id,merchant_id,customer_id,entitlement,source_type,source_id,start_at,end_at) VALUES($1,$2,$3,'scope-access','subscription',$4,$5,$6)`, uuid.New(), f.merchant, customerID, f.sub, now, now.Add(24*time.Hour))
		mctx := merchant.WithID(ctx, merchant.ID(f.merchant))
		repo := subscriptions.NewRepriceRepo(database)
		key := "scope-price"
		batch, e := repo.CreateBatch(mctx, &key, high, now.Add(24*time.Hour), 1, 1, 0)
		require.NoError(t, e)
		f.batch = batch.ID
		repr, e := repo.CreateSubscriptionReprice(mctx, f.sub, low, high, now.Add(24*time.Hour), &f.batch, false)
		require.NoError(t, e)
		f.reprice = repr.ID
		return f
	}
	a, b := seed(), seed()
	mctx := merchant.WithID(ctx, merchant.ID(a.merchant))
	repo := subscriptions.NewRepriceRepo(database)
	t.Run("reprice IDs and lists", func(t *testing.T) {
		own, e := repo.GetByID(mctx, a.reprice)
		require.NoError(t, e)
		require.Equal(t, a.reprice, own.ID)
		_, e = repo.GetByID(mctx, b.reprice)
		require.ErrorIs(t, e, pgx.ErrNoRows)
		_, e = repo.GetBatchByID(mctx, b.batch)
		require.ErrorIs(t, e, pgx.ErrNoRows)
		rows, e := repo.List(mctx, subscriptions.SubscriptionRepriceFilter{}, 100, 0)
		require.NoError(t, e)
		require.Len(t, rows, 1)
		require.Equal(t, a.reprice, rows[0].ID)
		batches, e := repo.ListBatchesByPriceKey(mctx, "scope-price", 100, 0)
		require.NoError(t, e)
		require.Len(t, batches, 1)
		require.Equal(t, a.batch, batches[0].ID)
	})
	t.Run("reprice cancellation", func(t *testing.T) {
		require.ErrorIs(t, repo.Cancel(mctx, b.reprice), subscriptions.ErrRepriceNotScheduled)
		var status string
		require.NoError(t, pool.QueryRow(ctx, `SELECT status FROM openrails.subscription_reprices WHERE id=$1`, b.reprice).Scan(&status))
		require.Equal(t, "scheduled", status)
		require.NoError(t, repo.Cancel(mctx, a.reprice))
	})
	t.Run("customer subscriptions", func(t *testing.T) {
		rows, e := subscriptions.NewSubscriptionRepo(database).GetActiveSubscriptionsByUserID(mctx, a.customer.String())
		require.NoError(t, e)
		require.Len(t, rows, 1)
		require.Equal(t, a.sub, rows[0].ID)
		foreign, e := subscriptions.NewSubscriptionRepo(database).GetActiveSubscriptionsByUserID(mctx, b.customer.String())
		require.NoError(t, e)
		require.Empty(t, foreign)
	})
	t.Run("customer entitlements", func(t *testing.T) {
		rows, e := entitlements.NewEntitlementService(database, nil).ListActiveRecords(mctx, a.customer.String(), time.Now())
		require.NoError(t, e)
		require.Len(t, rows, 1)
		require.Equal(t, a.merchant, rows[0].MerchantID)
		foreign, e := entitlements.NewEntitlementService(database, nil).ListActiveRecords(mctx, b.customer.String(), time.Now())
		require.NoError(t, e)
		require.Empty(t, foreign)
	})
	t.Run("customer payments", func(t *testing.T) {
		pr := payments.NewPaymentRepo(database)
		rows, e := pr.GetByUserID(mctx, a.customer.String())
		require.NoError(t, e)
		require.Len(t, rows, 1)
		require.Equal(t, a.payment, rows[0].ID)
		foreign, e := pr.GetByUserID(mctx, b.customer.String())
		require.NoError(t, e)
		require.Empty(t, foreign)
		paged, total, e := pr.GetPaginatedByUserID(mctx, a.customer.String(), 1, 10)
		require.NoError(t, e)
		require.Equal(t, 1, total)
		require.Len(t, paged, 1)
		require.Equal(t, a.payment, paged[0].ID)
		foreign, foreignTotal, e := pr.GetPaginatedByUserID(mctx, b.customer.String(), 1, 10)
		require.NoError(t, e)
		require.Empty(t, foreign)
		require.Zero(t, foreignTotal)
	})
	t.Run("customer payment methods", func(t *testing.T) {
		pr := paymentmethods.NewPaymentMethodRepo(database)
		rows, e := pr.GetByUserID(mctx, a.customer.String())
		require.NoError(t, e)
		require.Len(t, rows, 1)
		require.Equal(t, a.method, rows[0].ID)
		foreign, e := pr.GetByUserID(mctx, b.customer.String())
		require.NoError(t, e)
		require.Empty(t, foreign)
		paged, total, e := pr.ListByUserID(mctx, a.customer.String(), 10, 0)
		require.NoError(t, e)
		require.EqualValues(t, 1, total)
		require.Len(t, paged, 1)
		require.Equal(t, a.method, paged[0].ID)
		foreign, foreignTotal, e := pr.ListByUserID(mctx, b.customer.String(), 10, 0)
		require.NoError(t, e)
		require.Empty(t, foreign)
		require.Zero(t, foreignTotal)
	})
	t.Run("missing merchant fails closed", func(t *testing.T) {
		_, e := repo.GetByID(ctx, a.reprice)
		require.ErrorIs(t, e, merchant.ErrNoMerchant)
		require.ErrorIs(t, repo.Cancel(ctx, b.reprice), merchant.ErrNoMerchant)
		_, e = subscriptions.NewSubscriptionRepo(database).GetActiveSubscriptionsByUserID(ctx, a.customer.String())
		require.ErrorIs(t, e, merchant.ErrNoMerchant)
		_, e = entitlements.NewEntitlementService(database, nil).ListActiveRecords(ctx, a.customer.String(), time.Now())
		require.ErrorIs(t, e, merchant.ErrNoMerchant)
		_, e = payments.NewPaymentRepo(database).GetByUserID(ctx, a.customer.String())
		require.ErrorIs(t, e, merchant.ErrNoMerchant)
		_, e = paymentmethods.NewPaymentMethodRepo(database).GetByUserID(ctx, a.customer.String())
		require.ErrorIs(t, e, merchant.ErrNoMerchant)
	})
}
