//go:build integration

package intents

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// #696 integration: user CCBill cancel → local runway cancel + durable
// ccbill_cancel_subscription intent (atomic) → executor drains it through the
// DataLink SMS choke point against a fake HTTP server (the ONLY fake).

// fakeSMS scripts subscriptionManagement.cgi: viewSubscriptionStatus answers
// the scripted status, cancelSubscription answers the scripted body/HTTP code.
type fakeSMS struct {
	status      atomic.Value // string subscriptionStatus ("2" rebilling, "1"/"0" not)
	cancelBody  atomic.Value // string cancel response (default <results>1</results>)
	cancelHTTP  atomic.Int64 // != 0 forces an HTTP status on cancel
	viewCalls   atomic.Int64
	cancelCalls atomic.Int64
}

func newFakeSMS(t *testing.T, status string) (*fakeSMS, *ccbill.DataLinkClient) {
	t.Helper()
	f := &fakeSMS{}
	f.status.Store(status)
	f.cancelBody.Store(`<results>1</results>`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		switch r.PostForm.Get("action") {
		case "viewSubscriptionStatus":
			f.viewCalls.Add(1)
			fmt.Fprintf(w, `<results><subscriptionStatus>%s</subscriptionStatus><expirationDate>%s</expirationDate></results>`,
				f.status.Load().(string), time.Now().UTC().Add(20*24*time.Hour).Format("20060102"))
		case "cancelSubscription":
			f.cancelCalls.Add(1)
			if st := f.cancelHTTP.Load(); st != 0 {
				w.WriteHeader(int(st))
				return
			}
			f.status.Store("1") // provider-side: no further rebills after a cancel
			fmt.Fprint(w, f.cancelBody.Load().(string))
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)
	client := &ccbill.DataLinkClient{
		BaseURL: srv.URL, ClientAccNum: "900100", ClientSubAcc: "0000",
		Username: "dluser", Password: "dlpass", HTTPClient: srv.Client(),
	}
	return f, client
}

type ccbillFixture struct {
	db     *db.DB
	store  *Store
	subID  uuid.UUID
	psid   string
	userID uuid.UUID
}

// seedCCBillSubscription seeds an ACTIVE ccbill subscription with a future
// paid period (the shape a user cancels).
func seedCCBillSubscription(t *testing.T) ccbillFixture {
	t.Helper()
	ctx := dbtest.WithTestMerchant(context.Background())
	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()

	fx := ccbillFixture{db: dbi, store: NewStore(dbi)}
	fx.subID = uuid.New()
	fx.psid = "cc-" + uuid.NewString()[:8]
	fx.userID = dbtest.EnsureCustomerIDPgx(ctx, t, pool, uuid.NewString())

	productID, priceID := uuid.New(), uuid.New()
	suffix := uuid.NewString()[:8]
	now := time.Now().UTC()
	tenantID := dbtest.TestMerchantID.UUID()

	exec := func(sql string, args ...any) {
		t.Helper()
		_, err := pool.Exec(ctx, sql, args...)
		require.NoError(t, err)
	}
	exec(`INSERT INTO openrails.products (id, key, display_name, merchant_id) VALUES ($1, $2, $2, $3)`,
		productID, "ccbill-prod-"+suffix, tenantID)
	exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, access_duration_hours, auto_renew, merchant_id)
	      VALUES ($1, $2, 999, 'usd', 720, true, $3)`, priceID, productID, tenantID)
	exec(`INSERT INTO openrails.subscriptions
	        (id, price_id, product_id, status, rail, rail_subscription_id,
	         current_period_starts_at, current_period_ends_at, started_at, customer_id, merchant_id)
	      VALUES ($1, $2, $3, 'active', 'ccbill', $4, $5, $6, $5, $7, $8)`,
		fx.subID, priceID, productID, fx.psid,
		now.Add(-10*24*time.Hour), now.Add(20*24*time.Hour), fx.userID, tenantID)

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.rail_mutation_logs
			WHERE rail_intent_id IN (SELECT id FROM openrails.rail_intents WHERE subscription_id = $1)`, fx.subID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.rail_intents WHERE subscription_id = $1", fx.subID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.notification_queue WHERE customer_id = $1", fx.userID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.entitlements WHERE customer_id = $1", fx.userID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.subscriptions WHERE id = $1", fx.subID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})
	return fx
}

