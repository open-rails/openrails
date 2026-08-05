//go:build integration

package reconcile

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/merchant"
)

// #696 end-to-end: a parked-unknown CCBill subscription resolves via the REAL
// CCBillSubscriptionProber against a fake DataLink HTTP server (the ONLY
// fake) — no bulk export needed. Two shapes: provider-alive (adopt the
// provider clock) and provider-dead (cancel).
func TestReconcileUnknownCohort_CCBillProbeResolves(t *testing.T) {
	appDB := startReconcilePostgres(t)
	merchantID := dbtest.TestMerchantID.UUID()
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	lc := subscriptions.NewSubscriptionLifecycleService(appDB, nil, nil, nil, nil, nil, clockwork.NewRealClock())
	now := time.Now().UTC().Truncate(time.Second)
	periodEnd := now.Add(-5 * 24 * time.Hour)
	start := now.Add(-35 * 24 * time.Hour)
	futureExpiry := now.Add(25 * 24 * time.Hour)
	sfx := uuid.NewString()[:8]

	rsAlive, rsDead := "cc-probe-alive-"+sfx, "cc-probe-dead-"+sfx
	subAlive, subDead := uuid.New(), uuid.New()
	prices := map[uuid.UUID]uuid.UUID{}

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		exec := func(sql string, args ...any) {
			_, err := appDB.Qx(ctx).Exec(ctx, sql, args...)
			require.NoError(t, err)
		}
		pspID := dbtest.EnsureTestPSP(ctx, t, appDB.Qx(ctx), merchantID, "ccbill")
		mk := func(id uuid.UUID, railSub string) {
			cust := dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
			prod, price := uuid.New(), uuid.New()
			prices[id] = price
			exec(`INSERT INTO openrails.products (id,key,display_name,entitlements_spec,merchant_id) VALUES ($1,$2,$2,'{}'::jsonb,$3)`, prod, "ccp-"+railSub, merchantID)
			exec(`INSERT INTO openrails.prices (id,product_id,amount,currency,merchant_id) VALUES ($1,$2,5000000,'USD',$3)`, price, prod, merchantID)
			exec(`INSERT INTO openrails.subscriptions (id,merchant_id,customer_id,product_id,price_id,status,rail,rail_subscription_id,started_at,current_period_starts_at,current_period_ends_at,psp_id)
			      VALUES ($1,$2,$3,$4,$5,'unknown','ccbill',$6,$7,$7,$8,$9)`, id, merchantID, cust, prod, price, railSub, start, periodEnd, pspID)
		}
		mk(subAlive, rsAlive)
		mk(subDead, rsDead)
		return nil
	}))
	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			for id, price := range prices {
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.entitlements WHERE source_id=$1`, id)
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.payments WHERE subscription_id=$1`, id)
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.subscriptions WHERE id=$1`, id)
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.prices WHERE id=$1`, price)
			}
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.products WHERE key LIKE 'ccp-cc-probe-%'||$1`, sfx)
			return nil
		})
	})

	// Fake DataLink SMS: per-subscription answers; mutation guard.
	var mutations atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		if r.PostForm.Get("action") != "viewSubscriptionStatus" {
			mutations.Add(1)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch r.PostForm.Get("subscriptionId") {
		case rsAlive:
			fmt.Fprintf(w, `<results><subscriptionStatus>2</subscriptionStatus><expirationDate>%s</expirationDate></results>`, futureExpiry.Format("20060102"))
		case rsDead:
			fmt.Fprint(w, `<results><subscriptionStatus>0</subscriptionStatus></results>`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	prober := &CCBillSubscriptionProber{Client: &ccbill.DataLinkClient{
		BaseURL: srv.URL, ClientAccNum: "900100", Username: "u", Password: "p", HTTPClient: srv.Client(),
	}}

	// No ccbill bulk fetcher configured — the per-sub probe alone must resolve.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := ReconcileUnknownCohort(ctx, appDB, lc,
			map[Provider]RailFetcher{}, map[Provider]SubscriptionProber{ProviderCCBill: prober},
			dbtest.TestMerchantID, now, UnknownReconcileOptions{})
		require.NoError(t, err)
		assert.GreaterOrEqual(t, res.Probed, 2, "both rows resolved via targeted probes")
		assert.GreaterOrEqual(t, res.Adopted, 1)
		assert.GreaterOrEqual(t, res.Cancelled, 1)
		return nil
	}))
	assert.Zero(t, mutations.Load(), "probing must never mutate")

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		var status string
		var end *time.Time
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT status, current_period_ends_at FROM openrails.subscriptions WHERE id=$1`, subAlive).Scan(&status, &end))
		assert.Equal(t, "active", status, "provider-alive: adopted back to active")
		require.NotNil(t, end)
		assert.True(t, end.After(now), "adopted the provider's future boundary")

		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT status FROM openrails.subscriptions WHERE id=$1`, subDead).Scan(&status))
		assert.Equal(t, "cancelled", status, "provider-dead: resolved cancelled")
		return nil
	}))
}
