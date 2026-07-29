//go:build integration

package riverjobs

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	riverpgxv5 "github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/modules/webhooks"
	"github.com/open-rails/openrails/internal/shared/sigverify"
)

// #684: burst coalescing through the REAL River pipeline. N webhook wake-ups
// about one subscription (any order, duplicated) must collapse into ONE
// provider fetch: the enqueuer's unique-per-subscription debounced job, worked
// by the real client, converging through the real decider against a fake
// Stripe transport (an httptest server speaking the Stripe wire shape).

type convergeFakeStripe struct {
	mu       sync.Mutex
	body     string
	requests atomic.Int64
	srv      *httptest.Server
}

func newConvergeFakeStripe(t *testing.T) *convergeFakeStripe {
	t.Helper()
	f := &convergeFakeStripe{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		f.mu.Lock()
		body := f.body
		f.mu.Unlock()
		if body == "" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"No such subscription"}}`))
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func TestSubscriptionConvergeBurstCoalescesToOneFetch(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbtest.SharedMerchantPool(t, dbtest.TestMerchantID.UUID())
	merchantID := dbtest.TestMerchantID.UUID()
	baseCtx := dbtest.WithTestMerchant(context.Background())
	dbtest.EnsureTestMerchant(baseCtx, t, dbtest.SharedMerchantPool(t, dbtest.TestMerchantID.UUID()))

	// ---- seed one active Stripe subscription mid-renewal --------------------
	suffix := uuid.NewString()[:8]
	productID, priceID, subID := uuid.New(), uuid.New(), uuid.New()
	railSubID := "sub_burst_" + suffix
	now := time.Now().UTC().Truncate(time.Second)
	oldEnd := now.Add(-1 * time.Hour) // boundary just lapsed; Stripe billed it
	newEnd := now.Add(30 * 24 * time.Hour)
	var customer uuid.UUID

	require.NoError(t, dbi.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		customer = dbtest.EnsureCustomerIDPgx(ctx, t, dbi.Qx(ctx), uuid.NewString())
		exec := func(sql string, args ...any) {
			_, err := dbi.Qx(ctx).Exec(ctx, sql, args...)
			require.NoError(t, err)
		}
		exec(`INSERT INTO openrails.products (id, key, display_name, entitlements_spec, merchant_id) VALUES ($1,$2,$2,'{}'::jsonb,$3)`,
			productID, "burst-prod-"+suffix, merchantID)
		exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, access_duration_hours, auto_renew, merchant_id) VALUES ($1,$2,29990000,'USD',720,true,$3)`,
			priceID, productID, merchantID)
		exec(`INSERT INTO openrails.subscriptions (id, price_id, product_id, status, rail, rail_subscription_id, current_period_starts_at, current_period_ends_at, started_at, entitlements_spec_snapshot, customer_id, merchant_id)
		      VALUES ($1,$2,$3,'active','stripe',$4,$5,$6,$5,'{}'::jsonb,$7,$8)`,
			subID, priceID, productID, railSubID, now.Add(-25*24*time.Hour), oldEnd, customer, merchantID)
		return nil
	}))

	t.Cleanup(func() {
		_ = dbi.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			_, _ = dbi.Qx(ctx).Exec(ctx, `DELETE FROM openrails.payments WHERE subscription_id=$1`, subID)
			_, _ = dbi.Qx(ctx).Exec(ctx, `DELETE FROM openrails.subscriptions WHERE id=$1`, subID)
			_, _ = dbi.Qx(ctx).Exec(ctx, `DELETE FROM openrails.prices WHERE id=$1`, priceID)
			_, _ = dbi.Qx(ctx).Exec(ctx, `DELETE FROM openrails.products WHERE id=$1`, productID)
			return nil
		})
		_, _ = pool.Exec(context.Background(), `DELETE FROM river_job WHERE kind=$1 AND args->>'subscription_reference'=$2`,
			KindSubscriptionConverge, railSubID)
	})

	// ---- provider truth: renewed, one paid invoice --------------------------
	fake := newConvergeFakeStripe(t)
	truth := map[string]any{
		"id":                   railSubID,
		"status":               "active",
		"current_period_start": oldEnd.Unix(),
		"current_period_end":   newEnd.Unix(),
		"customer":             "cus_burst",
		"latest_invoice": map[string]any{
			"id": "in_burst_" + suffix, "paid": true, "amount_paid": 2999,
			"charge": "ch_burst_" + suffix, "currency": "usd", "created": oldEnd.Unix(),
		},
	}
	truthJSON, err := json.Marshal(truth)
	require.NoError(t, err)
	fake.mu.Lock()
	fake.body = string(truthJSON)
	fake.mu.Unlock()

	// ---- the real worker on a real client ------------------------------------
	worker := buildConvergeWorker(dbi, fake.srv.URL)
	workers := river.NewWorkers()
	require.NoError(t, river.AddWorkerSafely(workers, worker))
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues:            map[string]river.QueueConfig{QueueBilling: {MaxWorkers: 4}},
		Workers:           workers,
		FetchCooldown:     10 * time.Millisecond,
		FetchPollInterval: 50 * time.Millisecond,
	})
	require.NoError(t, err)
	events, cancelSub := client.Subscribe(river.EventKindJobCompleted, river.EventKindJobFailed)
	t.Cleanup(cancelSub)

	ctx := context.Background()
	require.NoError(t, client.Start(ctx))
	t.Cleanup(func() {
		stopCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		_ = client.Stop(stopCtx)
	})

	// ---- N wake-ups within the debounce window ⇒ ONE job, ONE fetch ---------
	enq := &SubscriptionConvergeEnqueuer{Client: client, Debounce: 500 * time.Millisecond}
	for i := 0; i < 5; i++ {
		require.NoError(t, enq.EnqueueSubscriptionConverge(ctx, webhooks.ConvergeRequest{
			MerchantID:            merchantID,
			Rail:                  "stripe",
			SubscriptionReference: railSubID,
			EventType:             fmt.Sprintf("event_%d", i),
			EventCreated:          time.Now().Unix() + int64(i),
		}))
	}

	// Exactly one job for this subscription exists in the queue.
	var queued int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM river_job WHERE kind=$1 AND args->>'subscription_reference'=$2 AND state IN ('available','scheduled','pending','running','retryable')`,
		KindSubscriptionConverge, railSubID).Scan(&queued))
	require.Equal(t, 1, queued, "a burst of 5 wake-ups must coalesce into one queued converge job")

	// Await the single completion.
	deadline := time.After(20 * time.Second)
	for done := false; !done; {
		select {
		case ev := <-events:
			if ev.Job.Kind == KindSubscriptionConverge {
				require.Equal(t, river.EventKindJobCompleted, ev.Kind, "converge job failed: %s", string(ev.Job.EncodedArgs))
				done = true
			}
		case <-deadline:
			t.Fatal("timed out waiting for the converge job to complete")
		}
	}

	require.EqualValues(t, 1, fake.requests.Load(), "5 wake-ups must produce exactly 1 provider fetch")

	// Converged: the renewal landed once, from fetched truth.
	require.NoError(t, dbi.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		var status string
		var periodEnd time.Time
		require.NoError(t, dbi.Qx(ctx).QueryRow(ctx,
			`SELECT status::text, current_period_ends_at FROM openrails.subscriptions WHERE id=$1`, subID).
			Scan(&status, &periodEnd))
		require.Equal(t, "active", status)
		require.True(t, periodEnd.UTC().Equal(newEnd), "period end %v, want %v", periodEnd, newEnd)

		var paymentCount int
		require.NoError(t, dbi.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM openrails.payments WHERE subscription_id=$1 AND status='completed'`, subID).
			Scan(&paymentCount))
		require.Equal(t, 1, paymentCount, "renewal payment recorded exactly once")
		return nil
	}))
}