func (fx ccbillFixture) userService() *subscriptions.UserSubscriptionService {
	subSvc := subscriptions.NewSubscriptionService(fx.db,
		catalog.NewPriceService(fx.db), catalog.NewProductService(fx.db), nil, nil, nil)
	usvc := subscriptions.NewUserSubscriptionService(subSvc,
		catalog.NewProductService(fx.db), catalog.NewPriceService(fx.db), nil,
		subscriptions.NewNotificationService(fx.db, nil),
		entitlements.NewEntitlementService(fx.db), nil)
	usvc.SetCCBillCancelScheduler(NewCCBillCancelScheduler(fx.db, nil, OriginUser, "test: user ccbill cancel"))
	return usvc
}

func (fx ccbillFixture) runner(client *ccbill.DataLinkClient, cfg *config.Config) *Runner {
	return &Runner{
		Store:    fx.store,
		Registry: NewRegistry(NewCCBillCancelHandler(fx.db, client, nil)),
		Config:   cfg,
	}
}

func (fx ccbillFixture) intentRow(t *testing.T) (id uuid.UUID, status, origin string, lastFailure *string) {
	t.Helper()
	require.NoError(t, fx.db.Pool().QueryRow(context.Background(),
		`SELECT id, status, origin, last_failure_reason FROM openrails.rail_intents
		 WHERE subscription_id = $1 AND intent_type = $2`, fx.subID, TypeCCBillCancelSubscription).
		Scan(&id, &status, &origin, &lastFailure))
	return id, status, origin, lastFailure
}

func (fx ccbillFixture) forceDue(t *testing.T, id uuid.UUID) {
	t.Helper()
	_, err := fx.db.Pool().Exec(context.Background(),
		`UPDATE openrails.rail_intents SET next_attempt_at = now() WHERE id = $1`, id)
	require.NoError(t, err)
}

// User cancels a ccbill sub: ONE call records the local runway cancel AND the
// pending user-origin intent atomically; the executor then drains it against
// the fake DataLink server and the verify-then-execute discipline holds.
func TestCCBillUserCancelQueuesAndExecutes(t *testing.T) {
	ctx := dbtest.WithTestMerchant(context.Background())
	fx := seedCCBillSubscription(t)
	fake, client := newFakeSMS(t, "2") // rebilling at the provider

	require.NoError(t, fx.userService().CancelUserSubscription(ctx, fx.userID.String(), "test feedback"))

	// Local decision durable: cancelled (paid runway preserved — period end
	// untouched), user cancel provenance, NO deletion marker (not resumable).
	var subStatus, cancelType string
	var periodEnd time.Time
	var marker *time.Time
	require.NoError(t, fx.db.Pool().QueryRow(ctx,
		`SELECT status, cancel_type, current_period_ends_at, deletion_scheduled_at
		   FROM openrails.subscriptions WHERE id = $1`, fx.subID).
		Scan(&subStatus, &cancelType, &periodEnd, &marker))
	assert.Equal(t, "cancelled", subStatus)
	assert.Equal(t, "user", cancelType)
	assert.True(t, periodEnd.After(time.Now()), "paid runway preserved (#691)")
	assert.Nil(t, marker, "ccbill cancels carry no NMI deletion marker")

	intentID, status, origin, _ := fx.intentRow(t)
	assert.Equal(t, StatusPending, status)
	assert.Equal(t, string(OriginUser), origin)
	assert.Zero(t, fake.cancelCalls.Load(), "enqueue never touches the provider")

	// Executor drains: view (rebilling) -> cancel -> success.
	_, err := fx.runner(client, fullModeConfig()).RunExecuteOnce(ctx)
	require.NoError(t, err)
	_, status, _, _ = fx.intentRow(t)
	assert.Equal(t, StatusSucceeded, status)
	assert.EqualValues(t, 1, fake.viewCalls.Load(), "verify-then-execute reads first")
	assert.EqualValues(t, 1, fake.cancelCalls.Load())
	_ = intentID

	// Re-running is a no-op (idempotent tombstone).
	_, err = fx.runner(client, fullModeConfig()).RunExecuteOnce(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 1, fake.cancelCalls.Load())
}

