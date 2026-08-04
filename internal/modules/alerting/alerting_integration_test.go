//go:build integration

package alerting_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/alerting"
	"github.com/open-rails/openrails/internal/modules/metrics"
	"github.com/open-rails/openrails/internal/shared/httpx"
	"github.com/open-rails/openrails/pkg/merchant"
)

// --- harness -----------------------------------------------------------------

// rlsSetup returns a privileged super pool (for cross-merchant fixture seeding)
// and an app-role *db.DB (RLS-enforcing) the alerting service runs on — the real
// production isolation posture.
func rlsSetup(t *testing.T) (*pgxpool.Pool, *db.DB) {
	t.Helper()
	superDSN, appDSN := dbtest.SharedRLSPostgres(t)
	cfg, err := pgxpool.ParseConfig(superDSN)
	require.NoError(t, err)
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool, dbtest.OpenAppDB(t, appDSN)
}

func mctx(mid uuid.UUID) context.Context {
	return merchant.WithID(context.Background(), merchant.ID(mid))
}

// inConn runs fn on an RLS-pinned merchant connection (simulating the request
// middleware) so request-path service methods hit the right merchant.
func inConn(t *testing.T, appDB *db.DB, mid uuid.UUID, fn func(ctx context.Context)) {
	t.Helper()
	require.NoError(t, appDB.RunInMerchantConn(mctx(mid), func(ctx context.Context) error {
		fn(ctx)
		return nil
	}))
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	_, err := pool.Exec(context.Background(), sql, args...)
	require.NoError(t, err, sql)
}