// buildConvergeWorker wires a SubscriptionConvergeWorker from real services
// over the given DB, with the Stripe prober pointed at the fake transport.
func buildConvergeWorker(dbi *db.DB, stripeBaseURL string) *SubscriptionConvergeWorker {
	priceSvc := catalog.NewPriceService(dbi)
	productSvc := catalog.NewProductService(dbi)
	entitlementSvc := entitlements.NewEntitlementService(dbi)
	paymentSvc := payments.NewPaymentService(dbi)
	subscriptionSvc := subscriptions.NewSubscriptionService(dbi, priceSvc, productSvc, nil, nil, nil)
	lifecycleSvc := subscriptions.NewSubscriptionLifecycleService(dbi, productSvc, priceSvc, entitlementSvc, nil, paymentSvc)
	return &SubscriptionConvergeWorker{
		DB:                           dbi,
		StripeProber:                 &subscriptions.HTTPStripeLivenessProber{SecretKey: "sk_test_fake", BaseURL: stripeBaseURL},
		PriceService:                 priceSvc,
		ProductService:               productSvc,
		SubscriptionService:          subscriptionSvc,
		SubscriptionLifecycleService: lifecycleSvc,
		PaymentService:               paymentSvc,
	}
}

// ---------------------------------------------------------------------------
// End-to-end (#684 acceptance e): a REAL-signature webhook in → identity-only
// handler → dedup Begin/Complete (Postgres truth) → coalesced fetch job →
// decider convergence → subscription + payment row in Postgres. One Stripe and
// one NMI renewal, against fake provider TRANSPORTS (httptest servers speaking
// the real wire shapes). Signature verification is the real code path
// (webhookutil.Prepare*): a bad signature is rejected before anything runs.
// (The HTTP mux → handler routing above this seam is covered by
// internal/http/routes_merchant_webhook_integration_test.go.)
// ---------------------------------------------------------------------------

