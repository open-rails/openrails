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
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/nmi"
)

// fakeNMI scripts the gateway: the Query API (recurring report) answers
// whether the subscription is "present", the Direct Post API answers the
// delete. Both record call counts.
type fakeNMI struct {
	present      atomic.Bool
	deleteBody   atomic.Value // string response for recurring=delete_subscription
	queryCalls   atomic.Int64
	deleteCalls  atomic.Int64
	deleteStatus atomic.Int64 // optional HTTP status for the delete (0 = 200)
	psid         string
}

func newFakeNMI(t *testing.T, psid string, present bool) (*fakeNMI, *nmi.NMIClient) {
	t.Helper()
	f := &fakeNMI{psid: psid}
	f.present.Store(present)
	f.deleteBody.Store("response=1&responsetext=SUCCESS")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("report_type") == "recurring" || r.URL.Query().Get("report_type") == "recurring" {
			f.queryCalls.Add(1)
			if f.present.Load() {
				fmt.Fprintf(w, `<nm_response><subscription><subscription_id>%s</subscription_id></subscription></nm_response>`, f.psid)
			} else {
				fmt.Fprint(w, `<nm_response></nm_response>`)
			}
			return
		}
		if r.Form.Get("recurring") == "delete_subscription" {
			f.deleteCalls.Add(1)
			if st := f.deleteStatus.Load(); st != 0 {
				w.WriteHeader(int(st))
				return
			}
			_, _ = w.Write([]byte(f.deleteBody.Load().(string)))
			return
		}
		_, _ = w.Write([]byte("response=1"))
	}))
	t.Cleanup(srv.Close)

	client, err := nmi.NewClient("mobius", &config.NMIProviderSettings{
		SecurityKey:   "test_security_key",
		WebhookSecret: "test_secret",
	}, true)
	require.NoError(t, err)
	client.DirectPostURL = srv.URL
	client.QueryURL = srv.URL
	return f, client
}

type intentFixture struct {
	db     *db.DB
	store  *Store
	subID  uuid.UUID
	psid   string
	userID uuid.UUID // tenant subject
}