func seedMerchant(t *testing.T, pool *pgxpool.Pool, mid uuid.UUID) {
	t.Helper()
	slug := "alert-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	exec(t, pool, `INSERT INTO openrails.merchants (id, slug, status) VALUES ($1,$2,'active') ON CONFLICT (id) DO NOTHING`, mid, slug)
	t.Cleanup(func() {
		ctx := context.Background()
		for _, tbl := range []string{"merchant_notifications", "alert_rules", "merchant_webhooks", "payments", "subscriptions", "prices", "products", "customers", "rail_merchant_accounts", "merchant_configurations", "webhook_health", "webhook_health_daily", "reconciliation_findings", "reconciliation_runs", "finding_digest_state"} {
			_, _ = pool.Exec(ctx, `DELETE FROM openrails.`+tbl+` WHERE merchant_id = $1`, mid)
		}
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.merchants WHERE id = $1`, mid)
	})
}

// seedChargebackBase seeds a rail account + 2 settled payments (payment_count=2)
// so chargeback_rate has a denominator. Returns the account label + a payment id.
func seedChargebackBase(t *testing.T, pool *pgxpool.Pool, mid uuid.UUID) (acctLabel string, payID uuid.UUID) {
	t.Helper()
	cust := uuid.New()
	prod := uuid.New()
	price := uuid.New()
	acct := uuid.New()
	acctLabel = "945280-0000"
	exec(t, pool, `INSERT INTO openrails.customers (id, merchant_id) VALUES ($1,$2)`, cust, mid)
	exec(t, pool, `INSERT INTO openrails.products (id, key, display_name, merchant_id) VALUES ($1,$2,'P',$3)`, prod, "k-"+uuid.NewString()[:8], mid)
	exec(t, pool, `INSERT INTO openrails.prices (id, product_id, amount, currency, merchant_id) VALUES ($1,$2,10000000,'USD',$3)`, price, prod, mid)
	exec(t, pool, `INSERT INTO openrails.psps (id, merchant_id, rail, account_id) VALUES ($1,$2,'nmi',$3)`, acct, mid, acctLabel)
	now := time.Now().UTC()
	p1, p2 := uuid.New(), uuid.New()
	ins := `INSERT INTO openrails.payments (id, merchant_id, customer_id, price_id, rail, psp_id, transaction_id, amount, list_amount, currency, status, attempt_kind, purchased_at, created_at)
		VALUES ($1,$2,$3,$4,'nmi',$5,$6,10000000,10000000,'USD','completed','initial',$7,$7)`
	exec(t, pool, ins, p1, mid, cust, price, acct, "tx-"+uuid.NewString()[:8], now.AddDate(0, 0, -3))
	exec(t, pool, ins, p2, mid, cust, price, acct, "tx-"+uuid.NewString()[:8], now.AddDate(0, 0, -2))
	return acctLabel, p1
}

// addChargeback inserts a chargeback mirror row on the same rail account
// (chargeback_count += 1). removeChargebacks deletes them (rate → 0).
func addChargeback(t *testing.T, pool *pgxpool.Pool, mid, refPay uuid.UUID) {
	t.Helper()
	cust := uuid.New()
	exec(t, pool, `INSERT INTO openrails.customers (id, merchant_id) VALUES ($1,$2)`, cust, mid)
	now := time.Now().UTC()
	exec(t, pool, `INSERT INTO openrails.payments (id, merchant_id, customer_id, price_id, refunded_payment_id, rail, psp_id, transaction_id, amount, list_amount, currency, status, reversal_kind, purchased_at, created_at)
		SELECT $1,$2,$3, price_id, $4, 'nmi', psp_id, $5, -10000000, 10000000, 'USD', 'completed', 'chargeback', $6, $6
		FROM openrails.payments WHERE id=$4`,
		uuid.New(), mid, cust, refPay, "cb-"+uuid.NewString()[:8], now.AddDate(0, 0, -1))
}

func removeChargebacks(t *testing.T, pool *pgxpool.Pool, mid uuid.UUID) {
	t.Helper()
	exec(t, pool, `DELETE FROM openrails.payments WHERE merchant_id=$1 AND reversal_kind='chargeback'`, mid)
}

func newService(appDB *db.DB, email alerting.EmailSender) *alerting.Service {
	return alerting.NewService(alerting.Deps{
		DB:      appDB,
		Metrics: metrics.NewService(appDB),
		Email:   email,
		// #SEC-21: the sinks under test are loopback httptest servers; the
		// production policy (zero value) refuses those.
		Outbound:       httpx.Policy{Allow: httpx.AllowLoopback},
		WebhookBackoff: time.Millisecond,
	})
}

// fake email sender
type fakeEmail struct {
	enabled bool
	mu      sync.Mutex
	sent    []string // "to|subject"
}

func (f *fakeEmail) IsEnabled() bool { return f.enabled }
func (f *fakeEmail) SendEmail(_ context.Context, to, subject, _, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, to+"|"+subject)
	return nil
}
func (f *fakeEmail) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.sent) }

// webhook recorder
type webhookRecorder struct {
	mu        sync.Mutex
	bodies    []map[string]any
	failFirst int
	calls     int
	server    *httptest.Server
}

func newWebhookRecorder(t *testing.T, failFirst int) *webhookRecorder {
	rec := &webhookRecorder{failFirst: failFirst}
	rec.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.calls++
		call := rec.calls
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		rec.bodies = append(rec.bodies, body)
		rec.mu.Unlock()
		if call <= rec.failFirst {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(rec.server.Close)
	return rec
}

func (w *webhookRecorder) callCount() int { w.mu.Lock(); defer w.mu.Unlock(); return w.calls }
func (w *webhookRecorder) lastBody() map[string]any {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.bodies) == 0 {
		return nil
	}
	return w.bodies[len(w.bodies)-1]
}

func countNotifications(t *testing.T, pool *pgxpool.Pool, mid uuid.UUID) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*) FROM openrails.merchant_notifications WHERE merchant_id=$1`, mid).Scan(&n))
	return n
}

// --- tests -------------------------------------------------------------------