func stripeE2ESig(secret, ts string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "." + string(body)))
	return fmt.Sprintf("t=%s,v1=%s", ts, hex.EncodeToString(mac.Sum(nil)))
}

func nmiE2ESig(secret, ts string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "." + string(body)))
	return fmt.Sprintf("t=%s,s=%s", ts, hex.EncodeToString(mac.Sum(nil)))
}

type convergeE2EFixture struct {
	dbi        *db.DB
	merchantID uuid.UUID
	customer   uuid.UUID
	productID  uuid.UUID
	priceID    uuid.UUID
	subID      uuid.UUID
	railSubID  string
	oldEnd     time.Time
	newEnd     time.Time
}

func seedConvergeE2ESubscription(t *testing.T, dbi *db.DB, baseCtx context.Context, rail, railSubID string) *convergeE2EFixture {
	t.Helper()
	suffix := uuid.NewString()[:8]
	now := time.Now().UTC().Truncate(time.Second)
	f := &convergeE2EFixture{
		dbi:        dbi,
		merchantID: dbtest.TestMerchantID.UUID(),
		productID:  uuid.New(),
		priceID:    uuid.New(),
		subID:      uuid.New(),
		railSubID:  railSubID,
		oldEnd:     now.Add(-1 * time.Hour),
		newEnd:     now.Add(30 * 24 * time.Hour),
	}
	require.NoError(t, dbi.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		f.customer = dbtest.EnsureCustomerIDPgx(ctx, t, dbi.Qx(ctx), uuid.NewString())
		exec := func(sql string, args ...any) {
			_, err := dbi.Qx(ctx).Exec(ctx, sql, args...)
			require.NoError(t, err)
		}
		exec(`INSERT INTO openrails.products (id, key, display_name, entitlements_spec, merchant_id) VALUES ($1,$2,$2,'{}'::jsonb,$3)`,
			f.productID, "e2e-prod-"+suffix, f.merchantID)
		exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, access_duration_hours, auto_renew, merchant_id) VALUES ($1,$2,29990000,'USD',720,true,$3)`,
			f.priceID, f.productID, f.merchantID)
		exec(`INSERT INTO openrails.subscriptions (id, price_id, product_id, status, rail, rail_subscription_id, current_period_starts_at, current_period_ends_at, started_at, entitlements_spec_snapshot, customer_id, merchant_id)
		      VALUES ($1,$2,$3,'active',$4,$5,$6,$7,$6,'{}'::jsonb,$8,$9)`,
			f.subID, f.priceID, f.productID, rail, railSubID, f.oldEnd.Add(-30*24*time.Hour), f.oldEnd, f.customer, f.merchantID)
		return nil
	}))
	t.Cleanup(func() {
		_ = dbi.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			_, _ = dbi.Qx(ctx).Exec(ctx, `DELETE FROM openrails.webhook_events WHERE merchant_id=$1`, f.merchantID)
			_, _ = dbi.Qx(ctx).Exec(ctx, `DELETE FROM openrails.rail_customer_accounts WHERE customer_id=$1`, f.customer)
			_, _ = dbi.Qx(ctx).Exec(ctx, `DELETE FROM openrails.payments WHERE subscription_id=$1`, f.subID)
			_, _ = dbi.Qx(ctx).Exec(ctx, `DELETE FROM openrails.subscriptions WHERE id=$1`, f.subID)
			_, _ = dbi.Qx(ctx).Exec(ctx, `DELETE FROM openrails.prices WHERE id=$1`, f.priceID)
			_, _ = dbi.Qx(ctx).Exec(ctx, `DELETE FROM openrails.products WHERE id=$1`, f.productID)
			return nil
		})
		_, _ = dbtest.SharedPGXPool(t).Exec(context.Background(),
			`DELETE FROM river_job WHERE kind=$1 AND args->>'subscription_reference'=$2`, KindSubscriptionConverge, f.railSubID)
	})
	return f
}

func (f *convergeE2EFixture) assertConverged(t *testing.T, baseCtx context.Context, wantTxn string, wantEnd time.Time) {
	t.Helper()
	require.NoError(t, f.dbi.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		var status string
		var periodEnd time.Time
		require.NoError(t, f.dbi.Qx(ctx).QueryRow(ctx,
			`SELECT status::text, current_period_ends_at FROM openrails.subscriptions WHERE id=$1`, f.subID).
			Scan(&status, &periodEnd))
		require.Equal(t, "active", status)
		require.True(t, periodEnd.UTC().Equal(wantEnd), "period end %v, want %v", periodEnd.UTC(), wantEnd)

		var txn string
		require.NoError(t, f.dbi.Qx(ctx).QueryRow(ctx,
			`SELECT transaction_id FROM openrails.payments WHERE subscription_id=$1 AND status='completed'`, f.subID).
			Scan(&txn))
		require.Equal(t, wantTxn, txn)
		return nil
	}))
}

func awaitConvergeCompletion(t *testing.T, events <-chan *river.Event) {
	t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		select {
		case ev := <-events:
			if ev.Job.Kind != KindSubscriptionConverge {
				continue
			}
			require.Equal(t, river.EventKindJobCompleted, ev.Kind, "converge job failed: %s", string(ev.Job.EncodedArgs))
			return
		case <-deadline:
			t.Fatal("timed out waiting for the converge job")
		}
	}
}

func TestWebhookWakeUpEndToEnd_StripeRenewal(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbtest.SharedMerchantPool(t, dbtest.TestMerchantID.UUID())
	baseCtx := dbtest.WithTestMerchant(context.Background())
	dbtest.EnsureTestMerchant(baseCtx, t, dbtest.SharedMerchantPool(t, dbtest.TestMerchantID.UUID()))

	f := seedConvergeE2ESubscription(t, dbi, baseCtx, "stripe", "sub_e2e_"+uuid.NewString()[:8])

	// Provider truth: renewed, one paid invoice at the lapsed boundary.
	fake := newConvergeFakeStripe(t)
	chargeID := "ch_e2e_" + uuid.NewString()[:8]
	truthJSON, err := json.Marshal(map[string]any{
		"id": f.railSubID, "status": "active",
		"current_period_start": f.oldEnd.Unix(), "current_period_end": f.newEnd.Unix(),
		"customer": "cus_e2e_" + uuid.NewString()[:8],
		"latest_invoice": map[string]any{
			"id": "in_e2e", "paid": true, "amount_paid": 2999, "charge": chargeID,
			"currency": "usd", "created": f.oldEnd.Unix(),
		},
	})
	require.NoError(t, err)
	fake.mu.Lock()
	fake.body = string(truthJSON)
	fake.mu.Unlock()

	// Real river client + worker.
	worker := buildConvergeWorker(dbi, fake.srv.URL)
	workers := river.NewWorkers()
	require.NoError(t, river.AddWorkerSafely(workers, worker))
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues:            map[string]river.QueueConfig{QueueBilling: {MaxWorkers: 2}},
		Workers:           workers,
		FetchCooldown:     10 * time.Millisecond,
		FetchPollInterval: 50 * time.Millisecond,
	})
	require.NoError(t, err)
	events, cancelSub := client.Subscribe(river.EventKindJobCompleted, river.EventKindJobFailed)
	t.Cleanup(cancelSub)
	require.NoError(t, client.Start(context.Background()))
	t.Cleanup(func() {
		stopCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		_ = client.Stop(stopCtx)
	})

	// The inbound event: REAL signature over the raw bytes, verified by the
	// same webhookutil path the HTTP handler uses. The payload carries ONLY
	// identity the handler needs — its amounts/periods are never applied.
	secret := "whsec_e2e_test"
	body := []byte(fmt.Sprintf(`{"id":"evt_e2e_1","type":"invoice.paid","created":%d,"data":{"object":{"id":"in_e2e","subscription":%q,"amount_paid":999999,"currency":"usd"}}}`,
		time.Now().Unix(), f.railSubID))
	ts := strconv.FormatInt(time.Now().Unix(), 10)

	// REAL signature verification (the same canonical verifier the HTTP
	// ingestion path delegates to): a bad signature is rejected before
	// anything runs.
	sigHeader := stripeE2ESig(secret, ts, body)
	require.Error(t, sigverify.VerifyStripe(secret, stripeE2ESig("whsec_evil", ts, body), body, 5*time.Minute),
		"invalid signature must be rejected")
	require.NoError(t, sigverify.VerifyStripe(secret, sigHeader, body, 5*time.Minute))

	dispatcher := &webhooks.WebhookDispatcher{
		DB:                   dbi,
		DeduplicationService: webhooks.NewDeduplicationService(nil, dbi),
		ConvergeEnqueuer:     &SubscriptionConvergeEnqueuer{Client: client, Debounce: 200 * time.Millisecond},
	}
	verified := true
	msg := &webhooks.WebhookMessage{
		Rail:           "stripe",
		EventID:        "evt_e2e_1",
		EventType:      "invoice.paid",
		Payload:        body,
		Signature:      sigHeader,
		SignatureValid: &verified,
		ReceivedAt:     time.Now(),
	}
	require.NoError(t, dispatcher.Process(baseCtx, msg))

	awaitConvergeCompletion(t, events)
	require.EqualValues(t, 1, fake.requests.Load(), "one wake-up, one fetch")
	f.assertConverged(t, baseCtx, chargeID, f.newEnd)

	// Dedup truth: the event is durably marked processed in Postgres.
	require.NoError(t, dbi.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		var n int
		require.NoError(t, dbi.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM openrails.webhook_events WHERE event_id='evt_e2e_1'`).Scan(&n))
		require.Equal(t, 1, n)
		return nil
	}))
}

