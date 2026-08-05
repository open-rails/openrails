//go:build integration

package subscriptions

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

// or#861: the #816 re-driver enumerated its work with `GenGlobal()`, believing
// the base pool gave it a cross-merchant view. It does not — subscription_reprices
// FORCEs RLS, so under the production openrails_app role the list came back
// EMPTY and the re-driver has never re-driven anything. Blocked plan-change rows
// (deferred pushes and crash-window rows) sat blocked forever.
//
// The fix is a per-merchant walk: 0022's SECURITY DEFINER work queue names the
// merchants, and every row read happens inside that merchant's own scope.
func TestRedriverEnumeratesMerchantsUnderTheEnforcingRole(t *testing.T) {
	ctx := context.Background()
	f := newRepriceFixture(t)
	mctx := dbtest.WithTestMerchant(ctx)

	// One blocked plan_change row with the re-drivable reason prefix. Seeded on
	// the merchant's own RLS-enforcing pool, like the product writes it.
	repriceID := uuid.New()
	subID := uuid.New()
	custID := uuid.New()
	now := time.Now().UTC()
	_, err := f.pool.Exec(ctx,
		`INSERT INTO openrails.customers (id, merchant_id) VALUES ($1, $2)`, custID, f.merchantID)
	require.NoError(t, err)
	_, err = f.pool.Exec(ctx, `
INSERT INTO openrails.subscriptions
    (id, price_id, product_id, status, rail, psp_id, rail_subscription_id,
     current_period_starts_at, current_period_ends_at, started_at, customer_id, merchant_id)
VALUES ($1, $2, $3, 'active', 'nmi', $4, $5, $6, $7, $6, $8, $9)`,
		subID, f.lowPriceID, f.productID, f.nmiPSPID, "or861-"+subID.String()[:8],
		now.Add(-24*time.Hour), now.Add(24*time.Hour), custID, f.merchantID)
	require.NoError(t, err)
	_, err = f.pool.Exec(ctx, `
INSERT INTO openrails.subscription_reprices
    (id, merchant_id, subscription_id, kind, status, from_price_id, to_price_id,
     effective_at, blocked_reason)
VALUES ($1, $2, $3, 'plan_change', 'blocked', $4, $5, now() + interval '30 days',
        'rail_push_failed:nmi_deferred_push_required')`,
		repriceID, f.merchantID, subID, f.lowPriceID, f.highPriceID)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(),
			`DELETE FROM openrails.subscription_reprices WHERE id = $1`, repriceID)
	})

	// An UNPINNED handle: the posture the River re-driver worker actually runs in.
	unpinned := dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t))
	repo := NewRepriceRepo(unpinned)

	t.Run("failing_before: the base-pool row read returns nothing", func(t *testing.T) {
		rows, err := gen.New(unpinned.Pool()).ListRedrivableBlockedPlanChangeReprices(ctx, 500)
		require.NoError(t, err, "no error is exactly why this went unnoticed for a whole feature")
		require.Empty(t, rows, "a GUC-less read of subscription_reprices can only ever return nothing")
	})

	t.Run("the definer work queue names the merchant", func(t *testing.T) {
		ids, err := repo.ListRedrivableMerchants(ctx, 500)
		require.NoError(t, err)
		require.Contains(t, ids, f.merchantID)
	})

	t.Run("the rows are readable inside the merchant's own scope", func(t *testing.T) {
		var found bool
		require.NoError(t, unpinned.RunInMerchantScope(ctx, merchant.ID(f.merchantID), "redrive-test",
			func(ctx context.Context) error {
				rows, err := repo.ListRedrivableBlockedPlanChanges(ctx, 500)
				if err != nil {
					return err
				}
				for _, r := range rows {
					if r.ID == repriceID {
						found = true
					}
				}
				return nil
			}))
		require.True(t, found, "the blocked row the re-driver exists for must be visible to it")
	})

	// The end-to-end pass (what the re-driver DOES with the row — push, defer,
	// skip) is the #816 suite's subject; those tests now exercise this same
	// enumeration, so they are the regression net for the behaviour.
	_ = mctx
}
