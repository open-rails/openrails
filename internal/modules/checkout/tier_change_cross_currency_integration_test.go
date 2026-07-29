//go:build integration

package checkout

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/merchant"
)

// #820 (real DB): a tier group whose products are priced in different
// currencies is expressible (pkg/catalog/load.go accepts any 3-letter ISO code
// per price), so TierChange must refuse the FX crossing instead of subtracting
// EUR micros from USD micros.
//
// Both directions are exercised because they fail in opposite directions:
// €20 -> $25 UNDERCHARGES ($6.33 taken for ~$20.16 of unused value) and
// $25 -> €20 OVERCHARGES.
func TestTierChangeRefusesCrossCurrencyUpgrade(t *testing.T) {
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

	tierGroup := "fx-tier-" + sfx
	seed := func(key string, rank int, amount int64, currency string) (uuid.UUID, uuid.UUID) {
		prod, price := uuid.New(), uuid.New()
		exec(`INSERT INTO openrails.products (id,key,display_name,tier_group,tier_rank,entitlements_spec,merchant_id)
		      VALUES ($1,$2,$2,$3,$4,'{}'::jsonb,$5)`, prod, "fx-"+key+"-"+sfx, tierGroup, rank, merchantID)
		exec(`INSERT INTO openrails.prices (id,product_id,amount,currency,access_duration_hours,auto_renew,merchant_id)
		      VALUES ($1,$2,$3,$4,720,true,$5)`, price, prod, amount, currency, merchantID)
		return prod, price
	}

	// Same tier group, different currencies: €20.00/mo basic, $25.00/mo pro.
	basicProd, basicPrice := seed("basic", 1, 20_000_000, "EUR")
	proProd, proPrice := seed("pro", 2, 25_000_000, "USD")

	// Subscriber sits 2 days into a 30-day cycle on the EUR plan.
	subID := uuid.New()
	exec(`INSERT INTO openrails.subscriptions (id,merchant_id,customer_id,product_id,price_id,status,rail,rail_subscription_id,started_at,current_period_starts_at,current_period_ends_at)
	      VALUES ($1,$2,$3,$4,$5,'active','nmi',$6,$7,$7,$8)`,
		subID, merchantID, cust, basicProd, basicPrice, "fx-sub-"+sfx,
		now.Add(-2*24*time.Hour), now.Add(28*24*time.Hour))

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.subscriptions WHERE id=$1`, subID)
		for _, p := range []uuid.UUID{basicPrice, proPrice} {
			_, _ = pool.Exec(ctx, `DELETE FROM openrails.prices WHERE id=$1`, p)
		}
		for _, p := range []uuid.UUID{basicProd, proProd} {
			_, _ = pool.Exec(ctx, `DELETE FROM openrails.products WHERE id=$1`, p)
		}
	})

	priceSvc := catalog.NewPriceService(dbi)
	productSvc := catalog.NewProductService(dbi)
	subSvc := subscriptions.NewSubscriptionService(dbi, priceSvc, productSvc, nil)
	svc := &CheckoutService{PriceService: priceSvc, ProductService: productSvc, SubscriptionService: subSvc}
	user := &UserIdentity{ID: userID}

	// EUR -> USD (would undercharge).
	req := &TierChangeRequest{PriceID: proPrice.String(), SubscriptionID: subID}
	_, err := svc.TierChange(ctx, req, user)
	require.ErrorIs(t, err, subscriptions.ErrRepriceCrossCurrency)
	_, err = svc.TierChangePreview(ctx, req, user)
	require.ErrorIs(t, err, subscriptions.ErrRepriceCrossCurrency)

	// USD -> EUR (would overcharge): flip the subscription onto the USD plan.
	exec(`UPDATE openrails.subscriptions SET product_id=$2, price_id=$3 WHERE id=$1`, subID, proProd, proPrice)
	req = &TierChangeRequest{PriceID: basicPrice.String(), SubscriptionID: subID}
	_, err = svc.TierChange(ctx, req, user)
	require.ErrorIs(t, err, subscriptions.ErrRepriceCrossCurrency)
	_, err = svc.TierChangePreview(ctx, req, user)
	require.ErrorIs(t, err, subscriptions.ErrRepriceCrossCurrency)
}