func TestRuleCRUDAndRLSIsolation(t *testing.T) {
	pool, appDB := rlsSetup(t)
	mA, mB := uuid.New(), uuid.New()
	seedMerchant(t, pool, mA)
	seedMerchant(t, pool, mB)
	svc := newService(appDB, nil)

	var ruleA alerting.Rule
	inConn(t, appDB, mA, func(ctx context.Context) {
		r, err := svc.CreateRule(ctx, alerting.CreateRuleInput{
			Name:     "cb",
			Template: "chargeback_rate_by_rail_account",
			Params:   map[string]any{"threshold": 0.1},
		})
		require.NoError(t, err)
		require.Equal(t, alerting.SeverityCritical, r.Severity) // template default
		require.Len(t, r.Channels, 2)                           // in_app + email
		ruleA = r
	})

	// Merchant B sees none of A's rules (RLS).
	inConn(t, appDB, mB, func(ctx context.Context) {
		rules, err := svc.ListRules(ctx)
		require.NoError(t, err)
		require.Empty(t, rules)
		_, err = svc.GetRule(ctx, ruleA.ID)
		require.Error(t, err) // cross-merchant id invisible
		ok, err := svc.DeleteRule(ctx, ruleA.ID)
		require.NoError(t, err)
		require.False(t, ok)
	})

	// A can list + update + delete its own.
	inConn(t, appDB, mA, func(ctx context.Context) {
		rules, err := svc.ListRules(ctx)
		require.NoError(t, err)
		require.Len(t, rules, 1)
		off := false
		upd, err := svc.UpdateRule(ctx, ruleA.ID, alerting.UpdateRuleInput{Enabled: &off})
		require.NoError(t, err)
		require.False(t, upd.Enabled)
		ok, err := svc.DeleteRule(ctx, ruleA.ID)
		require.NoError(t, err)
		require.True(t, ok)
	})
}

func TestCreateRuleValidation(t *testing.T) {
	pool, appDB := rlsSetup(t)
	mA := uuid.New()
	seedMerchant(t, pool, mA)
	svc := newService(appDB, nil)

	inConn(t, appDB, mA, func(ctx context.Context) {
		// unknown template
		_, err := svc.CreateRule(ctx, alerting.CreateRuleInput{Template: "nope"})
		var ve *alerting.ValidationError
		require.ErrorAs(t, err, &ve)

		// threshold out of range
		_, err = svc.CreateRule(ctx, alerting.CreateRuleInput{
			Template: "chargeback_rate_by_rail_account", Params: map[string]any{"threshold": 5},
		})
		require.ErrorAs(t, err, &ve)

		// webhook channel referencing a non-existent webhook
		bad := uuid.New()
		_, err = svc.CreateRule(ctx, alerting.CreateRuleInput{
			Template: "chargeback_rate_by_rail_account", Params: map[string]any{"threshold": 0.1},
			Channels: []alerting.ChannelRef{{Type: alerting.ChannelWebhook, WebhookID: &bad}},
		})
		require.ErrorAs(t, err, &ve)
	})
}

