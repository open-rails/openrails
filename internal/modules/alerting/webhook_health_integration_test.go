//go:build integration

package alerting_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/modules/alerting"
	"github.com/open-rails/openrails/internal/modules/metrics"
	"github.com/open-rails/openrails/internal/modules/webhookhealth"
)

// seedArmedRailWithBillableSub arms rail "nmi" (psps row) and
// gives it one active auto-renewing subscription — the webhook_silence
// expectation gate (#786).
func seedArmedRailWithBillableSub(t *testing.T, pool *pgxpool.Pool, mid uuid.UUID) {
	t.Helper()
	cust, prod, price := uuid.New(), uuid.New(), uuid.New()
	exec(t, pool, `INSERT INTO openrails.customers (id, merchant_id) VALUES ($1,$2)`, cust, mid)
	exec(t, pool, `INSERT INTO openrails.products (id, key, display_name, merchant_id) VALUES ($1,$2,'P',$3)`, prod, "k-"+uuid.NewString()[:8], mid)
	exec(t, pool, `INSERT INTO openrails.prices (id, product_id, amount, currency, merchant_id, auto_renew, access_duration_hours) VALUES ($1,$2,10000000,'USD',$3,true,720)`, price, prod, mid)
	psp := uuid.New()
	exec(t, pool, `INSERT INTO openrails.psps (id, merchant_id, rail, account_id) VALUES ($1,$2,'nmi',$3)`, psp, mid, "579145")
	exec(t, pool, `INSERT INTO openrails.subscriptions (id, merchant_id, customer_id, product_id, price_id, rail, psp_id, rail_subscription_id, status, started_at)
		VALUES ($1,$2,$3,$4,$5,'nmi',$6,$7,'active',now() - interval '30 days')`, uuid.New(), mid, cust, prod, price, psp, "sub-"+uuid.NewString()[:8])
}