// mode=readonly: the intent still queues (queue-always #679) but the executor
// parks it with the mode reason and ZERO provider traffic.
func TestCCBillCancelReadonlyParksWithZeroHTTP(t *testing.T) {
	ctx := dbtest.WithTestMerchant(context.Background())
	fx := seedCCBillSubscription(t)
	fake, client := newFakeSMS(t, "2")
	client.ReadOnly = true // build_runtime sets this under mode=readonly

	require.NoError(t, fx.userService().CancelUserSubscription(ctx, fx.userID.String(), "test feedback"))
	intentID, status, _, _ := fx.intentRow(t)
	assert.Equal(t, StatusPending, status, "queue-always: readonly never blocks the enqueue")

	_, err := fx.runner(client, readonlyModeConfig()).RunExecuteOnce(ctx)
	require.NoError(t, err)
	_, status, _, lastFailure := fx.intentRow(t)
	assert.Equal(t, StatusPending, status, "readonly parks, never fails")
	require.NotNil(t, lastFailure)
	assert.Contains(t, *lastFailure, "mode=readonly")
	assert.Zero(t, fake.viewCalls.Load(), "zero HTTP under readonly")
	assert.Zero(t, fake.cancelCalls.Load())

	// Mode lifts -> the queue drains.
	client.ReadOnly = false
	fx.forceDue(t, intentID)
	_, err = fx.runner(client, fullModeConfig()).RunExecuteOnce(ctx)
	require.NoError(t, err)
	_, status, _, _ = fx.intentRow(t)
	assert.Equal(t, StatusSucceeded, status)
	assert.EqualValues(t, 1, fake.cancelCalls.Load())
}

// mode=limited: user-origin intents are reactive completions and still execute.
func TestCCBillCancelLimitedModeExecutesUserOrigin(t *testing.T) {
	ctx := dbtest.WithTestMerchant(context.Background())
	fx := seedCCBillSubscription(t)
	fake, client := newFakeSMS(t, "2")

	require.NoError(t, fx.userService().CancelUserSubscription(ctx, fx.userID.String(), "test feedback"))
	_, err := fx.runner(client, limitedModeConfig()).RunExecuteOnce(ctx)
	require.NoError(t, err)
	_, status, _, _ := fx.intentRow(t)
	assert.Equal(t, StatusSucceeded, status, "user-origin executes under limited")
	assert.EqualValues(t, 1, fake.cancelCalls.Load())
}

// Already-cancelled at the provider: the pre-write read answers status "1"
// (no future rebill) — idempotent success, no mutation sent.
func TestCCBillCancelAlreadyCancelledIsIdempotentSuccess(t *testing.T) {
	ctx := dbtest.WithTestMerchant(context.Background())
	fx := seedCCBillSubscription(t)
	fake, client := newFakeSMS(t, "1") // not rebilling

	require.NoError(t, fx.userService().CancelUserSubscription(ctx, fx.userID.String(), "test feedback"))
	_, err := fx.runner(client, fullModeConfig()).RunExecuteOnce(ctx)
	require.NoError(t, err)
	_, status, _, _ := fx.intentRow(t)
	assert.Equal(t, StatusSucceeded, status)
	assert.EqualValues(t, 1, fake.viewCalls.Load())
	assert.Zero(t, fake.cancelCalls.Load(), "verified not-rebilling: no mutation sent")
}