func TestEvaluatorEdgeTrigger(t *testing.T) {
	pool, appDB := rlsSetup(t)
	mid := uuid.New()
	seedMerchant(t, pool, mid)
	_, payID := seedChargebackBase(t, pool, mid)
	svc := newService(appDB, nil)

	// A rule that fires when chargeback rate >= 0.1 (in_app only, no email/webhook).
	inConn(t, appDB, mid, func(ctx context.Context) {
		_, err := svc.CreateRule(ctx, alerting.CreateRuleInput{
			Name:     "cb-guard",
			Template: "chargeback_rate_by_rail_account",
			Params:   map[string]any{"threshold": 0.1, "window": "30d"},
			Channels: []alerting.ChannelRef{{Type: alerting.ChannelInApp}},
		})
		require.NoError(t, err)
	})

	// No chargebacks yet → rate 0 → no fire.
	stats, err := svc.EvaluateMerchant(mctx(mid))
	require.NoError(t, err)
	require.Equal(t, 0, stats.Fired)
	require.Equal(t, 0, countNotifications(t, pool, mid))

	// Add a chargeback → rate 0.5 → FIRE once.
	addChargeback(t, pool, mid, payID)
	stats, err = svc.EvaluateMerchant(mctx(mid))
	require.NoError(t, err)
	require.Equal(t, 1, stats.Fired)
	require.Equal(t, 1, countNotifications(t, pool, mid))

	// Still crossing → stays quiet (no re-fire, no new notification).
	stats, err = svc.EvaluateMerchant(mctx(mid))
	require.NoError(t, err)
	require.Equal(t, 0, stats.Fired)
	require.Equal(t, 1, countNotifications(t, pool, mid))

	// Recross under threshold → CLEAR.
	removeChargebacks(t, pool, mid)
	stats, err = svc.EvaluateMerchant(mctx(mid))
	require.NoError(t, err)
	require.Equal(t, 1, stats.Cleared)
	require.Equal(t, 1, countNotifications(t, pool, mid))

	// Cross again → RE-FIRE (second notification).
	addChargeback(t, pool, mid, payID)
	stats, err = svc.EvaluateMerchant(mctx(mid))
	require.NoError(t, err)
	require.Equal(t, 1, stats.Fired)
	require.Equal(t, 2, countNotifications(t, pool, mid))

	// The notification carries severity + a dashboard link + offending dim.
	inConn(t, appDB, mid, func(ctx context.Context) {
		notes, err := svc.ListNotifications(ctx, false)
		require.NoError(t, err)
		require.NotEmpty(t, notes)
		require.Equal(t, alerting.SeverityCritical, notes[0].Severity)
		require.Contains(t, notes[0].Link, "chargeback_rate")
		require.Contains(t, notes[0].Body, "945280-0000")
	})
}

func TestTestFireChannelsAndShaping(t *testing.T) {
	pool, appDB := rlsSetup(t)
	mid := uuid.New()
	seedMerchant(t, pool, mid)
	exec(t, pool, `INSERT INTO openrails.merchant_configurations (merchant_id, config, created_at, updated_at) VALUES ($1, $2, now(), now())`,
		mid, []byte(`{"alert_email":"ops@example.com"}`))

	generic := newWebhookRecorder(t, 0)
	discord := newWebhookRecorder(t, 0)
	slack := newWebhookRecorder(t, 0)
	email := &fakeEmail{enabled: true}
	svc := newService(appDB, email)

	var results []alerting.DeliveryResult
	inConn(t, appDB, mid, func(ctx context.Context) {
		mkWebhook := func(format, url string) uuid.UUID {
			w, err := svc.CreateWebhook(ctx, alerting.CreateWebhookInput{Name: format, URL: url, Format: alerting.WebhookFormat(format)})
			require.NoError(t, err)
			return w.ID
		}
		gID := mkWebhook("generic", generic.server.URL)
		dID := mkWebhook("discord", discord.server.URL)
		sID := mkWebhook("slack", slack.server.URL)

		rule, err := svc.CreateRule(ctx, alerting.CreateRuleInput{
			Name:     "route-test",
			Template: "chargeback_rate_by_rail_account",
			Params:   map[string]any{"threshold": 0.1},
			Severity: alerting.SeverityCritical,
			Channels: []alerting.ChannelRef{
				{Type: alerting.ChannelInApp},
				{Type: alerting.ChannelEmail},
				{Type: alerting.ChannelWebhook, WebhookID: &gID},
				{Type: alerting.ChannelWebhook, WebhookID: &dID},
				{Type: alerting.ChannelWebhook, WebhookID: &sID},
			},
		})
		require.NoError(t, err)
		results, err = svc.TestFireRule(ctx, rule.ID)
		require.NoError(t, err)
	})

	// Every channel succeeded.
	require.Len(t, results, 5)
	for _, r := range results {
		require.True(t, r.OK, "channel %s failed: %s", r.Channel, r.Detail)
	}
	require.Equal(t, 1, email.count())
	require.Equal(t, 1, generic.callCount())
	require.Equal(t, 1, discord.callCount())
	require.Equal(t, 1, slack.callCount())

	// Format shaping.
	require.Contains(t, generic.lastBody(), "rule_name") // our alert JSON
	require.Contains(t, discord.lastBody(), "content")   // discord shape
	require.Contains(t, slack.lastBody(), "text")        // slack shape
	// A test alert is clearly marked.
	require.Contains(t, discord.lastBody()["content"].(string), "[TEST]")

	// In-app test notification landed too.
	require.Equal(t, 1, countNotifications(t, pool, mid))
}