func TestWebhookWakeUpEndToEnd_NMIRenewal(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbtest.SharedMerchantPool(t, dbtest.TestMerchantID.UUID())
	baseCtx := dbtest.WithTestMerchant(context.Background())
	dbtest.EnsureTestMerchant(baseCtx, t, dbtest.SharedMerchantPool(t, dbtest.TestMerchantID.UUID()))

	f := seedConvergeE2ESubscription(t, dbi, baseCtx, "nmi", "nmi_e2e_"+uuid.NewString()[:8])

	// Fake NMI: query.php reports the renewal sale for the local order
	// reference; the v5 GET reports the record alive with the next boundary.
	txnID := "txn_e2e_" + uuid.NewString()[:8]
	chargedAt := time.Now().UTC().Add(-30 * time.Minute)
	saleXML := fmt.Sprintf(`<?xml version="1.0"?>
<nm_response><transaction><transaction_id>%s</transaction_id><order_id>%s</order_id><currency>usd</currency>
<action><amount>29.99</amount><action_type>sale</action_type><success>1</success><date>%s</date></action>
</transaction></nm_response>`, txnID, f.subID.String(), chargedAt.Format("20060102150405"))
	subJSON := fmt.Sprintf(`{"object":"subscription","id":"%s","next_billing_date":"%s"}`,
		f.railSubID, f.newEnd.Format("2006-01-02"))
	nmiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(subJSON))
			return
		}
		_ = r.ParseForm()
		if r.Form.Get("report_type") == "" {
			t.Error("fetch-and-converge must never send a provider mutation")
			_, _ = w.Write([]byte("response=1"))
			return
		}
		_, _ = w.Write([]byte(saleXML))
	}))
	t.Cleanup(nmiSrv.Close)

	nmiClient, err := nmi.NewClient("nmi", &config.NMIProviderSettings{SecurityKey: "k", WebhookSecret: "s"}, true)
	require.NoError(t, err)
	nmiClient.DirectPostURL = nmiSrv.URL
	nmiClient.QueryURL = nmiSrv.URL
	nmiClient.V5BaseURL = nmiSrv.URL

	worker := buildConvergeWorker(dbi, "")
	worker.StripeProber = nil
	worker.NMIResolver = fakeDunningNMIResolver{client: nmiClient}
	workers := river.NewWorkers()
	require.NoError(t, river.AddWorkerSafely(workers, worker))
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues:            map[string]river.QueueConfig{QueueBilling: {MaxWorkers: 2}},
		Workers:           workers,
		FetchCooldown:     10 * time.Millisecond,
		FetchPollInterval: 50 * time.Millisecond,
	})
	require.NoError(t, err)
	events, cancelSub := client.Subscribe(river.EventKindJobCompleted, river.EventKindJobFailed)
	t.Cleanup(cancelSub)
	require.NoError(t, client.Start(context.Background()))
	t.Cleanup(func() {
		stopCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		_ = client.Stop(stopCtx)
	})

	// Inbound NMI event with a REAL signature; payload amounts are decoys —
	// only the identity may be read.
	secret := "nmi_e2e_secret"
	body := []byte(fmt.Sprintf(`{"event_id":"evt_nmi_e2e_1","event_type":"transaction.sale.success","event_body":{"subscription":{"subscription_id":%q},"transaction_id":"decoy_txn","amount":"9999.99","currency":"usd"}}`, f.railSubID))
	ts := strconv.FormatInt(time.Now().Unix(), 10)

	nmiHeader := nmiE2ESig(secret, ts, body)
	require.Error(t, sigverify.VerifyNMI(secret, nmiE2ESig("evil", ts, body), body, 5*time.Minute),
		"invalid signature must be rejected")
	require.NoError(t, sigverify.VerifyNMI(secret, nmiHeader, body, 5*time.Minute))

	dispatcher := &webhooks.WebhookDispatcher{
		DB:                   dbi,
		DeduplicationService: webhooks.NewDeduplicationService(nil, dbi),
		ConvergeEnqueuer:     &SubscriptionConvergeEnqueuer{Client: client, Debounce: 200 * time.Millisecond},
	}
	verified := true
	msg := &webhooks.WebhookMessage{
		Rail:           "nmi",
		EventID:        "evt_nmi_e2e_1",
		EventType:      "transaction.sale.success",
		Payload:        body,
		Signature:      nmiHeader,
		SigningSecret:  secret,
		SignatureValid: &verified,
		ReceivedAt:     time.Now(),
	}
	// The NMI dispatcher leg RE-VERIFIES the signature itself (handler.Verify).
	require.NoError(t, dispatcher.Process(baseCtx, msg))

	awaitConvergeCompletion(t, events)
	// NMI v5 declares next billing as a bare DATE — the adopted period end is
	// that day's UTC midnight.
	wantEnd, err := time.ParseInLocation("2006-01-02", f.newEnd.Format("2006-01-02"), time.UTC)
	require.NoError(t, err)
	f.assertConverged(t, baseCtx, txnID, wantEnd)
}
