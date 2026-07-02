//go:build integration

package intents

import (
	"context"
	"encoding/json"
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

// fakeNMI scripts the gateway: GET /subscriptions/{id} answers whether the
// subscription is "present" (404 = gone), DELETE /subscriptions/{id} answers
// the delete. Both record call counts.
type fakeNMI struct {
	present      atomic.Bool
	deleteBody   atomic.Value // string JSON response for the delete (empty = 200 no body)
	queryCalls   atomic.Int64
	deleteCalls  atomic.Int64
	deleteStatus atomic.Int64 // optional HTTP status for the delete (0 = 200)
	psid         string
}

func newFakeNMI(t *testing.T, psid string, present bool) (*fakeNMI, *nmi.NMIClient) {
	t.Helper()
	f := &fakeNMI{psid: psid}
	f.present.Store(present)
	f.deleteBody.Store("{}")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			f.queryCalls.Add(1)
			if f.present.Load() {
				fmt.Fprintf(w, `{"object":"subscription","id":"%s"}`, f.psid)
			} else {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"type":"notFound","error_code":"E_NOT_FOUND","message":"subscription not found"}`)
			}
		case http.MethodDelete:
			f.deleteCalls.Add(1)
			if st := f.deleteStatus.Load(); st != 0 {
				w.WriteHeader(int(st))
				fmt.Fprint(w, `{"type":"internalError","error_code":"E_INTERNAL","message":"delete failed"}`)
				return
			}
			_, _ = w.Write([]byte(f.deleteBody.Load().(string)))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)

	client, err := nmi.NewClient("mobius", &config.NMIProviderSettings{
		SecurityKey:   "test_security_key",
		WebhookSecret: "test_secret",
	}, true)
	require.NoError(t, err)
	client.DirectPostURL = srv.URL
	client.QueryURL = srv.URL
	client.V5BaseURL = srv.URL
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
	fx.userID = dbtest.EnsureCustomerIDPgx(ctx, t, pool, uuid.NewString())

	productID := uuid.New()
	priceID := uuid.New()
	suffix := uuid.NewString()[:8]
	now := time.Now().UTC()

	exec := func(sql string, args ...any) {
		t.Helper()
		_, err := pool.Exec(ctx, sql, args...)
		require.NoError(t, err)
	}
	tenantID := dbtest.TestMerchantID.UUID()
	exec(`INSERT INTO openrails.products (id, key, display_name, merchant_id) VALUES ($1, $2, $2, $3)`,
		productID, "intent-prod-"+suffix, tenantID)
	exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, access_duration_hours, auto_renew, merchant_id)
	      VALUES ($1, $2, 999, 'usd', 720, true, $3)`, priceID, productID, tenantID)
	exec(`INSERT INTO openrails.subscriptions
	        (id, price_id, product_id, status, rail, rail_subscription_id,
	         current_period_starts_at, current_period_ends_at, started_at,
	         cancelled_at, cancel_type, deletion_scheduled_at, customer_id, merchant_id)
	      VALUES ($1, $2, $3, 'cancelled', 'mobius', $4, $5, $6, $5, $7, 'user', $8, $9, $10)`,
		fx.subID, priceID, productID, fx.psid,
		now.Add(-10*24*time.Hour), now.Add(20*24*time.Hour), now,
		deletionScheduledAt.UTC(), fx.userID, tenantID)

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.rail_mutation_logs
			WHERE provider_intent_id IN (SELECT id FROM openrails.rail_intents WHERE subscription_id = $1)`, fx.subID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.rail_intents WHERE subscription_id = $1", fx.subID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.subscriptions WHERE id = $1", fx.subID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})
	return fx
}

func (fx intentFixture) enqueueDelete(t *testing.T, origin Origin, dueAt time.Time) gen.OpenrailsRailIntent {
	t.Helper()
	row, err := fx.store.Enqueue(context.Background(), EnqueueParams{
		MerchantID:     dbtest.TestMerchantID.UUID(),
		Provider:       "mobius",
		IntentType:     TypeNMIDeleteSubscription,
		SubscriptionID: &fx.subID,
		Payload:        NMIDeletePayload{UserID: fx.userID.String(), RailSubscriptionID: fx.psid},
		IdempotencyKey: NMIDeleteIdempotencyKey(fx.subID),
		NextAttemptAt:  dueAt,
		Origin:         origin,
		OriginReason:   "integration test",
	})
	require.NoError(t, err)
	return row
}

func (fx intentFixture) intent(t *testing.T, id uuid.UUID) gen.OpenrailsRailIntent {
	t.Helper()
	row, err := fx.db.Gen(context.Background()).GetRailIntent(context.Background(), id)
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
	// #607: the succeeded tombstone is slimmed — nmi_delete's forensic evidence
	// (deleted:true) is dropped from Postgres but retained in the mutation log
	// (asserted below), and the payload is pruned.
	assert.Empty(t, got.ResultEvidence, "forensic evidence pruned from the tombstone")
	assert.Nil(t, got.Payload, "payload pruned from the tombstone")
	assert.EqualValues(t, 1, fake.queryCalls.Load(), "verify-then-execute reads first")
	assert.EqualValues(t, 1, fake.deleteCalls.Load())
	assert.Nil(t, fx.deletionMarker(t), "success finalizes the read model (marker cleared)")

	rows, err := fx.db.Pool().Query(context.Background(), `
		SELECT phase, attempt, evidence::text
		FROM openrails.rail_mutation_logs
		WHERE provider_intent_id = $1
		ORDER BY created_at, id`, row.ID)
	require.NoError(t, err)
	defer rows.Close()
	var phases []string
	var attempts []int32
	var evidence []string
	for rows.Next() {
		var phase string
		var attempt int32
		var ev *string
		require.NoError(t, rows.Scan(&phase, &attempt, &ev))
		phases = append(phases, phase)
		attempts = append(attempts, attempt)
		if ev != nil {
			evidence = append(evidence, *ev)
		}
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []string{"attempting", "succeeded"}, phases)
	assert.Equal(t, []int32{1, 1}, attempts)
	require.NotEmpty(t, evidence)
	assert.Contains(t, evidence[len(evidence)-1], `"deleted": true`)
	assert.Contains(t, evidence[len(evidence)-1], fx.subID.String())
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
	assert.Empty(t, got.ResultEvidence, "#607: forensic evidence pruned from the succeeded tombstone")
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
		"UPDATE openrails.rail_intents SET next_attempt_at = now() WHERE id = $1", row.ID)
	require.NoError(t, err)

	stats, err := fx.runner(client, fullModeConfig()).RunVerifyOnce(context.Background())
	require.NoError(t, err)
	require.GreaterOrEqual(t, stats.Claimed, 1)

	got = fx.intent(t, row.ID)
	assert.Equal(t, StatusSucceeded, got.Status)
	assert.Empty(t, got.ResultEvidence, "#607: forensic evidence pruned even when the verifier resolves the success")
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
		"UPDATE openrails.rail_intents SET next_attempt_at = now() WHERE id = $1", row.ID)
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
		"UPDATE openrails.rail_intents SET next_attempt_at = now() WHERE id = $1", row.ID)
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
			"UPDATE openrails.rail_intents SET next_attempt_at = now() WHERE id = $1", row.ID)
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
		"SELECT count(*) FROM openrails.rail_intents WHERE subscription_id = $1", fx.subID).Scan(&count))
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

// pruneTestHandler always succeeds with rich evidence: two result-pointer keys
// the dunning repair path reads back (transaction_id, response_code) plus bulk
// forensic keys that prune (#607) must drop. It counts Execute calls so a test
// can prove the dedupe tombstone survives the prune and never re-charges.
type pruneTestHandler struct{ calls *atomic.Int64 }

const typePruneTest = "prune_test_intent"

func (h *pruneTestHandler) Type() string { return typePruneTest }
func (h *pruneTestHandler) CheckRelevance(context.Context, gen.OpenrailsRailIntent) (Relevance, error) {
	return StillRelevant(), nil
}
func (h *pruneTestHandler) Execute(context.Context, gen.OpenrailsRailIntent) Outcome {
	h.calls.Add(1)
	return Succeeded(map[string]any{
		"transaction_id": "txn-abc-123",
		"response_code":  1,
		"gateway_raw":    "verbose forensic blob that ClickHouse already retains",
		"auth_code":      "XYZ789",
	})
}
func (h *pruneTestHandler) Verify(context.Context, gen.OpenrailsRailIntent) Outcome {
	return Ambiguous("unused")
}
func (h *pruneTestHandler) Backoff(int32) time.Duration { return time.Minute }

// TestSucceededIntentIsPrunedToSlimTombstone (#607): after a successful
// execute the rail_intents row STILL EXISTS (it is the effectively-once
// dedupe tombstone) but its heavy payload is dropped and result_evidence is
// reduced to the pointer keys. A re-enqueue + re-execute with the same
// idempotency key returns the same succeeded row WITHOUT re-charging, proving
// the dedupe survived the prune.
func TestSucceededIntentIsPrunedToSlimTombstone(t *testing.T) {
	fx := seedCancelledNMISubscription(t, time.Now().Add(-time.Minute))
	ctx := context.Background()
	var calls atomic.Int64

	runner := &Runner{
		Store:    fx.store,
		Registry: NewRegistry(&pruneTestHandler{calls: &calls}),
		Config:   fullModeConfig(),
	}

	idemKey := "prune-test-" + uuid.NewString()
	enqueue := func() gen.OpenrailsRailIntent {
		t.Helper()
		row, err := fx.store.Enqueue(ctx, EnqueueParams{
			MerchantID:     dbtest.TestMerchantID.UUID(),
			Provider:       "mobius",
			IntentType:     typePruneTest,
			SubscriptionID: &fx.subID, // so the fixture cleanup reaps the row
			Payload:        map[string]any{"big": "heavy payload that must be pruned on success", "rebill_amount": 4999},
			IdempotencyKey: idemKey,
			NextAttemptAt:  time.Now().Add(-time.Minute),
			Origin:         OriginSystem,
			OriginReason:   "prune integration test",
		})
		require.NoError(t, err)
		return row
	}

	row := enqueue()
	require.NotEmpty(t, row.Payload, "payload present before execution")

	// (1) Execute -> success -> prune.
	_, err := runner.EnqueueAndExecute(ctx, EnqueueParams{
		MerchantID:     dbtest.TestMerchantID.UUID(),
		Provider:       "mobius",
		IntentType:     typePruneTest,
		SubscriptionID: &fx.subID,
		Payload:        map[string]any{"big": "heavy payload that must be pruned on success"},
		IdempotencyKey: idemKey,
		NextAttemptAt:  time.Now().Add(-time.Minute),
		Origin:         OriginSystem,
		OriginReason:   "prune integration test",
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, calls.Load(), "executed exactly once")

	// (a) Row STILL EXISTS (count=1), status succeeded, payload pruned to NULL,
	// result_evidence slimmed to ONLY the pointer keys.
	status, payload, evidence := readRaw(t, fx, row.ID)
	assert.Equal(t, StatusSucceeded, status)
	assert.Nil(t, payload, "payload pruned to NULL on success")
	require.NotNil(t, evidence, "result_evidence retained as slim tombstone")
	assert.Contains(t, *evidence, `"transaction_id"`)
	assert.Contains(t, *evidence, "txn-abc-123")
	assert.Contains(t, *evidence, `"response_code"`)
	assert.NotContains(t, *evidence, "gateway_raw", "bulk forensics dropped from Postgres")
	assert.NotContains(t, *evidence, "auth_code", "bulk forensics dropped from Postgres")

	// The dunning repair path reads these back off the slim tombstone.
	tomb := fx.intent(t, row.ID)
	assert.Equal(t, "txn-abc-123", manualRebillEvidenceStringForTest(tomb, "transaction_id"))

	var count int
	require.NoError(t, fx.db.Pool().QueryRow(ctx,
		"SELECT count(*) FROM openrails.rail_intents WHERE merchant_id = $1 AND idempotency_key = $2",
		dbtest.TestMerchantID.UUID(), idemKey).Scan(&count))
	assert.Equal(t, 1, count, "tombstone row retained (dedupe guard)")

	// (b) Re-enqueue + re-execute with the same idempotency key: same row,
	// still succeeded, NO re-charge (Execute counter stays at 1).
	again, err := runner.EnqueueAndExecute(ctx, EnqueueParams{
		MerchantID:     dbtest.TestMerchantID.UUID(),
		Provider:       "mobius",
		IntentType:     typePruneTest,
		SubscriptionID: &fx.subID,
		Payload:        map[string]any{"big": "re-enqueue payload should not revive a succeeded row"},
		IdempotencyKey: idemKey,
		NextAttemptAt:  time.Now().Add(-time.Minute),
		Origin:         OriginSystem,
		OriginReason:   "prune integration test re-enqueue",
	})
	require.NoError(t, err)
	assert.Equal(t, row.ID, again.ID, "same logical intent, same row")
	assert.Equal(t, StatusSucceeded, again.Status, "succeeded is immutable to re-enqueue")
	assert.EqualValues(t, 1, calls.Load(), "dedupe survived the prune: no second charge")

	// Payload stayed pruned (the re-enqueue did NOT resurrect the heavy payload).
	_, payload2, _ := readRaw(t, fx, row.ID)
	assert.Nil(t, payload2, "re-enqueue of a succeeded row leaves payload pruned")

	// (c) A second scheduled executor pass post-prune does NOT double-execute.
	_, err = runner.RunExecuteOnce(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 1, calls.Load(), "scheduled pass never re-claims a succeeded tombstone")
}

// readRaw reads the row's status/payload/result_evidence straight from
// Postgres (not via the gen row) so the test asserts on the literal stored
// column values.
func readRaw(t *testing.T, fx intentFixture, id uuid.UUID) (status string, payload, evidence *string) {
	t.Helper()
	err := fx.db.Pool().QueryRow(context.Background(),
		`SELECT status, payload::text, result_evidence::text
		   FROM openrails.rail_intents WHERE id = $1`, id).Scan(&status, &payload, &evidence)
	require.NoError(t, err)
	return status, payload, evidence
}

// manualRebillEvidenceStringForTest mirrors the dunning repair path's read of
// one string key off result_evidence, proving the slim tombstone still answers
// it.
func manualRebillEvidenceStringForTest(intent gen.OpenrailsRailIntent, key string) string {
	if len(intent.ResultEvidence) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(intent.ResultEvidence, &m); err != nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

// TestRelevanceWindowExpiry: an intent past its expires_at is expired by the
// executor pass instead of firing stale.
func TestRelevanceWindowExpiry(t *testing.T) {
	fx := seedCancelledNMISubscription(t, time.Now().Add(-time.Minute))
	fake, client := newFakeNMI(t, fx.psid, true)

	expired := time.Now().Add(-time.Hour).UTC()
	row, err := fx.store.Enqueue(context.Background(), EnqueueParams{
		MerchantID:     dbtest.TestMerchantID.UUID(),
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
		`UPDATE openrails.rail_intents
		 SET status = 'in_flight', claimed_until = now() - interval '1 minute', attempts = 1
		 WHERE id = $1`, row.ID)
	require.NoError(t, err)

	stats, err := fx.runner(client, fullModeConfig()).RunExecuteOnce(context.Background())
	require.NoError(t, err)
	require.GreaterOrEqual(t, stats.Claimed, 1)
	assert.Equal(t, StatusSucceeded, fx.intent(t, row.ID).Status)
}
