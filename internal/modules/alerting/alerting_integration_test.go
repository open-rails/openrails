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

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/merchantsecrets"
	"github.com/open-rails/openrails/internal/modules/alerting"
	"github.com/open-rails/openrails/internal/reconcile"
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
	exec(t, pool, `INSERT INTO billing.merchants (id, slug, status) VALUES ($1,$2,'active') ON CONFLICT (id) DO NOTHING`, mid, slug)
	t.Cleanup(func() {
		ctx := context.Background()
		for _, tbl := range []string{"notifications", "merchant_webhooks", "payments", "subscriptions", "prices", "products", "customers", "psps", "merchant_configurations", "webhook_health", "webhook_health_daily", "reconciliation_findings", "maintenance_runs"} {
			_, _ = pool.Exec(ctx, `DELETE FROM billing.`+tbl+` WHERE merchant_id = $1`, mid)
		}
		_, _ = pool.Exec(ctx, `DELETE FROM billing.merchants WHERE id = $1`, mid)
	})
}

func newService(t *testing.T, appDB *db.DB, email alerting.EmailSender) *alerting.Service {
	t.Helper()
	backend, err := merchantsecrets.Build(context.Background(), &config.Config{Env: "dev", MerchantConfigSource: config.MerchantConfigSourceAPI, SecretBackend: config.SecretBackendDB, Encryption: &config.EncryptionConfig{MasterKey: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="}}, appDB.DataPool())
	require.NoError(t, err)
	return alerting.NewService(alerting.Deps{
		DB:      appDB,
		Secrets: backend.Secrets,
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
		`SELECT count(*) FROM billing.notifications WHERE merchant_id=$1`, mid).Scan(&n))
	return n
}

func TestFindingDeliveryRetainsWebhookFormatsRetriesAndBell(t *testing.T) {
	pool, appDB := rlsSetup(t)
	mid, other := uuid.New(), uuid.New()
	seedMerchant(t, pool, mid)
	seedMerchant(t, pool, other)
	exec(t, pool, `INSERT INTO billing.merchant_configurations (merchant_id, config) VALUES ($1, $2)`, mid, []byte(`{"alert_email":"ops@example.com"}`))
	email := &fakeEmail{enabled: true}
	svc := newService(t, appDB, email)
	generic, discord, slack, disabled := newWebhookRecorder(t, 2), newWebhookRecorder(t, 0), newWebhookRecorder(t, 0), newWebhookRecorder(t, 0)
	inConn(t, appDB, mid, func(ctx context.Context) {
		for format, sink := range map[alerting.WebhookFormat]*webhookRecorder{alerting.FormatGeneric: generic, alerting.FormatDiscord: discord, alerting.FormatSlack: slack} {
			_, err := svc.CreateWebhook(ctx, alerting.CreateWebhookInput{Name: string(format), URL: sink.server.URL, Format: format})
			require.NoError(t, err)
		}
		off := false
		_, err := svc.CreateWebhook(ctx, alerting.CreateWebhookInput{Name: "disabled", URL: disabled.server.URL, Enabled: &off})
		require.NoError(t, err)
	})
	findingStore := &reconcile.PGStore{DB: appDB}
	inConn(t, appDB, mid, func(ctx context.Context) {
		run, err := findingStore.CreateRun(ctx, reconcile.ModeAdvisory, []reconcile.Provider{reconcile.ProviderNMI}, nil, nil)
		require.NoError(t, err)
		for _, severity := range []reconcile.Severity{reconcile.SeverityLow, reconcile.SeverityCritical} {
			rec, err := findingStore.UpsertFinding(ctx, run, reconcile.Finding{Provider: reconcile.ProviderNMI, Type: reconcile.FindingChargebackActiveSub, SubjectKey: uuid.NewString(), Severity: severity, Status: reconcile.FindingStatusRequiresReview})
			require.NoError(t, err)
			require.NoError(t, svc.NotifyFinding(ctx, rec))
			rec, err = findingStore.GetFinding(ctx, rec.ID)
			require.NoError(t, err)
			require.NoError(t, svc.NotifyFinding(ctx, rec))
		}
		notes, err := svc.ListNotifications(ctx, true)
		require.NoError(t, err)
		require.Len(t, notes, 2, "low findings retain one deduplicated console notification")
		read, err := svc.MarkNotificationRead(ctx, notes[0].ID)
		require.NoError(t, err)
		require.True(t, read)
		unread, err := svc.UnreadCount(ctx)
		require.NoError(t, err)
		require.EqualValues(t, 1, unread)
	})
	require.Equal(t, 1, email.count(), "only the critical finding emails")
	require.Equal(t, 3, generic.callCount(), "two failures retry before successful delivery")
	require.Equal(t, 1, discord.callCount())
	require.Equal(t, 1, slack.callCount())
	require.Zero(t, disabled.callCount())
	require.Contains(t, generic.lastBody(), "title")
	require.NotContains(t, generic.lastBody(), "rule_id")
	require.Contains(t, discord.lastBody(), "content")
	require.Contains(t, slack.lastBody(), "text")
	require.Equal(t, 2, countNotifications(t, pool, mid))
	inConn(t, appDB, other, func(ctx context.Context) {
		hooks, err := svc.ListWebhooks(ctx)
		require.NoError(t, err)
		require.Empty(t, hooks)
		notes, err := svc.ListNotifications(ctx, false)
		require.NoError(t, err)
		require.Empty(t, notes)
	})
}