// Ambiguous cancel outcome (HTTP 500 after the send) parks as
// unknown_needs_verify; the verifier resolves via the provider READ.
func TestCCBillCancelAmbiguousThenVerifyResolves(t *testing.T) {
	ctx := dbtest.WithTestMerchant(context.Background())
	fx := seedCCBillSubscription(t)
	fake, client := newFakeSMS(t, "2")
	fake.cancelHTTP.Store(http.StatusInternalServerError)

	require.NoError(t, fx.userService().CancelUserSubscription(ctx, fx.userID.String(), "test feedback"))
	_, err := fx.runner(client, fullModeConfig()).RunExecuteOnce(ctx)
	require.NoError(t, err)
	intentID, status, _, lastFailure := fx.intentRow(t)
	assert.Equal(t, StatusUnknownNeedsVerify, status, "post-send failure may have executed: verify, never declare")
	require.NotNil(t, lastFailure)
	assert.EqualValues(t, 1, fake.cancelCalls.Load())

	// Provider truth: the cancel DID land (status now non-rebilling).
	fake.status.Store("1")
	fake.cancelHTTP.Store(0)
	fx.forceDue(t, intentID)
	_, err = fx.runner(client, fullModeConfig()).RunVerifyOnce(ctx)
	require.NoError(t, err)
	_, status, _, _ = fx.intentRow(t)
	assert.Equal(t, StatusSucceeded, status)
	assert.EqualValues(t, 1, fake.cancelCalls.Load(), "verification is read-only")
}

// A definite provider reject (results=0) is ALSO verify-not-decline: the
// verifier finds the sub still rebilling and hands it back for retry.
func TestCCBillCancelRejectVerifiesNotExecuted(t *testing.T) {
	ctx := dbtest.WithTestMerchant(context.Background())
	fx := seedCCBillSubscription(t)
	fake, client := newFakeSMS(t, "2")
	fake.cancelBody.Store(`<results>0</results>`)

	require.NoError(t, fx.userService().CancelUserSubscription(ctx, fx.userID.String(), "test feedback"))
	_, err := fx.runner(client, fullModeConfig()).RunExecuteOnce(ctx)
	require.NoError(t, err)
	intentID, status, _, _ := fx.intentRow(t)
	assert.Equal(t, StatusUnknownNeedsVerify, status)

	// The reject was real: still rebilling. Verify -> failed_retryable
	// ("verified not executed"), the executor may retry.
	fake.status.Store("2")
	fx.forceDue(t, intentID)
	_, err = fx.runner(client, fullModeConfig()).RunVerifyOnce(ctx)
	require.NoError(t, err)
	_, status, _, lastFailure := fx.intentRow(t)
	assert.Equal(t, StatusFailedRetryable, status)
	require.NotNil(t, lastFailure)
	assert.Contains(t, *lastFailure, "verified not executed")
}

// A reactivated subscription supersedes the queued cancel (relevance re-check
// at execution time — a stale cancel can never kill a live subscription).
func TestCCBillCancelSupersededByReactivation(t *testing.T) {
	ctx := dbtest.WithTestMerchant(context.Background())
	fx := seedCCBillSubscription(t)
	fake, client := newFakeSMS(t, "2")

	require.NoError(t, fx.userService().CancelUserSubscription(ctx, fx.userID.String(), "test feedback"))
	_, err := fx.db.Pool().Exec(ctx,
		`UPDATE openrails.subscriptions SET status = 'active', cancelled_at = NULL, cancel_type = NULL WHERE id = $1`, fx.subID)
	require.NoError(t, err)

	_, err = fx.runner(client, fullModeConfig()).RunExecuteOnce(ctx)
	require.NoError(t, err)
	_, status, _, _ := fx.intentRow(t)
	assert.Equal(t, StatusSuperseded, status)
	assert.Zero(t, fake.viewCalls.Load())
	assert.Zero(t, fake.cancelCalls.Load())
}

