//go:build integration

package controlplane

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

// or#861: the hosted fleet dashboard read payments/subscriptions/prices/psps on
// the base pool, believing the control plane held a privileged one. It does not
// — controlplane/service.go opens the SAME DSN as the app, so under the
// production openrails_app role every aggregate matched `merchant_id = NULL`
// and the dashboard reported ALL ZEROS. It only ever appeared to work because
// dev and CI connect as a superuser.
//
// The whole test runs the ControlPlane over the openrails_app pool, which is
// the posture that makes the bug observable at all.
func TestFleetAggregatesUnderTheEnforcingRole(t *testing.T) {
	ctx := context.Background()
	appPool, err := pgxpool.New(ctx, dbtest.SharedPostgresDSN(t))
	require.NoError(t, err)
	t.Cleanup(appPool.Close)

	cfg := &config.Config{
		Env:  "test",
		DB:   &config.DBConfig{},
		Auth: &config.AuthConfig{Issuer: "https://openrails.test", MintDisabled: true, DirectPeerIP: true},
	}
	rdb, _ := dbtest.SharedRedisClient(t)
	cp, err := New(ctx, cfg, appPool, WithRedis(rdb))
	require.NoError(t, err)

	sfx := strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	mid := uuid.New()
	seedFleetMerchantWithRevenue(t, mid, sfx)

	t.Run("failing_before: the same aggregate on the base pool sees nothing", func(t *testing.T) {
		var payments int64
		require.NoError(t, appPool.QueryRow(ctx, `
SELECT count(*) FROM openrails.payments
 WHERE status = 'completed' AND reversal_kind IS NULL AND merchant_id = $1`, mid).Scan(&payments))
		require.Zero(t, payments,
			"a GUC-less read of payments returns zero rows and no error — the silence this whole class is made of")
	})

	t.Run("FleetAnalytics reports the merchant's real revenue", func(t *testing.T) {
		out, err := cp.FleetAnalytics(ctx, merchant.ID{}, 30)
		require.NoError(t, err)
		require.NotZero(t, out.Merchants.Total, "the funnel must count provisioned merchants")
		require.NotZero(t, out.Merchants.FirstRevenue, "a merchant with a settled payment has first revenue")

		var settled int64
		for _, r := range out.Revenue {
			if r.Currency == "USD" {
				settled += r.SettledAmount
			}
		}
		require.GreaterOrEqual(t, settled, int64(7_000_000), "settled fleet volume must include the seeded sale")

		var nmiSucceeded int64
		for _, r := range out.Rails {
			if r.Rail == "nmi" {
				nmiSucceeded += r.Succeeded
			}
		}
		require.NotZero(t, nmiSucceeded, "rail health must see the seeded completed payment")
	})

	t.Run("FleetTimeseries reports weekly movement", func(t *testing.T) {
		out, err := cp.FleetTimeseries(ctx, merchant.ID{}, 12)
		require.NoError(t, err)
		require.NotEmpty(t, out.Points)

		var active int64
		for _, p := range out.Points {
			active += p.ActiveMerchants
		}
		require.NotZero(t, active, "a merchant with a settled sale is active in its week")

		var volume int64
		for _, v := range out.Volume {
			volume += v.SettledAmount
		}
		require.GreaterOrEqual(t, volume, int64(7_000_000), "weekly volume must include the seeded sale")
	})

	t.Run("excluding a merchant still works through the definer", func(t *testing.T) {
		all, err := cp.FleetAnalytics(ctx, merchant.ID{}, 30)
		require.NoError(t, err)
		without, err := cp.FleetAnalytics(ctx, merchant.ID(mid), 30)
		require.NoError(t, err)
		require.Greater(t, all.Merchants.Total, without.Merchants.Total,
			"p_exclude must drop exactly the named merchant, not be ignored")
	})
}

// seedFleetMerchantWithRevenue seeds one active merchant with one completed NMI
// sale, on the merchant's OWN RLS-enforcing connection — the fixture proves the
// merchant can write its own rows, so nothing here is superuser magic.
func seedFleetMerchantWithRevenue(t *testing.T, mid uuid.UUID, sfx string) {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.SharedMerchantPool(t, mid)
	exec := func(sql string, args ...any) {
		t.Helper()
		_, err := pool.Exec(ctx, sql, args...)
		require.NoError(t, err, sql)
	}
	custID, prodID, priceID, payID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO openrails.merchants (id, slug, status) VALUES ($1, $2, 'active')`, mid, "fleet-"+sfx)
	exec(`INSERT INTO openrails.customers (id, merchant_id) VALUES ($1, $2)`, custID, mid)
	exec(`INSERT INTO openrails.products (id, key, display_name, merchant_id) VALUES ($1, $2, 'Fleet', $3)`,
		prodID, "fleet-prod-"+sfx, mid)
	exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, merchant_id, auto_renew, access_duration_hours)
	      VALUES ($1, $2, 7000000, 'USD', $3, true, 720)`, priceID, prodID, mid)
	pspID := dbtest.EnsureTestPSP(ctx, t, pool, mid, "nmi")
	exec(`INSERT INTO openrails.payments
	        (id, merchant_id, customer_id, price_id, transaction_id, amount, list_amount, currency, status, rail, purchased_at, psp_id)
	      VALUES ($1, $2, $3, $4, $5, 7000000, 7000000, 'USD', 'completed', 'nmi', $6, $7)`,
		payID, mid, custID, priceID, "fleet-txn-"+sfx, time.Now().UTC().Add(-time.Hour), pspID)
}