func TestWebhookRetry(t *testing.T) {
	pool, appDB := rlsSetup(t)
	mid := uuid.New()
	seedMerchant(t, pool, mid)
	svc := newService(appDB, nil)

	recovering := newWebhookRecorder(t, 2) // fail twice, then 200
	failing := newWebhookRecorder(t, 99)   // always fail

	var results []alerting.DeliveryResult
	inConn(t, appDB, mid, func(ctx context.Context) {
		rID, err := svc.CreateWebhook(ctx, alerting.CreateWebhookInput{Name: "recovering", URL: recovering.server.URL, Format: alerting.FormatGeneric})
		require.NoError(t, err)
		fID, err := svc.CreateWebhook(ctx, alerting.CreateWebhookInput{Name: "failing", URL: failing.server.URL, Format: alerting.FormatGeneric})
		require.NoError(t, err)
		rule, err := svc.CreateRule(ctx, alerting.CreateRuleInput{
			Template: "chargeback_rate_by_rail_account", Params: map[string]any{"threshold": 0.1},
			Channels: []alerting.ChannelRef{
				{Type: alerting.ChannelWebhook, WebhookID: &rID.ID},
				{Type: alerting.ChannelWebhook, WebhookID: &fID.ID},
			},
		})
		require.NoError(t, err)
		results, err = svc.TestFireRule(ctx, rule.ID)
		require.NoError(t, err)
	})

	require.Len(t, results, 2)
	// Recovering: succeeds on the 3rd attempt.
	require.True(t, results[0].OK)
	require.Equal(t, 3, results[0].Attempts)
	require.Equal(t, 3, recovering.callCount())
	// Failing: exhausts 3 attempts, records failure.
	require.False(t, results[1].OK)
	require.Equal(t, 3, results[1].Attempts)
	require.Equal(t, 3, failing.callCount())
}

func TestEmailFailSoft(t *testing.T) {
	pool, appDB := rlsSetup(t)
	mid := uuid.New()
	seedMerchant(t, pool, mid)

	// Case 1: alert_email configured but NO sender → fail-soft skip.
	exec(t, pool, `INSERT INTO openrails.merchant_configurations (merchant_id, config, created_at, updated_at) VALUES ($1, $2, now(), now())`,
		mid, []byte(`{"alert_email":"ops@example.com"}`))
	svcNoSender := newService(appDB, nil)
	var res1 []alerting.DeliveryResult
	inConn(t, appDB, mid, func(ctx context.Context) {
		rule, err := svcNoSender.CreateRule(ctx, alerting.CreateRuleInput{
			Template: "chargeback_rate_by_rail_account", Params: map[string]any{"threshold": 0.1},
			Channels: []alerting.ChannelRef{{Type: alerting.ChannelInApp}, {Type: alerting.ChannelEmail}},
		})
		require.NoError(t, err)
		res1, err = svcNoSender.TestFireRule(ctx, rule.ID)
		require.NoError(t, err)
	})
	require.Len(t, res1, 2)
	require.True(t, res1[0].OK)  // in_app still delivered
	require.False(t, res1[1].OK) // email failed soft
	require.Contains(t, res1[1].Detail, "sender not configured")

	// Case 2: sender present but NO alert_email → fail-soft skip.
	exec(t, pool, `UPDATE openrails.merchant_configurations SET config = '{}'::jsonb WHERE merchant_id = $1`, mid)
	svcSender := newService(appDB, &fakeEmail{enabled: true})
	var res2 []alerting.DeliveryResult
	inConn(t, appDB, mid, func(ctx context.Context) {
		rule, err := svcSender.CreateRule(ctx, alerting.CreateRuleInput{
			Template: "chargeback_rate_by_rail_account", Params: map[string]any{"threshold": 0.1},
			Channels: []alerting.ChannelRef{{Type: alerting.ChannelEmail}},
		})
		require.NoError(t, err)
		res2, err = svcSender.TestFireRule(ctx, rule.ID)
		require.NoError(t, err)
	})
	require.Len(t, res2, 1)
	require.False(t, res2[0].OK)
	require.Contains(t, res2[0].Detail, "no alert_email")
}

