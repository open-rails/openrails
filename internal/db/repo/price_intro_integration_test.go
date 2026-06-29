//go:build integration

package repo

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

// #602: a price can carry an introductory/trial FIRST period (initial_amount +
// initial_period_days) distinct from its recurring terms. Proves the schema +
// generated code + model round-trip against real Postgres: a step-down intro
// ($19.95 -> $14.95), a free trial (initial_amount 0), and a flat price (no
// intro) all persist and read back correctly. A $0 initial is a valid trial,
// NOT "no intro".
func TestPriceRepo_IntroPricing_RoundTrip(t *testing.T) {
	ctx := dbtest.WithTestMerchant(context.Background())
	pool := dbtest.SharedPGXPool(t)
	merchantID := dbtest.TestMerchantID.UUID()

	dbi, err := db.NewWithPGXPool(pool, "")
	require.NoError(t, err)

	productID := uuid.New()
	_, err = pool.Exec(ctx,
		`INSERT INTO openrails.products (id, merchant_id, slug, display_name) VALUES ($1,$2,$3,$3)`,
		productID, merchantID, "prod-"+uuid.NewString()[:8])
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.prices WHERE product_id = $1`, productID)
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.products WHERE id = $1`, productID)
	})

	now := time.Now().UTC().Truncate(time.Second)
	cycle := 30
	r := NewPriceRepo(dbi)
	mk := func(amount, initialAmount int64, initialDays int, withIntro bool) *models.Price {
		p := &models.Price{
			ID: uuid.New(), MerchantID: merchantID, ProductID: productID,
			Status: models.CatalogStatusArchived, Amount: amount, Currency: "usd",
			BillingCycleDays: &cycle, CreatedAt: now, UpdatedAt: now,
		}
		if withIntro {
			ia, id := initialAmount, initialDays
			p.InitialAmount, p.InitialPeriodDays = &ia, &id
		}
		require.NoError(t, r.Create(ctx, p))
		return p
	}
	mk(1495, 1995, 30, true) // step-down: $19.95 first 30d -> $14.95/30d
	mk(1500, 0, 7, true)     // free trial: $0/7d -> $15/30d
	mk(2300, 0, 0, false)    // flat $23/30d, no intro

	prices, err := r.GetByProductID(ctx, productID)
	require.NoError(t, err)
	byAmount := map[int64]*models.Price{}
	for _, p := range prices {
		byAmount[p.Amount] = p
	}

	// step-down intro round-trips
	step := byAmount[1495]
	require.NotNil(t, step)
	a, d, ok := step.GetIntro()
	require.True(t, ok)
	require.Equal(t, int64(1995), a)
	require.Equal(t, 30, d)

	// free trial: initial amount 0 is still an intro (NOT "no intro")
	trial := byAmount[1500]
	require.NotNil(t, trial)
	ta, td, ok := trial.GetIntro()
	require.True(t, ok, "initial_amount 0 is a free trial, still an intro")
	require.Equal(t, int64(0), ta)
	require.Equal(t, 7, td)

	// flat price has no intro
	flat := byAmount[2300]
	require.NotNil(t, flat)
	require.False(t, flat.HasIntro())
	require.Nil(t, flat.InitialAmount)
	require.Nil(t, flat.InitialPeriodDays)
}
