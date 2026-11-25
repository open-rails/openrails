//go:build integration

package critical_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

// A database written before the reporting rollups holds cancels of every
// kind a subscription records; upgrading it through them adopts each as a
// cancel decision of its kind.
func TestCancellationsMigration(t *testing.T) {
	t.Run("OR-J31-S17/an-upgrade-through-the-reporting-rollups-adopts-cancel-decisions-of-every-kind", migrateCancelKinds)
}

func migrateCancelKinds(t *testing.T) {
	s := newLegacySchemaBefore(t, "_reporting_mrr.up.sql")
	d := s.seed(t)
	product, psp := uuid.New(), uuid.New()
	d.exec("INSERT INTO "+s.table("products")+" (merchant_id, id, key, display_name, tier_rank, archived) VALUES ($1, $2, 'member', 'Member', 0, false)", d.m, product)
	d.exec("INSERT INTO "+s.table("psps")+" (merchant_id, id, key, rail, environment, account_id) VALUES ($1, $2, 'stripe', 'stripe', 'test', 'acct_upgrade')", d.m, psp)
	kinds := []string{"abandoned", "chargeback", "expired", "merchant", "provider", "upgrade", "user"}
	for i, kind := range kinds {
		customer := d.customer()
		var id uuid.UUID
		require.NoError(t, d.tx.QueryRow(t.Context(), "INSERT INTO "+s.table("subscriptions")+
			" (merchant_id, customer_id, product_id, status, rail, collection_policy, rail_subscription_id, psp_id, started_at, created_at, updated_at)"+
			" VALUES ($1, $2, $3, 'active', 'stripe', 'provider', $4, $5, $6, $6, $6) RETURNING id", d.m, customer, product, fmt.Sprintf("sub_upgrade_%d", i), psp, d.at).Scan(&id))
		d.exec("UPDATE "+s.table("subscriptions")+" SET status = 'canceled', cancel_type = $3, canceled_at = $4, ended_at = $4, updated_at = $4, lifecycle_rev = lifecycle_rev + 1"+
			" WHERE merchant_id = $1 AND id = $2", d.m, id, kind, d.at.Add(time.Hour))
	}
	d.commit()

	require.NoError(t, s.apply(t.Context(), s.full), "every cancel kind passes the backfill")
	rows, err := s.pool.Query(t.Context(), "SELECT cancel_type FROM "+s.table("subscription_cancellations")+" WHERE merchant_id = $1 AND reversed_at IS NULL ORDER BY 1", d.m)
	require.NoError(t, err)
	adopted, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	require.Equal(t, kinds, adopted, "one cancel decision of each kind")
}