func TestNotificationsBell(t *testing.T) {
	pool, appDB := rlsSetup(t)
	mid := uuid.New()
	seedMerchant(t, pool, mid)
	svc := newService(appDB, nil)

	// Two test-fires produce two in_app notifications.
	inConn(t, appDB, mid, func(ctx context.Context) {
		rule, err := svc.CreateRule(ctx, alerting.CreateRuleInput{
			Template: "chargeback_rate_by_rail_account", Params: map[string]any{"threshold": 0.1},
			Channels: []alerting.ChannelRef{{Type: alerting.ChannelInApp}},
		})
		require.NoError(t, err)
		_, err = svc.TestFireRule(ctx, rule.ID)
		require.NoError(t, err)
		_, err = svc.TestFireRule(ctx, rule.ID)
		require.NoError(t, err)
	})

	inConn(t, appDB, mid, func(ctx context.Context) {
		count, err := svc.UnreadCount(ctx)
		require.NoError(t, err)
		require.Equal(t, int64(2), count)

		all, err := svc.ListNotifications(ctx, false)
		require.NoError(t, err)
		require.Len(t, all, 2)

		ok, err := svc.MarkNotificationRead(ctx, all[0].ID)
		require.NoError(t, err)
		require.True(t, ok)

		count, err = svc.UnreadCount(ctx)
		require.NoError(t, err)
		require.Equal(t, int64(1), count)

		unread, err := svc.ListNotifications(ctx, true)
		require.NoError(t, err)
		require.Len(t, unread, 1)
		require.Equal(t, all[1].ID, unread[0].ID)

		// Cross-merchant read isolation: a fresh merchant sees nothing.
		other := uuid.New()
		seedMerchant(t, pool, other)
		inConn(t, appDB, other, func(ctx context.Context) {
			c, err := svc.UnreadCount(ctx)
			require.NoError(t, err)
			require.Equal(t, int64(0), c)
		})
	})
}

func TestArmedMerchantSelection(t *testing.T) {
	// Cross-merchant enumeration runs on the base pool (BYPASSRLS admin DSN),
	// matching the deployment posture of the #358 intent executor sweeps.
	appDB := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	cfg, err := pgxpool.ParseConfig(dbtest.MerchantPinnedDSN(t, dbtest.TestMerchantID.UUID()))
	require.NoError(t, err)
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	armed := uuid.New()
	disabledOnly := uuid.New()
	seedMerchant(t, pool, armed)
	seedMerchant(t, pool, disabledOnly)
	svc := newService(appDB, nil)

	inConn(t, appDB, armed, func(ctx context.Context) {
		_, err := svc.CreateRule(ctx, alerting.CreateRuleInput{
			Template: "chargeback_rate_by_rail_account", Params: map[string]any{"threshold": 0.1},
			Channels: []alerting.ChannelRef{{Type: alerting.ChannelInApp}},
		})
		require.NoError(t, err)
	})
	inConn(t, appDB, disabledOnly, func(ctx context.Context) {
		off := false
		_, err := svc.CreateRule(ctx, alerting.CreateRuleInput{
			Template: "chargeback_rate_by_rail_account", Params: map[string]any{"threshold": 0.1},
			Channels: []alerting.ChannelRef{{Type: alerting.ChannelInApp}}, Enabled: &off,
		})
		require.NoError(t, err)
	})

	ids, err := svc.ArmedMerchantIDs(context.Background())
	require.NoError(t, err)
	set := map[uuid.UUID]bool{}
	for _, id := range ids {
		set[id] = true
	}
	require.True(t, set[armed], "merchant with an enabled rule must be armed")
	require.False(t, set[disabledOnly], "merchant with only a disabled rule must NOT be armed")
}
