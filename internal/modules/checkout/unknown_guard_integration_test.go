//go:build integration

package checkout

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/merchant"
)

// #691 checkout guard (real DB): a customer whose subscription is parked
// `unknown` cannot silently re-purchase the same product or tier-group — the
// provider-side sub may still be alive and billing (double-billing hole).
// A different product is allowed; once the sub resolves terminal (cancelled),
// checkout is allowed again.
func TestUnknownSubscriptionCheckoutGuard(t *testing.T) {
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	pool := dbi.Pool()
	dbtest.EnsureTestMerchant(context.Background(), t, pool)
	merchantID := dbtest.TestMerchantID.UUID()
	sfx := uuid.NewString()[:8]

	now := time.Now().UTC().Truncate(time.Second)
	userID := uuid.NewString()
	cust := dbtest.EnsureCustomerIDPgx(ctx, t, pool, userID)

	exec := func(sql string, args ...any) {
		_, err := pool.Exec(ctx, sql, args...)
		require.NoError(t, err)
	}

	tierGroup := "ug-tier-" + sfx
	seedRecurringProduct := func(key string, group *string, rank int) (uuid.UUID, uuid.UUID) {
		prod, price := uuid.New(), uuid.New()
		exec(`INSERT INTO openrails.products (id,key,display_name,tier_group,tier_rank,entitlements_spec,merchant_id)
		      VALUES ($1,$2,$2,$3,$4,'{}'::jsonb,$5)`, prod, "ug-"+key+"-"+sfx, group, rank, merchantID)
		exec(`INSERT INTO openrails.prices (id,product_id,amount,currency,access_duration_hours,auto_renew,merchant_id)
		      VALUES ($1,$2,5000000,'USD',720,true,$3)`, price, prod, merchantID)
		return prod, price
	}

	prodA, priceA := seedRecurringProduct("a", &tierGroup, 1) // the unknown sub's product
	prodB, priceB := seedRecurringProduct("b", &tierGroup, 2) // same tier-group, other tier
	prodC, priceC := seedRecurringProduct("c", nil, 0)        // unrelated product, no group
	unknownSub := uuid.New()
	exec(`INSERT INTO openrails.subscriptions (id,merchant_id,customer_id,product_id,price_id,status,rail,rail_subscription_id,started_at,current_period_starts_at,current_period_ends_at)
	      VALUES ($1,$2,$3,$4,$5,'unknown','nmi',$6,$7,$7,$8)`,
		unknownSub, merchantID, cust, prodA, priceA, "ug-sub-"+sfx, now.Add(-40*24*time.Hour), now.Add(-10*24*time.Hour))

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.subscriptions WHERE id=$1`, unknownSub)
		for _, p := range []uuid.UUID{priceA, priceB, priceC} {
			_, _ = pool.Exec(ctx, `DELETE FROM openrails.prices WHERE id=$1`, p)
		}
		for _, p := range []uuid.UUID{prodA, prodB, prodC} {
			_, _ = pool.Exec(ctx, `DELETE FROM openrails.products WHERE id=$1`, p)
		}
	})

	priceSvc := catalog.NewPriceService(dbi)
	productSvc := catalog.NewProductService(dbi)
	entSvc := entitlements.NewEntitlementService(dbi, nil)
	paymentSvc := payments.NewPaymentService(dbi, nil)
	subSvc := subscriptions.NewSubscriptionService(dbi, priceSvc, productSvc, nil, nil, nil, nil)
	purchase := NewCheckoutPurchaseService(priceSvc, productSvc, paymentSvc, entSvc, subSvc, nil)

	load := func(priceID uuid.UUID) (*models.Price, *models.Product) {
		price, err := priceSvc.GetByID(ctx, priceID)
		require.NoError(t, err)
		product, err := productSvc.GetByID(ctx, price.ProductID)
		require.NoError(t, err)
		return price, product
	}

	// Same product: rejected with the machine-readable verification code.
	priceAm, productAm := load(priceA)
	conflict, err := purchase.CheckSubscriptionConflict(ctx, userID, priceAm, productAm)
	require.NoError(t, err)
	require.True(t, conflict.Blocked, "unknown sub for the same product must block checkout")
	require.Equal(t, ConflictCodeMembershipPendingVerification, conflict.Code)
	require.NotNil(t, conflict.Existing)
	require.Equal(t, unknownSub, conflict.Existing.ID)

	// Same tier-group, different tier: rejected too (same provider sub would
	// keep billing next to the new one).
	priceBm, productBm := load(priceB)
	conflict, err = purchase.CheckSubscriptionConflict(ctx, userID, priceBm, productBm)
	require.NoError(t, err)
	require.True(t, conflict.Blocked, "unknown sub in the tier-group must block checkout")
	require.Equal(t, ConflictCodeMembershipPendingVerification, conflict.Code)

	// A different product outside the tier-group is allowed.
	priceCm, productCm := load(priceC)
	conflict, err = purchase.CheckSubscriptionConflict(ctx, userID, priceCm, productCm)
	require.NoError(t, err)
	require.False(t, conflict.Blocked, "unrelated product must not be blocked")

	// The full Checkout() entry point (standalone sessions + embedded /checkout
	// both funnel here) rejects with the code before any rail work.
	checkout := &CheckoutService{
		SubscriptionService: subSvc,
		ProductService:      productSvc,
		PriceService:        priceSvc,
		PaymentService:      paymentSvc,
		EntitlementService:  entSvc,
		PurchaseService:     purchase,
	}
	resp, err := checkout.Checkout(ctx, &CheckoutRequest{PriceID: priceA.String(), Rail: "nmi"}, &UserIdentity{ID: userID})
	require.NoError(t, err)
	require.Equal(t, "blocked", resp.Status)
	require.Equal(t, ConflictCodeMembershipPendingVerification, resp.Code)

	// The sub resolves to cancelled (provider-confirmed dead): checkout allowed.
	exec(`UPDATE openrails.subscriptions SET status='cancelled', cancelled_at=$2, cancel_type='expired', ended_at=$2, updated_at=$2 WHERE id=$1`, unknownSub, now)
	conflict, err = purchase.CheckSubscriptionConflict(ctx, userID, priceAm, productAm)
	require.NoError(t, err)
	require.False(t, conflict.Blocked, "a terminally-resolved sub frees the customer to subscribe again")
}
