//go:build integration

package service

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/catalog"
)

func newPriceKeyTestService(t *testing.T) (*Service, context.Context, uuid.UUID) {
	t.Helper()
	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	dbtest.EnsureTestMerchant(ctx, t, dbi.Pool())
	svc := &Service{rt: &app.Runtime{
		DB:             dbi,
		ProductService: catalog.NewProductService(dbi),
		PriceService:   catalog.NewPriceService(dbi),
	}}
	return svc, ctx, dbtest.TestMerchantID.UUID()
}

func seedTestProduct(t *testing.T, ctx context.Context, svc *Service, key string) *CatalogProduct {
	t.Helper()
	p, err := svc.CreateProduct(ctx, CreateProductRequest{Key: key, DisplayName: key})
	require.NoError(t, err)
	return p
}

// TestCreatePrice_AmountEditSameKey_VersionBumpAndGrandfather is #774's core
// invariant: editing the amount under the SAME key creates a new substance
// row, re-points the key to it, and archives the displaced row — while an
// existing subscription pinned to the archived row is untouched (grandfather).
func TestCreatePrice_AmountEditSameKey_VersionBumpAndGrandfather(t *testing.T) {
	svc, ctx, merchantID := newPriceKeyTestService(t)
	pool := svc.rt.DB.Pool()

	product := seedTestProduct(t, ctx, svc, "premium-"+uuid.NewString()[:8])
	key := product.Key + "-monthly"

	original, err := svc.CreatePrice(ctx, CreatePriceRequest{
		ProductID: product.ID, Key: key, UnitAmount: 1000000, Currency: "USD",
		AccessDurationHours: intPtrPK(720), AutoRenew: true,
	})
	require.NoError(t, err)
	require.Equal(t, key, original.Key)
	require.False(t, original.Archived)

	// Pin a subscription to the original ($10) row — the grandfather target.
	customerID := seedCustomerPK(t, ctx, pool, merchantID)
	_, err = pool.Exec(ctx, `
		INSERT INTO openrails.subscriptions (merchant_id, customer_id, product_id, price_id, status, rail, rail_subscription_id)
		VALUES ($1, $2, $3, $4, 'active', 'nmi', $5)`,
		merchantID, customerID, product.ID, original.ID, "grandfather-sub-"+uuid.NewString())
	require.NoError(t, err)

	// Edit the amount under the SAME key: $10 -> $12.
	bumped, err := svc.CreatePrice(ctx, CreatePriceRequest{
		ProductID: product.ID, Key: key, UnitAmount: 1200000, Currency: "USD",
		AccessDurationHours: intPtrPK(720), AutoRenew: true,
	})
	require.NoError(t, err)
	require.Equal(t, key, bumped.Key)
	require.NotEqual(t, original.ID, bumped.ID, "different substance -> different #662 id")
	require.False(t, bumped.Archived)

	// The key now resolves to the NEW ($12) row.
	current, err := svc.GetPriceByKey(ctx, key)
	require.NoError(t, err)
	require.Equal(t, bumped.ID, current.ID)
	require.EqualValues(t, 1200000, current.UnitAmount)

	// The OLD ($10) row is archived — displaced, not deleted.
	oldRow, err := svc.GetPrice(ctx, original.ID)
	require.NoError(t, err)
	require.True(t, oldRow.Archived)
	require.Equal(t, key, oldRow.Key, "archived predecessor keeps the key as a back-reference")

	// Grandfather pin: the existing subscription's price_id is untouched and
	// still resolves to the OLD amount.
	var pinnedPriceID uuid.UUID
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT price_id FROM openrails.subscriptions WHERE product_id = $1 AND rail_subscription_id LIKE 'grandfather-sub-%'`,
		product.ID).Scan(&pinnedPriceID))
	require.Equal(t, original.ID, pinnedPriceID)
	pinnedPrice, err := svc.GetPrice(ctx, pinnedPriceID)
	require.NoError(t, err)
	require.EqualValues(t, 1000000, pinnedPrice.UnitAmount, "grandfathered subscription still charges the OLD amount")

	// The pointer-movement log recorded exactly two movements for this key.
	movements, err := svc.rt.PriceService.ListKeyMovements(ctx, merchantID, key)
	require.NoError(t, err)
	require.Len(t, movements, 2)
}

// TestCreatePrice_FlipFlop_TwoRowsTotal is #774's dedup invariant: flipping a
// key between two amounts forever ($10 -> $12 -> $10) reactivates the
// previously-seen archived row instead of minting a third — exactly two rows
// ever exist for the chain, and reactivation is directly observed.
func TestCreatePrice_FlipFlop_TwoRowsTotal(t *testing.T) {
	svc, ctx, merchantID := newPriceKeyTestService(t)

	product := seedTestProduct(t, ctx, svc, "flipflop-"+uuid.NewString()[:8])
	key := product.Key + "-monthly"

	priceReq := func(amount int64) CreatePriceRequest {
		return CreatePriceRequest{
			ProductID: product.ID, Key: key, UnitAmount: amount, Currency: "USD",
			AccessDurationHours: intPtrPK(720), AutoRenew: true,
		}
	}

	ten, err := svc.CreatePrice(ctx, priceReq(1000000))
	require.NoError(t, err)
	twelve, err := svc.CreatePrice(ctx, priceReq(1200000))
	require.NoError(t, err)
	require.NotEqual(t, ten.ID, twelve.ID)

	// Flip back to $10 — must REACTIVATE the original row, not create a third.
	backToTen, err := svc.CreatePrice(ctx, priceReq(1000000))
	require.NoError(t, err)
	require.Equal(t, ten.ID, backToTen.ID, "flip-flop must reactivate the original row (#662 deterministic id)")
	require.False(t, backToTen.Archived)

	twelveNow, err := svc.GetPrice(ctx, twelve.ID)
	require.NoError(t, err)
	require.True(t, twelveNow.Archived, "the displaced $12 row is archived, not deleted")

	// Flip forward again: $10 -> $12 must REACTIVATE the same $12 row.
	twelveAgain, err := svc.CreatePrice(ctx, priceReq(1200000))
	require.NoError(t, err)
	require.Equal(t, twelve.ID, twelveAgain.ID, "re-flipping to $12 must reactivate the SAME row, never a third")

	chain, err := svc.rt.PriceService.ListChainByKey(ctx, merchantID, key)
	require.NoError(t, err)
	require.Len(t, chain, 2, "flip-flopping forever yields exactly two rows total")

	movements, err := svc.rt.PriceService.ListKeyMovements(ctx, merchantID, key)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(movements), 3, "each genuine move (incl. reactivations) logs a movement entry")
}

// TestCreatePrice_RelabelInPlace covers #774's "key renamed" path: a
// substance-unchanged price gets a NEW key via SetPriceKey without changing
// row identity, and displaces whatever else held that key.
func TestCreatePrice_RelabelInPlace(t *testing.T) {
	svc, ctx, _ := newPriceKeyTestService(t)
	product := seedTestProduct(t, ctx, svc, "relabel-"+uuid.NewString()[:8])
	oldKey := product.Key + "-monthly"
	newKey := product.Key + "-premium-monthly"

	price, err := svc.CreatePrice(ctx, CreatePriceRequest{
		ProductID: product.ID, Key: oldKey, UnitAmount: 500000, Currency: "USD",
		AccessDurationHours: intPtrPK(720), AutoRenew: true,
	})
	require.NoError(t, err)

	relabeled, err := svc.SetPriceKey(ctx, price.ID, newKey)
	require.NoError(t, err)
	require.Equal(t, price.ID, relabeled.ID, "relabel never changes row identity")
	require.Equal(t, newKey, relabeled.Key)

	_, err = svc.GetPriceByKey(ctx, oldKey)
	require.Error(t, err, "the old key no longer resolves to anything current")

	current, err := svc.GetPriceByKey(ctx, newKey)
	require.NoError(t, err)
	require.Equal(t, price.ID, current.ID)
}

func intPtrPK(v int) *int { return &v }

func seedCustomerPK(t *testing.T, ctx context.Context, pool *pgxpool.Pool, merchantID uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO openrails.customers (merchant_id, subject) VALUES ($1, $2) RETURNING id`,
		merchantID, "price-key-test-"+uuid.NewString()).Scan(&id))
	return id
}
