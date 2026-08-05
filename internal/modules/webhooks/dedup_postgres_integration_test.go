//go:build integration

package webhooks

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/replaycache"
	"github.com/stretchr/testify/require"
)

// #678 integration: Postgres (webhook_events) is the dedup TRUTH; Redis is a
// flushable cache + lease coordination layer.

func dedupMarkRows(t *testing.T, ctx context.Context, dbi *db.DB, op, eventID string) int {
	t.Helper()
	var n int
	require.NoError(t, dbi.Pool().QueryRow(ctx,
		"SELECT count(*) FROM openrails.webhook_events WHERE op = $1 AND event_id = $2",
		op, eventID).Scan(&n))
	return n
}

func cleanupDedupMark(t *testing.T, ctx context.Context, dbi *db.DB, eventID string) {
	t.Cleanup(func() {
		_, _ = dbi.Pool().Exec(ctx, "DELETE FROM openrails.webhook_events WHERE event_id = $1", eventID)
	})
}

// Headline property: FLUSHALL between delivery and redelivery must NOT reapply
// the event — the Postgres truth row backstops the flushed cache.
func TestWebhookDedupSurvivesRedisFlush(t *testing.T) {
	rdb, rctx := dbtest.SharedRedisClient(t)
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	ctx := dbtest.WithTestMerchant(context.Background())

	idem := replaycache.NewStoreWithTTL(rdb, time.Hour)
	defer idem.Close()
	svc, errsvc := NewDeduplicationService(idem, dbi)
	require.NoError(t, errsvc)

	eventID := "evt_flush_" + uuid.NewString()
	cleanupDedupMark(t, ctx, dbi, eventID)
	applies := 0
	deliver := func() error {
		return svc.ProcessWebhook(ctx, eventID, "TestEvent", models.RailCCBill.EventSource(), nil,
			func(context.Context) error { applies++; return nil })
	}

	require.NoError(t, deliver())
	require.Equal(t, 1, applies)
	require.Equal(t, 1, dedupMarkRows(t, ctx, dbi, "webhook.ccbill.TestEvent", eventID))

	// Nuke Redis entirely: the truth must hold.
	require.NoError(t, rdb.FlushAll(rctx).Err())

	require.NoError(t, deliver())
	require.Equal(t, 1, applies, "redelivery after redis flush must not reapply effects")

	// And the cache was backfilled: the next check is a fast-path skip.
	rec, err := idem.Get(ctx, "webhook.ccbill.TestEvent", eventID)
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Equal(t, replaycache.StatusSuccess, rec.Status)
}

// Two replicas with NO shared Redis (independent per-process memStores) but
// one Postgres: the second replica's delivery must not reapply.
func TestWebhookDedupTwoReplicasNoSharedRedis(t *testing.T) {
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	ctx := dbtest.WithTestMerchant(context.Background())

	idemA := replaycache.NewStore(nil)
	defer idemA.Close()
	idemB := replaycache.NewStore(nil)
	defer idemB.Close()
	replicaA, errreplicaA := NewDeduplicationService(idemA, dbi)
	require.NoError(t, errreplicaA)
	replicaB, errreplicaB := NewDeduplicationService(idemB, dbi)
	require.NoError(t, errreplicaB)

	eventID := "evt_replicas_" + uuid.NewString()
	cleanupDedupMark(t, ctx, dbi, eventID)
	applies := 0
	handler := func(context.Context) error { applies++; return nil }

	require.NoError(t, replicaA.ProcessWebhook(ctx, eventID, "TestEvent", models.RailNMI.EventSource(), nil, handler))
	require.NoError(t, replicaB.ProcessWebhook(ctx, eventID, "TestEvent", models.RailNMI.EventSource(), nil, handler))
	require.Equal(t, 1, applies, "exactly one replica may apply the delivery")
	require.Equal(t, 1, dedupMarkRows(t, ctx, dbi, "webhook.nmi.TestEvent", eventID))
}

// Crash between the effects tx and the write-after mark: effects committed, no
// mark, no cache entry. Redelivery must RUN the (replay-safe, #675) handler and
// converge — final state correct, effect applied once, mark recorded.
func TestWebhookDedupCrashBeforeMarkConverges(t *testing.T) {
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	ctx := dbtest.WithTestMerchant(context.Background())

	idem := replaycache.NewStore(nil)
	defer idem.Close()
	svc, errsvc := NewDeduplicationService(idem, dbi)
	require.NoError(t, errsvc)

	eventID := "evt_crash_" + uuid.NewString()
	cleanupDedupMark(t, ctx, dbi, eventID)

	// #675-style replay-safe effect: apply-once keyed by the event id.
	effects := map[string]int{}
	runs := 0
	handler := func(context.Context) error {
		runs++
		if _, applied := effects[eventID]; !applied { // e.g. DB unique constraint
			effects[eventID] = 1
		}
		return nil
	}

	// Simulate the post-crash state: effects committed, mark never written.
	require.NoError(t, handler(ctx))
	require.Equal(t, 0, dedupMarkRows(t, ctx, dbi, "webhook.stripe.TestEvent", eventID))

	// Redelivery: handler runs again (no mark), effect converges, mark lands.
	require.NoError(t, svc.ProcessWebhook(ctx, eventID, "TestEvent", models.RailStripe.EventSource(), nil, handler))
	require.Equal(t, 2, runs, "post-crash redelivery must run the handler")
	require.Equal(t, 1, effects[eventID], "replay-safe effect applied exactly once")
	require.Equal(t, 1, dedupMarkRows(t, ctx, dbi, "webhook.stripe.TestEvent", eventID))

	// Further redeliveries are skipped outright.
	require.NoError(t, svc.ProcessWebhook(ctx, eventID, "TestEvent", models.RailStripe.EventSource(), nil, handler))
	require.Equal(t, 2, runs)
}