// #679/#696: ccbill cancels count toward the merchant's destructive budget —
// exactly the budget executes, the rest park behind ONE held_bulk finding.
func TestBreakerCountsCCBillCancelsTowardDestructiveBudget(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()
	store := NewStore(dbi)
	sfx := uuid.NewString()[:8]

	const over = 2
	mid := uuid.New()
	exec := func(sql string, args ...any) {
		t.Helper()
		_, err := pool.Exec(ctx, sql, args...)
		require.NoError(t, err)
	}
	exec(`INSERT INTO openrails.merchants (id, slug, status) VALUES ($1, $2, 'active')`, mid, "ccbreaker-"+sfx)
	productID, priceID := uuid.New(), uuid.New()
	exec(`INSERT INTO openrails.products (id, key, display_name, merchant_id) VALUES ($1, $2, $2, $3)`,
		productID, "ccbreaker-prod-"+sfx, mid)
	exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, access_duration_hours, auto_renew, merchant_id)
	      VALUES ($1, $2, 999, 'usd', 720, true, $3)`, priceID, productID, mid)
	now := time.Now().UTC()
	for i := 0; i < DestructiveBudgetFloor+over; i++ {
		subID := uuid.New()
		var custID uuid.UUID
		require.NoError(t, pool.QueryRow(ctx,
			`INSERT INTO openrails.customers (id, merchant_id) VALUES ($1, $2) RETURNING id`, uuid.New(), mid).Scan(&custID))
		psid := fmt.Sprintf("cc-%s-%d", sfx, i)
		exec(`INSERT INTO openrails.subscriptions
		        (id, price_id, product_id, status, rail, rail_subscription_id,
		         current_period_starts_at, current_period_ends_at, started_at,
		         cancelled_at, cancel_type, customer_id, merchant_id)
		      VALUES ($1, $2, $3, 'cancelled', 'ccbill', $4, $5, $6, $5, $7, 'user', $8, $9)`,
			subID, priceID, productID, psid,
			now.Add(-10*24*time.Hour), now.Add(20*24*time.Hour), now, custID, mid)
		_, err := store.Enqueue(ctx, EnqueueParams{
			MerchantID:     mid,
			Provider:       "ccbill",
			IntentType:     TypeCCBillCancelSubscription,
			SubscriptionID: &subID,
			Payload:        CCBillCancelPayload{UserID: custID.String(), RailSubscriptionID: psid},
			IdempotencyKey: CCBillCancelIdempotencyKey(subID),
			NextAttemptAt:  now.Add(-time.Minute),
			Origin:         OriginUser,
			OriginReason:   "ccbill breaker integration test",
		})
		require.NoError(t, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.rail_mutation_logs WHERE merchant_id = $1`, mid)
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.rail_intents WHERE merchant_id = $1`, mid)
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.reconciliation_findings WHERE merchant_id = $1`, mid)
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.subscriptions WHERE merchant_id = $1`, mid)
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.prices WHERE merchant_id = $1`, mid)
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.products WHERE merchant_id = $1`, mid)
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.customers WHERE merchant_id = $1`, mid)
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.merchants WHERE id = $1`, mid)
	})

	fake, client := newFakeSMS(t, "2")
	runner := &Runner{
		Store:    store,
		Registry: NewRegistry(NewCCBillCancelHandler(dbi, client, nil)),
		Config:   fullModeConfig(),
		Breaker:  NewVolumeBreaker(dbi),
	}
	_, err := runner.RunExecuteOnce(ctx)
	require.NoError(t, err)

	counts := map[string]int{}
	rows, err := pool.Query(ctx, `SELECT status, count(*) FROM openrails.rail_intents WHERE merchant_id = $1 GROUP BY status`, mid)
	require.NoError(t, err)
	for rows.Next() {
		var s string
		var n int
		require.NoError(t, rows.Scan(&s, &n))
		counts[s] = n
	}
	require.NoError(t, rows.Err())
	rows.Close()

	assert.Equal(t, DestructiveBudgetFloor, counts[StatusSucceeded], "exactly the budget executes")
	assert.Equal(t, over, counts[StatusPending], "over-budget ccbill cancels held pending")
	_ = fake

	var findingCount int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM openrails.reconciliation_findings
		 WHERE merchant_id = $1 AND finding_type = $2 AND status = 'requires_review'`, mid, HeldBulkFindingType).Scan(&findingCount))
	assert.Equal(t, 1, findingCount, "one held_bulk finding raised for the ccbill cohort")
}