func countRuleNotifications(t *testing.T, pool *pgxpool.Pool, mid, ruleID uuid.UUID) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*) FROM openrails.merchant_notifications WHERE merchant_id=$1 AND rule_id=$2`, mid, ruleID).Scan(&n))
	return n
}

// webhookHealthRow reads the two substrates #786 actually has: the silence
// watermark on the snapshot row, and the daily counter buckets the metrics read.
// or#823 dropped webhook_health's lifetime tallies — they were incremented on
// every event and consulted by nothing, so asserting on them proved only that
// the write happened.
func webhookHealthRow(t *testing.T, pool *pgxpool.Pool, mid uuid.UUID, rail string) (lastAccepted *time.Time, rejected, drift int64) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT last_accepted_at FROM openrails.webhook_health WHERE merchant_id=$1 AND rail=$2`,
		mid, rail).Scan(&lastAccepted))
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(rejected),0)::bigint, COALESCE(SUM(drift),0)::bigint
		   FROM openrails.webhook_health_daily WHERE merchant_id=$1 AND rail=$2`,
		mid, rail).Scan(&rejected, &drift))
	return
}

// TestWebhookHealthAlertCycle drives the full #786 cycle on a fake clock:
// verified ingest stamps the watermark → silence past window on an armed rail
// with a billable sub fires ONCE (edge) → a fresh accepted webhook clears;
// signature rejects bump the reject counter WITHOUT touching the accepted
// watermark and fire webhook_rejects at threshold; a pull correction with no
// webhook since the previous pull passes the drift gate and fires
// webhook_drift; a correction right after an accepted webhook does not.
func TestWebhookHealthAlertCycle(t *testing.T) {
	pool, appDB := rlsSetup(t)
	mid := uuid.New()
	seedMerchant(t, pool, mid)
	seedArmedRailWithBillableSub(t, pool, mid)

	clk := clockwork.NewFakeClockAt(time.Now().UTC())
	svc := alerting.NewService(alerting.Deps{
		DB:      appDB,
		Metrics: metrics.NewService(appDB),
		Clock:   clk,
	})
	rec := &webhookhealth.Recorder{DB: appDB, Clock: clk}
	inApp := []alerting.ChannelRef{{Type: alerting.ChannelInApp}}

	var silenceRule, rejectsRule, driftRule alerting.Rule
	inConn(t, appDB, mid, func(ctx context.Context) {
		var err error
		silenceRule, err = svc.CreateRule(ctx, alerting.CreateRuleInput{
			Name: "silence", Template: "webhook_silence",
			Params: map[string]any{"window": "2d"}, Channels: inApp,
		})
		require.NoError(t, err)
		rejectsRule, err = svc.CreateRule(ctx, alerting.CreateRuleInput{
			Name: "rejects", Template: "webhook_rejects",
			Params: map[string]any{"min_count": 3, "window": "1d"}, Channels: inApp,
		})
		require.NoError(t, err)
		driftRule, err = svc.CreateRule(ctx, alerting.CreateRuleInput{
			Name: "drift", Template: "webhook_drift",
			Params: map[string]any{"min_count": 1, "window": "1d"}, Channels: inApp,
		})
		require.NoError(t, err)
	})

	evaluate := func() alerting.EvalStats {
		t.Helper()
		stats, err := svc.EvaluateMerchant(mctx(mid))
		require.NoError(t, err)
		return stats
	}

	// 1. Verified-accepted ingest stamps the watermark; nothing fires.
	rec.Accepted(mctx(mid), "nmi")
	firstAccepted, _, _ := webhookHealthRow(t, pool, mid, "nmi")
	require.NotNil(t, firstAccepted)
	stats := evaluate()
	require.Equal(t, 0, stats.Fired)
	require.Equal(t, 0, countNotifications(t, pool, mid))

	// 2. Clock past the 2d window on the armed+billable rail: silence fires ONCE.
	clk.Advance(3 * 24 * time.Hour)
	stats = evaluate()
	require.Equal(t, 1, stats.Fired)
	require.Equal(t, 1, countRuleNotifications(t, pool, mid, silenceRule.ID))
	// Edge-triggered: still silent → no re-fire.
	stats = evaluate()
	require.Equal(t, 0, stats.Fired)
	require.Equal(t, 1, countRuleNotifications(t, pool, mid, silenceRule.ID))

	// 3. A new accepted webhook resets the watermark; re-eval clears the alert.
	rec.Accepted(mctx(mid), "nmi")
	stats = evaluate()
	require.Equal(t, 1, stats.Cleared)
	require.Equal(t, 0, stats.Fired)
	require.Equal(t, 1, countRuleNotifications(t, pool, mid, silenceRule.ID))

	// 4. Signature rejects (arriving later than the last accept, so a wrongly
	// stamped watermark is detectable) bump the reject counter but NEVER the
	// accepted watermark; the third reject crosses the webhook_rejects threshold.
	clk.Advance(time.Hour)
	acceptedBefore, _, _ := webhookHealthRow(t, pool, mid, "nmi")
	for i := 0; i < 3; i++ {
		rec.Rejected(mctx(mid), "nmi")
	}
	acceptedAfter, rejectedCount, _ := webhookHealthRow(t, pool, mid, "nmi")
	require.Equal(t, int64(3), rejectedCount)
	require.Equal(t, acceptedBefore.UTC(), acceptedAfter.UTC(), "a rejected webhook must not stamp the accepted watermark")
	stats = evaluate()
	require.Equal(t, 1, stats.Fired)
	require.Equal(t, 1, countRuleNotifications(t, pool, mid, rejectsRule.ID))
	require.Equal(t, 0, countRuleNotifications(t, pool, mid, driftRule.ID))

	// 5. Drift gate. First pull (an hour after the last accepted webhook): no
	// previous pull watermark → an initial import is not drift.
	clk.Advance(time.Hour)
	inConn(t, appDB, mid, func(ctx context.Context) {
		admitted, err := webhookhealth.Drift(ctx, appDB, "nmi", clk.Now(), 2)
		require.NoError(t, err)
		require.False(t, admitted, "first-ever pull must not count as drift")
		require.NoError(t, webhookhealth.StampPull(ctx, appDB, "nmi", clk.Now()))
	})

	// Next refresh 4h later finds corrections; no webhook was accepted since
	// before the previous pull → drift admitted, webhook_drift fires.
	clk.Advance(4 * time.Hour)
	inConn(t, appDB, mid, func(ctx context.Context) {
		admitted, err := webhookhealth.Drift(ctx, appDB, "nmi", clk.Now(), 2)
		require.NoError(t, err)
		require.True(t, admitted, "correction with stale accepted watermark must count as drift")
		require.NoError(t, webhookhealth.StampPull(ctx, appDB, "nmi", clk.Now()))
	})
	_, _, driftCount := webhookHealthRow(t, pool, mid, "nmi")
	require.Equal(t, int64(2), driftCount)
	stats = evaluate()
	require.Equal(t, 1, stats.Fired)
	require.Equal(t, 1, countRuleNotifications(t, pool, mid, driftRule.ID))

	// 6. A correction announced by a fresh webhook is NOT drift (gate closed).
	clk.Advance(4 * time.Hour)
	rec.Accepted(mctx(mid), "nmi")
	inConn(t, appDB, mid, func(ctx context.Context) {
		admitted, err := webhookhealth.Drift(ctx, appDB, "nmi", clk.Now(), 1)
		require.NoError(t, err)
		require.False(t, admitted, "webhook-announced change must not count as drift")
	})
	_, _, driftCount = webhookHealthRow(t, pool, mid, "nmi")
	require.Equal(t, int64(2), driftCount)
}

// TestWebhookSilenceExpectationGate: without an armed rail carrying billable
// subscriptions there is no expected traffic, so total silence never fires.
func TestWebhookSilenceExpectationGate(t *testing.T) {
	pool, appDB := rlsSetup(t)
	mid := uuid.New()
	seedMerchant(t, pool, mid)

	clk := clockwork.NewFakeClockAt(time.Now().UTC())
	svc := alerting.NewService(alerting.Deps{DB: appDB, Metrics: metrics.NewService(appDB), Clock: clk})
	rec := &webhookhealth.Recorder{DB: appDB, Clock: clk}

	inConn(t, appDB, mid, func(ctx context.Context) {
		_, err := svc.CreateRule(ctx, alerting.CreateRuleInput{
			Name: "silence", Template: "webhook_silence",
			Params:   map[string]any{"window": "2d"},
			Channels: []alerting.ChannelRef{{Type: alerting.ChannelInApp}},
		})
		require.NoError(t, err)
	})

	// A tracked-but-unexpected rail (one old accepted webhook, no armed account,
	// no billable subs) stays quiet forever.
	rec.Accepted(mctx(mid), "nmi")
	clk.Advance(30 * 24 * time.Hour)
	stats, err := svc.EvaluateMerchant(mctx(mid))
	require.NoError(t, err)
	require.Equal(t, 0, stats.Fired)
	require.Equal(t, 0, countNotifications(t, pool, mid))
}