// seedCancelledNMISubscription inserts a product/price/subscription where the
// subscription is cancelled with a pending DeletionScheduledAt marker — the
// state whose deferred delete the ledger owns.
func seedCancelledNMISubscription(t *testing.T, deletionScheduledAt time.Time) intentFixture {
	t.Helper()
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()

	fx := intentFixture{db: dbi, store: NewStore(dbi)}
	fx.subID = uuid.New()
	fx.psid = "psid-" + uuid.NewString()[:8]
	fx.userID = dbtest.EnsureMerchantSubjectIDPgx(ctx, t, pool, uuid.NewString())

	productID := uuid.New()
	priceID := uuid.New()
	suffix := uuid.NewString()[:8]
	now := time.Now().UTC()

	exec := func(sql string, args ...any) {
		t.Helper()
		_, err := pool.Exec(ctx, sql, args...)
		require.NoError(t, err)
	}
	tenantID := dbtest.TestTenantID.UUID()
	exec(`INSERT INTO openrails.products (id, slug, display_name, merchant_id) VALUES ($1, $2, $2, $3)`,
		productID, "intent-prod-"+suffix, tenantID)
	exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, billing_cycle_days, merchant_id)
	      VALUES ($1, $2, 999, 'usd', 30, $3)`, priceID, productID, tenantID)
	exec(`INSERT INTO openrails.subscriptions
	        (id, price_id, product_id, status, processor, processor_subscription_id,
	         current_period_starts_at, current_period_ends_at, started_at,
	         cancelled_at, cancel_type, deletion_scheduled_at, merchant_subject_id, merchant_id)
	      VALUES ($1, $2, $3, 'cancelled', 'mobius', $4, $5, $6, $5, $7, 'user', $8, $9, $10)`,
		fx.subID, priceID, productID, fx.psid,
		now.Add(-10*24*time.Hour), now.Add(20*24*time.Hour), now,
		deletionScheduledAt.UTC(), fx.userID, tenantID)

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.provider_intents WHERE subscription_id = $1", fx.subID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.subscriptions WHERE id = $1", fx.subID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})
	return fx
}

func (fx intentFixture) enqueueDelete(t *testing.T, origin Origin, dueAt time.Time) gen.OpenrailsProviderIntent {
	t.Helper()
	row, err := fx.store.Enqueue(context.Background(), EnqueueParams{
		MerchantID:       dbtest.TestTenantID.UUID(),
		Provider:       "mobius",
		IntentType:     TypeNMIDeleteSubscription,
		SubscriptionID: &fx.subID,
		Payload:        NMIDeletePayload{UserID: fx.userID.String(), ProcessorSubscriptionID: fx.psid},
		IdempotencyKey: NMIDeleteIdempotencyKey(fx.subID),
		NextAttemptAt:  dueAt,
		Origin:         origin,
		OriginReason:   "integration test",
	})
	require.NoError(t, err)
	return row
}

func (fx intentFixture) intent(t *testing.T, id uuid.UUID) gen.OpenrailsProviderIntent {
	t.Helper()
	row, err := fx.db.Gen(context.Background()).GetProviderIntent(context.Background(), id)
	require.NoError(t, err)
	return row
}

func (fx intentFixture) deletionMarker(t *testing.T) *time.Time {
	t.Helper()
	row, err := fx.db.Gen(context.Background()).GetSubscriptionByID(context.Background(), fx.subID)
	require.NoError(t, err)
	return row.DeletionScheduledAt
}

func (fx intentFixture) runner(client *nmi.NMIClient, cfg *config.Config) *Runner {
	return &Runner{
		Store: fx.store,
		Registry: NewRegistry(
			NewNMIDeleteHandler(fx.db, cfg, map[string]*nmi.NMIClient{"mobius": client}, nil),
		),
		Config: cfg,
	}
}

func fullModeConfig() *config.Config     { return &config.Config{Mode: config.ModeFull} }
func limitedModeConfig() *config.Config  { return &config.Config{Mode: config.ModeLimited} }
func readonlyModeConfig() *config.Config { return &config.Config{Mode: config.ModeReadOnly} }

// TestExecutorDeletesPresentSubscription: enqueue -> executor verifies the
// subscription exists at the provider, deletes it, succeeds, and clears the
// DeletionScheduledAt read model.
func TestExecutorDeletesPresentSubscription(t *testing.T) {
	fx := seedCancelledNMISubscription(t, time.Now().Add(-time.Minute))
	fake, client := newFakeNMI(t, fx.psid, true)
	row := fx.enqueueDelete(t, OriginUser, time.Now().Add(-time.Minute))

	stats, err := fx.runner(client, fullModeConfig()).RunExecuteOnce(context.Background())
	require.NoError(t, err)
	require.GreaterOrEqual(t, stats.Claimed, 1)

	got := fx.intent(t, row.ID)
	assert.Equal(t, StatusSucceeded, got.Status)
	require.NotNil(t, got.ExecutedAt)
	assert.Contains(t, string(got.ResultEvidence), `"deleted": true`)
	assert.EqualValues(t, 1, fake.queryCalls.Load(), "verify-then-execute reads first")
	assert.EqualValues(t, 1, fake.deleteCalls.Load())
	assert.Nil(t, fx.deletionMarker(t), "success finalizes the read model (marker cleared)")
}

// TestExecutorVerifiedAbsentIsSuccess: absent at the provider = success
// without sending a delete (deletes are idempotent by observation).
func TestExecutorVerifiedAbsentIsSuccess(t *testing.T) {
	fx := seedCancelledNMISubscription(t, time.Now().Add(-time.Minute))
	fake, client := newFakeNMI(t, fx.psid, false)
	row := fx.enqueueDelete(t, OriginUser, time.Now().Add(-time.Minute))

	_, err := fx.runner(client, fullModeConfig()).RunExecuteOnce(context.Background())
	require.NoError(t, err)

	got := fx.intent(t, row.ID)
	assert.Equal(t, StatusSucceeded, got.Status)
	assert.Contains(t, string(got.ResultEvidence), `"verified_absent": true`)
	assert.Zero(t, fake.deleteCalls.Load(), "nothing to delete; no write sent")
	assert.Nil(t, fx.deletionMarker(t))
}

// TestAmbiguousOutcomeRoutesThroughVerifier: a delete whose response is
// unreadable parks as unknown_needs_verify (never blind-completed); the
// verifier then resolves it via the provider read.
func TestAmbiguousOutcomeRoutesThroughVerifier(t *testing.T) {
	fx := seedCancelledNMISubscription(t, time.Now().Add(-time.Minute))
	fake, client := newFakeNMI(t, fx.psid, true)
	fake.deleteStatus.Store(http.StatusBadGateway) // transport-level failure mid-write
	row := fx.enqueueDelete(t, OriginUser, time.Now().Add(-time.Minute))

	_, err := fx.runner(client, fullModeConfig()).RunExecuteOnce(context.Background())
	require.NoError(t, err)

	got := fx.intent(t, row.ID)
	require.Equal(t, StatusUnknownNeedsVerify, got.Status, "a possibly-sent delete must verify, not retry blindly")
	require.NotNil(t, got.LastFailureReason)
	assert.Contains(t, *got.LastFailureReason, "delete_subscription failed")
	assert.NotNil(t, fx.deletionMarker(t), "marker survives until the outcome is known")

	// The delete actually landed at NMI: the next read shows it gone. Make
	// the intent due now and run the verifier.
	fake.present.Store(false)
	_, err = fx.db.Pool().Exec(context.Background(),
		"UPDATE openrails.provider_intents SET next_attempt_at = now() WHERE id = $1", row.ID)
	require.NoError(t, err)

	stats, err := fx.runner(client, fullModeConfig()).RunVerifyOnce(context.Background())
	require.NoError(t, err)
	require.GreaterOrEqual(t, stats.Claimed, 1)

	got = fx.intent(t, row.ID)
	assert.Equal(t, StatusSucceeded, got.Status)
	assert.Contains(t, string(got.ResultEvidence), `"verified_absent": true`)
	assert.Nil(t, fx.deletionMarker(t))
}

// TestVerifierStillPresentReturnsToExecutor: verify finds the subscription
// alive -> the delete definitely did not happen -> failed_retryable (the
// executor retries it; here with the gateway healthy it then succeeds).
func TestVerifierStillPresentReturnsToExecutor(t *testing.T) {
	fx := seedCancelledNMISubscription(t, time.Now().Add(-time.Minute))
	fake, client := newFakeNMI(t, fx.psid, true)
	fake.deleteStatus.Store(http.StatusBadGateway)
	row := fx.enqueueDelete(t, OriginUser, time.Now().Add(-time.Minute))

	_, err := fx.runner(client, fullModeConfig()).RunExecuteOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, StatusUnknownNeedsVerify, fx.intent(t, row.ID).Status)

	_, err = fx.db.Pool().Exec(context.Background(),
		"UPDATE openrails.provider_intents SET next_attempt_at = now() WHERE id = $1", row.ID)
	require.NoError(t, err)
	_, err = fx.runner(client, fullModeConfig()).RunVerifyOnce(context.Background())
	require.NoError(t, err)

	got := fx.intent(t, row.ID)
	require.Equal(t, StatusFailedRetryable, got.Status)
	require.NotNil(t, got.LastFailureReason)
	assert.Contains(t, *got.LastFailureReason, "still present")

	// Gateway recovers; pull the backoff in and re-run the executor.
	fake.deleteStatus.Store(0)
	_, err = fx.db.Pool().Exec(context.Background(),
		"UPDATE openrails.provider_intents SET next_attempt_at = now() WHERE id = $1", row.ID)
	require.NoError(t, err)
	_, err = fx.runner(client, fullModeConfig()).RunExecuteOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, StatusSucceeded, fx.intent(t, row.ID).Status)
}

// TestKillSwitchParksPending pins #344 on the ledger: kill switch on -> the
// intent stays PENDING with the reason recorded (not failed), attempts not
// burned; lifting the switch lets the next pass execute it.
func TestKillSwitchParksPending(t *testing.T) {
	fx := seedCancelledNMISubscription(t, time.Now().Add(-time.Minute))
	fake, client := newFakeNMI(t, fx.psid, true)
	client.SubscriptionDeletesDisabled = true
	row := fx.enqueueDelete(t, OriginUser, time.Now().Add(-time.Minute))

	_, err := fx.runner(client, fullModeConfig()).RunExecuteOnce(context.Background())
	require.NoError(t, err)

	got := fx.intent(t, row.ID)
	assert.Equal(t, StatusPending, got.Status, "kill switch parks, never fails")
	require.NotNil(t, got.LastFailureReason)
	assert.Contains(t, *got.LastFailureReason, "kill switch")
	assert.EqualValues(t, 0, got.Attempts, "a park does not burn an attempt")
	assert.Zero(t, fake.deleteCalls.Load())
	assert.Zero(t, fake.queryCalls.Load(), "parks before any provider traffic")
	assert.NotNil(t, fx.deletionMarker(t))

	// Switch lifts -> queue drains on the next due pass.
	client.SubscriptionDeletesDisabled = false
	_, err = fx.db.Pool().Exec(context.Background(),
		"UPDATE openrails.provider_intents SET next_attempt_at = now() WHERE id = $1", row.ID)
	require.NoError(t, err)
	_, err = fx.runner(client, fullModeConfig()).RunExecuteOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, StatusSucceeded, fx.intent(t, row.ID).Status)
}

// TestLimitedModeOriginGating: user-origin executes under limited (reactive
// completion), system-origin parks until full mode.
func TestLimitedModeOriginGating(t *testing.T) {
	t.Run("user origin executes", func(t *testing.T) {
		fx := seedCancelledNMISubscription(t, time.Now().Add(-time.Minute))
		_, client := newFakeNMI(t, fx.psid, true)
		row := fx.enqueueDelete(t, OriginUser, time.Now().Add(-time.Minute))

		_, err := fx.runner(client, limitedModeConfig()).RunExecuteOnce(context.Background())
		require.NoError(t, err)
		assert.Equal(t, StatusSucceeded, fx.intent(t, row.ID).Status)
	})

	t.Run("system origin parks", func(t *testing.T) {
		fx := seedCancelledNMISubscription(t, time.Now().Add(-time.Minute))
		fake, client := newFakeNMI(t, fx.psid, true)
		row := fx.enqueueDelete(t, OriginSystem, time.Now().Add(-time.Minute))

		_, err := fx.runner(client, limitedModeConfig()).RunExecuteOnce(context.Background())
		require.NoError(t, err)

		got := fx.intent(t, row.ID)
		assert.Equal(t, StatusPending, got.Status)
		require.NotNil(t, got.LastFailureReason)
		assert.Contains(t, *got.LastFailureReason, "mode=limited")
		assert.Zero(t, fake.deleteCalls.Load())

		// Mode lifts to full -> drains.
		_, err = fx.db.Pool().Exec(context.Background(),
			"UPDATE openrails.provider_intents SET next_attempt_at = now() WHERE id = $1", row.ID)
		require.NoError(t, err)
		_, err = fx.runner(client, fullModeConfig()).RunExecuteOnce(context.Background())
		require.NoError(t, err)
		assert.Equal(t, StatusSucceeded, fx.intent(t, row.ID).Status)
	})

	t.Run("readonly parks even user origin", func(t *testing.T) {
		fx := seedCancelledNMISubscription(t, time.Now().Add(-time.Minute))
		fake, client := newFakeNMI(t, fx.psid, true)
		row := fx.enqueueDelete(t, OriginUser, time.Now().Add(-time.Minute))

		_, err := fx.runner(client, readonlyModeConfig()).RunExecuteOnce(context.Background())
		require.NoError(t, err)

		got := fx.intent(t, row.ID)
		assert.Equal(t, StatusPending, got.Status)
		require.NotNil(t, got.LastFailureReason)
		assert.Contains(t, *got.LastFailureReason, "mode=readonly")
		assert.Zero(t, fake.deleteCalls.Load())
		assert.Zero(t, fake.queryCalls.Load(), "readonly attempts NOTHING")
	})
}

// TestResumeSupersedesAndRecancelRevives covers the cancel -> resume ->
// re-cancel lifecycle on the ledger: supersede-by-subject kills the pending
// intent, the executor's relevance check guards a racing resume, and a fresh
// cancel revives the SAME idempotency identity.
func TestResumeSupersedesAndRecancelRevives(t *testing.T) {
	fx := seedCancelledNMISubscription(t, time.Now().Add(2*time.Hour))
	fake, client := newFakeNMI(t, fx.psid, true)
	row := fx.enqueueDelete(t, OriginUser, time.Now().Add(2*time.Hour))

	// Resume: supersede by subject (what CancelNMIDelete / the resume worker do).
	n, err := fx.store.SupersedeBySubject(context.Background(), TypeNMIDeleteSubscription, fx.subID, "resumed")
	require.NoError(t, err)
	assert.EqualValues(t, 1, n)
	got := fx.intent(t, row.ID)
	assert.Equal(t, StatusSuperseded, got.Status)

	// Re-cancel: the enqueue revives the same row (one identity per
	// subscription, no duplicate intents).
	revived := fx.enqueueDelete(t, OriginUser, time.Now().Add(-time.Minute))
	assert.Equal(t, row.ID, revived.ID, "revive, not duplicate")
	assert.Equal(t, StatusPending, revived.Status)
	assert.EqualValues(t, 0, revived.Attempts, "revive resets attempt state")

	_, err = fx.runner(client, fullModeConfig()).RunExecuteOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, StatusSucceeded, fx.intent(t, row.ID).Status)
	assert.EqualValues(t, 1, fake.deleteCalls.Load())
}

// TestExecutorRelevanceSupersedesResumedSubscription: even without an
// explicit supersede, the executor re-reads the subscription and refuses to
// delete one that is active again (the authoritative resume guard).
func TestExecutorRelevanceSupersedesResumedSubscription(t *testing.T) {
	fx := seedCancelledNMISubscription(t, time.Now().Add(-time.Minute))
	fake, client := newFakeNMI(t, fx.psid, true)
	row := fx.enqueueDelete(t, OriginUser, time.Now().Add(-time.Minute))

	// Simulate the resume that missed its advisory supersede.
	_, err := fx.db.Pool().Exec(context.Background(),
		`UPDATE openrails.subscriptions
		 SET status = 'active', cancelled_at = NULL, cancel_type = NULL, deletion_scheduled_at = NULL
		 WHERE id = $1`, fx.subID)
	require.NoError(t, err)

	_, err = fx.runner(client, fullModeConfig()).RunExecuteOnce(context.Background())
	require.NoError(t, err)

	got := fx.intent(t, row.ID)
	assert.Equal(t, StatusSuperseded, got.Status)
	require.NotNil(t, got.LastFailureReason)
	assert.Contains(t, *got.LastFailureReason, "resumed or already finalized")
	assert.Zero(t, fake.deleteCalls.Load(), "a resumed subscription must never be deleted")
}

// TestIdempotentEnqueue: the same logical intent enqueued repeatedly is ONE
// row; a pending re-enqueue refreshes the schedule.
func TestIdempotentEnqueue(t *testing.T) {
	fx := seedCancelledNMISubscription(t, time.Now().Add(2*time.Hour))
	first := fx.enqueueDelete(t, OriginUser, time.Now().Add(2*time.Hour))
	laterDue := time.Now().Add(30 * time.Minute).UTC()
	second := fx.enqueueDelete(t, OriginUser, laterDue)

	assert.Equal(t, first.ID, second.ID, "one row per idempotency key")
	assert.WithinDuration(t, laterDue, second.NextAttemptAt, time.Second, "pending re-enqueue refreshes the schedule")

	var count int
	require.NoError(t, fx.db.Pool().QueryRow(context.Background(),
		"SELECT count(*) FROM openrails.provider_intents WHERE subscription_id = $1", fx.subID).Scan(&count))
	assert.Equal(t, 1, count)
}

// TestSucceededIntentIsNeverRevived: re-enqueueing an executed intent must
// not re-execute it (effectively-once).
func TestSucceededIntentIsNeverRevived(t *testing.T) {
	fx := seedCancelledNMISubscription(t, time.Now().Add(-time.Minute))
	fake, client := newFakeNMI(t, fx.psid, true)
	row := fx.enqueueDelete(t, OriginUser, time.Now().Add(-time.Minute))

	_, err := fx.runner(client, fullModeConfig()).RunExecuteOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, StatusSucceeded, fx.intent(t, row.ID).Status)

	again := fx.enqueueDelete(t, OriginUser, time.Now().Add(-time.Minute))
	assert.Equal(t, row.ID, again.ID)
	assert.Equal(t, StatusSucceeded, again.Status, "succeeded is immutable to enqueues")

	_, err = fx.runner(client, fullModeConfig()).RunExecuteOnce(context.Background())
	require.NoError(t, err)
	assert.EqualValues(t, 1, fake.deleteCalls.Load(), "no second delete")
}

// TestRelevanceWindowExpiry: an intent past its expires_at is expired by the
// executor pass instead of firing stale.
func TestRelevanceWindowExpiry(t *testing.T) {
	fx := seedCancelledNMISubscription(t, time.Now().Add(-time.Minute))
	fake, client := newFakeNMI(t, fx.psid, true)

	expired := time.Now().Add(-time.Hour).UTC()
	row, err := fx.store.Enqueue(context.Background(), EnqueueParams{
		MerchantID:       dbtest.TestTenantID.UUID(),
		Provider:       "mobius",
		IntentType:     TypeNMIDeleteSubscription,
		SubscriptionID: &fx.subID,
		IdempotencyKey: NMIDeleteIdempotencyKey(fx.subID),
		NextAttemptAt:  time.Now().Add(-2 * time.Hour),
		Origin:         OriginUser,
		ExpiresAt:      &expired,
	})
	require.NoError(t, err)

	_, err = fx.runner(client, fullModeConfig()).RunExecuteOnce(context.Background())
	require.NoError(t, err)

	got := fx.intent(t, row.ID)
	assert.Equal(t, StatusExpired, got.Status)
	require.NotNil(t, got.LastFailureReason)
	assert.Contains(t, *got.LastFailureReason, "relevance window elapsed")
	assert.Zero(t, fake.deleteCalls.Load(), "expired intents never fire")
}

// TestClaimLeaseReclaim: an in_flight row whose lease elapsed (crashed
// executor) is reclaimable; verify-then-execute makes the reclaim safe.
func TestClaimLeaseReclaim(t *testing.T) {
	fx := seedCancelledNMISubscription(t, time.Now().Add(-time.Minute))
	_, client := newFakeNMI(t, fx.psid, true)
	row := fx.enqueueDelete(t, OriginUser, time.Now().Add(-time.Minute))

	// Simulate a crashed executor: claimed long ago, lease elapsed.
	_, err := fx.db.Pool().Exec(context.Background(),
		`UPDATE openrails.provider_intents
		 SET status = 'in_flight', claimed_until = now() - interval '1 minute', attempts = 1
		 WHERE id = $1`, row.ID)
	require.NoError(t, err)

	stats, err := fx.runner(client, fullModeConfig()).RunExecuteOnce(context.Background())
	require.NoError(t, err)
	require.GreaterOrEqual(t, stats.Claimed, 1)
	assert.Equal(t, StatusSucceeded, fx.intent(t, row.ID).Status)
}
