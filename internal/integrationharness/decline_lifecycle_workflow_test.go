//go:build integration

package integrationharness

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/collection"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	embcp "github.com/open-rails/openrails/internal/operator"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/open-rails/openrails/pkg/merchant"
)

// FailMembership is the shared production lifecycle boundary for already
// observed declines. Provider receipt qualification remains in the intent
// workflows; this table owns access, retry, notification and deletion effects.
func TestDeclineLifecyclePreservesCustomerInstruments(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("lifecycle recording must enqueue, not perform provider I/O: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(500)
	}))
	t.Cleanup(gateway.Close)
	h := New(t, t.Context())
	surface := h.StartStandalone("USD", WithClock(clockwork.NewFakeClockAt(now)), WithConfig(func(c *config.Config) {
		c.MerchantSource = config.MerchantSourceAPI
		c.SecretBackend = config.SecretBackendDB
		c.ProviderWriteMode = config.ProviderWriteModeLimited
		c.ProviderSandbox = &config.ProviderSandboxConfig{NMIGatewayURL: gateway.URL}
	}))
	owned := surface.ProvisionOwnedMerchant("decline-" + uuid.NewString()[:8])
	client := surface.Client(openrails.WithAPIKey(owned.APIKey), openrails.WithMerchantID(owned.MerchantID))
	product, err := client.CreateProduct(t.Context(), openrails.CreateProductRequest{Key: "decline", DisplayName: "Recurring access", EntitlementsSpec: map[string]*int{"decline_access": nil}})
	require.NoError(t, err)
	hours := 720
	price, err := client.CreatePrice(t.Context(), openrails.CreatePriceRequest{ProductID: product.ID, UnitAmount: 9_990_000, Currency: "USD", AutoRenew: true, AccessDurationHours: &hours})
	require.NoError(t, err)
	rt := surface.App().Runtime
	SeedPSPs(t.Context(), t, rt, owned.MerchantID, config.PSPSet{"nmi": {Rail: "nmi", AccountID: "decline-" + uuid.NewString(), NMI: &config.NMIRailConfig{SecurityKey: "synthetic", WebhookSigningSecret: "synthetic"}}})
	pool := h.MerchantPool(owned.MerchantID.UUID())
	t.Cleanup(func() {
		// Keep later fleet tests from executing this completed fixture's
		// deliberately queued provider work against their endpoint config.
		for _, statement := range []string{
			"DELETE FROM billing.rail_intents WHERE merchant_id=$1",
			"UPDATE billing.subscriptions SET deleted_at=now() WHERE merchant_id=$1",
			"UPDATE billing.psps SET archived=true WHERE merchant_id=$1",
		} {
			_, err := pool.Exec(context.Background(), statement, owned.MerchantID.UUID())
			require.NoError(t, err)
		}
	})
	psp := dbtest.EnsureTestPSP(t.Context(), t, pool, owned.MerchantID.UUID(), "nmi")
	ctx := db.WithPSPID(merchant.WithID(t.Context(), owned.MerchantID), psp)
	type subject struct{ customer, subscription, method uuid.UUID }
	seed := func(t *testing.T, retries int) subject {
		t.Helper()
		suffix := uuid.NewString()[:8]
		user, err := embcp.Get(surface.App()).Core().CreateUser(t.Context(), suffix+"@example.test", "decline"+suffix)
		require.NoError(t, err)
		customer := openrails.CustomerID(uuid.MustParse(user.ID))
		_, err = client.EnsureCustomer(t.Context(), customer)
		require.NoError(t, err)
		s := subject{customer.UUID(), uuid.New(), uuid.New()}
		_, err = pool.Exec(ctx, `INSERT INTO billing.payment_methods(id,merchant_id,customer_id,psp_id,rail,custodian,rail_customer_ref,initial_transaction_id) VALUES($1,$2,$3,$4,'nmi','psp',$5,'')`, s.method, owned.MerchantID.UUID(), s.customer, psp, "vault-"+s.method.String())
		require.NoError(t, err)
		end := now.Add(-48 * time.Hour)
		_, err = pool.Exec(ctx, `INSERT INTO billing.subscriptions(id,merchant_id,customer_id,product_id,price_id,psp_id,payment_method_id,rail,rail_subscription_id,status,current_period_starts_at,current_period_ends_at,next_retry_at,retry_attempts) VALUES($1,$2,$3,$4,$5,$6,$7,'nmi',$8,'past_due',$9,$10,$11,$12)`, s.subscription, owned.MerchantID.UUID(), s.customer, product.ID.UUID(), price.ID.UUID(), psp, s.method, "provider-"+s.subscription.String(), end.Add(-720*time.Hour), end, now.Add(-time.Hour), retries)
		require.NoError(t, err)
		_, err = rt.EntitlementService.PushNewEntitlement(ctx, entitlements.PushNewEntitlementParams{UserID: customer.String(), Entitlement: "decline_access", Indefinite: true, SourceType: models.EntitlementSourceSubscription, SourceID: s.subscription})
		require.NoError(t, err)
		active, err := client.HasEntitlement(t.Context(), customer, "decline_access", now)
		require.NoError(t, err)
		require.True(t, active, "the seeded subscription begins with standing access")
		return s
	}
	fail := func(t *testing.T, s subject, code string) {
		t.Helper()
		outcome := collection.ClassifyDecline("nmi", code)
		certainty := ""
		if outcome == collection.DeclineNonRecoverable {
			certainty = collection.CertaintyNonRetryableDecline
		}
		reason := "observed decline"
		require.NoError(t, rt.SubscriptionLifecycleService.FailMembership(ctx, &subscriptions.FailMembershipParams{Rail: models.RailNMI, SubscriptionID: &s.subscription, FailureReason: &reason, FailureCode: &code, Decline: outcome, RecordFailedAttempt: true, TerminalCertainty: certainty}))
	}
	inspect := func(t *testing.T, s subject, want string, notify models.NotificationEventType) {
		t.Helper()
		var state string
		var cancelType *string
		var next, deleted *time.Time
		var attempts int
		require.NoError(t, pool.QueryRow(ctx, `SELECT status,next_retry_at,deletion_scheduled_at,COALESCE(retry_attempts,0),cancel_type FROM billing.subscriptions WHERE id=$1`, s.subscription).Scan(&state, &next, &deleted, &attempts, &cancelType))
		require.Equal(t, want, state)
		var card bool
		var cardDeletes, scheduleDeletes int
		require.NoError(t, pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM billing.payment_methods WHERE id=$1)`, s.method).Scan(&card))
		require.True(t, card)
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM billing.rail_intents WHERE intent_type='nmi_vault_delete' AND idempotency_key=$1`, intents.NMIPaymentMethodDeleteIdempotencyKey(s.method)).Scan(&cardDeletes))
		require.Zero(t, cardDeletes)
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM billing.rail_intents WHERE intent_type='nmi_delete_subscription' AND subscription_id=$1`, s.subscription).Scan(&scheduleDeletes))
		var names []string
		rows, err := pool.Query(ctx, `SELECT event_type FROM billing.notifications WHERE customer_id=$1`, s.customer)
		require.NoError(t, err)
		for rows.Next() {
			var name string
			require.NoError(t, rows.Scan(&name))
			names = append(names, name)
		}
		rows.Close()
		require.NoError(t, rows.Err())
		require.Contains(t, names, string(notify))
		switch want {
		case "unknown":
			require.Nil(t, next)
			require.Nil(t, deleted)
			require.Zero(t, scheduleDeletes)
		case "past_due":
			require.NotNil(t, next)
			require.True(t, next.After(now))
			require.GreaterOrEqual(t, attempts, 1)
			require.Zero(t, scheduleDeletes)
			require.NotContains(t, names, string(models.NotificationPremiumEnded))
		case "cancelled":
			require.NotNil(t, cancelType)
			require.Equal(t, string(models.CancelTypeExpired), *cancelType)
			require.Nil(t, next)
			require.NotNil(t, deleted)
			require.Equal(t, 1, scheduleDeletes)
			var origin string
			require.NoError(t, pool.QueryRow(ctx, `SELECT origin FROM billing.rail_intents WHERE intent_type='nmi_delete_subscription' AND subscription_id=$1`, s.subscription).Scan(&origin))
			require.Equal(t, string(intents.OriginSystem), origin)
		}
		entitled, err := client.HasEntitlement(t.Context(), openrails.CustomerID(s.customer), "decline_access", now)
		require.NoError(t, err)
		require.Equal(t, want != "cancelled", entitled)
	}
	for _, row := range []struct {
		name, code, state string
		notification      models.NotificationEventType
	}{
		{"fixable card", "223", "unknown", models.NotificationPaymentMethodUpdateRequired},
		{"withdrawn mandate", "261", "cancelled", models.NotificationPremiumEnded},
		{"unknown evidence", "999", "past_due", models.NotificationPaymentMethodFailed},
	} {
		t.Run(row.name, func(t *testing.T) { s := seed(t, 0); fail(t, s, row.code); inspect(t, s, row.state, row.notification) })
	}
	t.Run("retry ladder", func(t *testing.T) {
		s := seed(t, 0)
		fail(t, s, "202")
		inspect(t, s, "past_due", models.NotificationPaymentMethodFailed)
		for n := 1; n < collection.MaxFailures(hours); n++ {
			fail(t, s, "202")
		}
		inspect(t, s, "cancelled", models.NotificationPremiumEnded)
	})
	t.Run("user cancellation and worker resume", func(t *testing.T) {
		s := seed(t, 0)
		end := now.Add(20 * 24 * time.Hour)
		_, err := pool.Exec(ctx, `UPDATE billing.subscriptions SET status='active',current_period_ends_at=$2,next_retry_at=NULL,retry_attempts=NULL WHERE id=$1`, s.subscription, end)
		require.NoError(t, err)
		require.NoError(t, rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
			return rt.UserSubscriptionService.CancelUserSubscription(ctx, s.customer.String(), "requested")
		}))
		var status, origin string
		var scheduled, next time.Time
		require.NoError(t, pool.QueryRow(ctx, `SELECT deletion_scheduled_at FROM billing.subscriptions WHERE id=$1`, s.subscription).Scan(&scheduled))
		require.NoError(t, pool.QueryRow(ctx, `SELECT status,origin,next_attempt_at FROM billing.rail_intents WHERE subscription_id=$1 AND intent_type='nmi_delete_subscription'`, s.subscription).Scan(&status, &origin, &next))
		require.Equal(t, intents.StatusPending, status)
		require.Equal(t, string(intents.OriginUser), origin)
		require.True(t, scheduled.Equal(next))
		require.True(t, next.After(now))
		require.True(t, next.Before(end))
		worker := riverjobs.ResumeSubscriptionWorker{DB: rt.DB, Config: rt.Config, Clock: rt.Clock, EntitlementService: rt.EntitlementService, SubscriptionService: rt.SubscriptionService, SubscriptionLifecycleService: rt.SubscriptionLifecycleService}
		job := &river.Job[riverjobs.ResumeSubscriptionArgs]{Args: riverjobs.ResumeSubscriptionArgs{MerchantID: owned.MerchantID.UUID(), UserID: s.customer.String(), SubscriptionID: s.subscription}}
		require.NoError(t, worker.Work(t.Context(), job))
		var deleted *time.Time
		require.NoError(t, pool.QueryRow(ctx, `SELECT status,deletion_scheduled_at FROM billing.subscriptions WHERE id=$1`, s.subscription).Scan(&status, &deleted))
		require.Equal(t, "active", status)
		require.Nil(t, deleted)
		require.NoError(t, pool.QueryRow(ctx, `SELECT status FROM billing.rail_intents WHERE subscription_id=$1 AND intent_type='nmi_delete_subscription'`, s.subscription).Scan(&status))
		require.Equal(t, intents.StatusSuperseded, status)
		entitled, err := client.HasEntitlement(t.Context(), openrails.CustomerID(s.customer), "decline_access", now)
		require.NoError(t, err)
		require.True(t, entitled)
		// Invisible requested work must fail, rather than reporting a completed job.
		job.Args.SubscriptionID = uuid.New()
		require.Error(t, worker.Work(t.Context(), job))
		job.Args.MerchantID = uuid.Nil
		require.Error(t, worker.Work(t.Context(), job))
	})

}