// In-tx mark path: MarkWebhookProcessedInTx inside the handler's MerchantTx
// commits atomically with the effects — a rollback takes the mark with it, and
// a commit makes the wrapper's verify-or-write a no-op.
func TestWebhookDedupInTxMarkAtomicity(t *testing.T) {
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	ctx := dbtest.WithTestMerchant(context.Background())

	idem := replaycache.NewStore(nil)
	defer idem.Close()
	svc, errsvc := NewDeduplicationService(idem, dbi)
	require.NoError(t, errsvc)

	eventID := "evt_intx_" + uuid.NewString()
	op := "webhook.ccbill.TestEvent"
	cleanupDedupMark(t, ctx, dbi, eventID)

	// Effects tx rolls back AFTER writing the mark: the mark must be gone too.
	boom := errors.New("boom")
	err := svc.ProcessWebhook(ctx, eventID, "TestEvent", models.RailCCBill.EventSource(), nil,
		func(ctx context.Context) error {
			return dbi.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
				if err := MarkWebhookProcessedInTx(ctx, tx); err != nil {
					return err
				}
				return boom // roll the whole effect tx back
			})
		})
	require.Error(t, err)
	require.Equal(t, 0, dedupMarkRows(t, ctx, dbi, op, eventID), "rolled-back effects must roll the mark back")

	// Success: mark written inside the effect tx; wrapper verify is a no-op.
	runs := 0
	deliver := func() error {
		return svc.ProcessWebhook(ctx, eventID, "TestEvent", models.RailCCBill.EventSource(), nil,
			func(ctx context.Context) error {
				runs++
				return dbi.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
					return MarkWebhookProcessedInTx(ctx, tx)
				})
			})
	}
	require.NoError(t, deliver())
	require.Equal(t, 1, runs)
	require.Equal(t, 1, dedupMarkRows(t, ctx, dbi, op, eventID))

	require.NoError(t, deliver())
	require.Equal(t, 1, runs, "marked event must not reapply")
}

// Without a merchant on ctx there is no Postgres truth row — dedup degrades to
// the Redis/memory layer with a warning instead of failing the webhook.
func TestWebhookDedupNoMerchantFallsBackToRedisOnly(t *testing.T) {
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	ctx := context.Background() // deliberately no merchant

	idem := replaycache.NewStore(nil)
	defer idem.Close()
	svc, errsvc := NewDeduplicationService(idem, dbi)
	require.NoError(t, errsvc)

	eventID := "evt_nomerchant_" + uuid.NewString()
	applies := 0
	deliver := func() error {
		return svc.ProcessWebhook(ctx, eventID, "TestEvent", models.RailCCBill.EventSource(), nil,
			func(context.Context) error { applies++; return nil })
	}
	require.NoError(t, deliver())
	require.NoError(t, deliver())
	require.Equal(t, 1, applies, "memory-layer dedup still applies")
	require.Equal(t, 0, dedupMarkRows(t, ctx, dbi, "webhook.ccbill.TestEvent", eventID))
}

// Non-retryable failures are terminally handled: the truth row is written, so
// even a cache flush cannot resurrect the futile retries.
func TestWebhookDedupNonRetryableWritesTruth(t *testing.T) {
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	ctx := dbtest.WithTestMerchant(context.Background())

	idem := replaycache.NewStore(nil)
	defer idem.Close()
	svc, errsvc := NewDeduplicationService(idem, dbi)
	require.NoError(t, errsvc)

	eventID := "evt_nonretry_" + uuid.NewString()
	cleanupDedupMark(t, ctx, dbi, eventID)
	attempts := 0
	require.NoError(t, svc.ProcessWebhook(ctx, eventID, "TestEvent", models.RailCCBill.EventSource(), nil,
		func(context.Context) error {
			attempts++
			return MarkWebhookErrorNonRetryable(fmt.Errorf("bad payload"))
		}))
	require.Equal(t, 1, attempts)
	require.Equal(t, 1, dedupMarkRows(t, ctx, dbi, "webhook.ccbill.TestEvent", eventID))

	// Fresh replica (empty memStore = post-flush): still skipped via Postgres.
	idem2 := replaycache.NewStore(nil)
	defer idem2.Close()
	svc2, errsvc2 := NewDeduplicationService(idem2, dbi)
	require.NoError(t, errsvc2)
	require.NoError(t, svc2.ProcessWebhook(ctx, eventID, "TestEvent", models.RailCCBill.EventSource(), nil,
		func(context.Context) error { attempts++; return nil }))
	require.Equal(t, 1, attempts)
}
