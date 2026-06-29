//go:build integration

package repo

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
)

// GetByProductID must return ALL prices (active + archived/draft). The catalog
// converge relies on it to reconcile an already-archived historical price
// instead of re-creating it — a prior bug wired GetByProductID to the
// active-only query, so re-applying a manifest with archived legacy tiers blew
// up on unique_prices_product_amount_cycle. GetActiveByProductID stays
// active-only. Real Postgres via testcontainers.
func TestGetByProductID_IncludesArchived(t *testing.T) {
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

	activeID, archivedID := uuid.New(), uuid.New()
	insertPrice := func(id uuid.UUID, amount int64, status string) {
		_, e := pool.Exec(ctx,
			`INSERT INTO openrails.prices (id, product_id, merchant_id, amount, currency, access_duration_hours, auto_renew, status)
			 VALUES ($1,$2,$3,$4,'usd',720,true,$5)`,
			id, productID, merchantID, amount, status)
		require.NoError(t, e)
	}
	insertPrice(activeID, 2300, "active")
	insertPrice(archivedID, 1900, "archived") // historical tier

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.prices WHERE product_id = $1`, productID)
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.products WHERE id = $1`, productID)
	})

	ids := func(t *testing.T, productID uuid.UUID, activeOnly bool) map[uuid.UUID]bool {
		r := NewPriceRepo(dbi)
		var rows []uuid.UUID
		got := map[uuid.UUID]bool{}
		prices, err := r.GetByProductID(ctx, productID)
		if activeOnly {
			prices, err = r.GetActiveByProductID(ctx, productID)
		}
		require.NoError(t, err)
		for _, p := range prices {
			got[p.ID] = true
			rows = append(rows, p.ID)
		}
		return got
	}

	all := ids(t, productID, false)
	require.True(t, all[activeID], "active price must be returned")
	require.True(t, all[archivedID], "ARCHIVED price must be returned (so the converge reconciles, not re-creates, it)")

	active := ids(t, productID, true)
	require.True(t, active[activeID], "active-only variant returns the active price")
	require.False(t, active[archivedID], "active-only variant must EXCLUDE archived")
}
