//go:build integration

package riverjobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/riverqueue/river"
	riverpgxv5 "github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

// #689 end-to-end through a REAL River client: the one middleware records every
// worked job's outcome in openrails.worker_health, and the checker routes
// unhealthy kinds to the existing repair-alert channel (notification_queue
// system alerts) — the failure mode of #673 (a worker failing 100% of its runs
// since birth) becomes a durable alert instead of log wallpaper.

const (
	whFailingKind = "test.wh_always_failing"
	whHealthyKind = "test.wh_healthy"
	whNoMerchKind = "test.wh_merchant_require"
)

type whFailingArgs struct{}

func (whFailingArgs) Kind() string { return whFailingKind }

type whFailingWorker struct {
	river.WorkerDefaults[whFailingArgs]
}

func (whFailingWorker) Work(context.Context, *river.Job[whFailingArgs]) error {
	return errors.New("boom: this worker has never worked")
}

type whHealthyArgs struct{}

func (whHealthyArgs) Kind() string { return whHealthyKind }

type whHealthyWorker struct {
	river.WorkerDefaults[whHealthyArgs]
}

func (whHealthyWorker) Work(context.Context, *river.Job[whHealthyArgs]) error { return nil }

type whNoMerchArgs struct{}

func (whNoMerchArgs) Kind() string { return whNoMerchKind }

// whNoMerchWorker reproduces #673 exactly: Work() on a bare job context hits
// merchant.Require and errors on every single run.
type whNoMerchWorker struct {
	river.WorkerDefaults[whNoMerchArgs]
}

func (whNoMerchWorker) Work(ctx context.Context, _ *river.Job[whNoMerchArgs]) error {
	if _, err := merchant.Require(ctx); err != nil {
		return err
	}
	return nil
}

func cleanupWorkerHealth(t *testing.T, dbi *db.DB, kinds ...string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		for _, kind := range kinds {
			_, _ = dbi.Qx(ctx).Exec(ctx, `DELETE FROM openrails.worker_health WHERE worker_kind = $1`, kind)
		}
		mctx := dbtest.WithTestMerchant(ctx)
		_ = dbi.RunInMerchantConn(mctx, func(ctx context.Context) error {
			_, _ = dbi.Qx(ctx).Exec(ctx, `DELETE FROM openrails.notification_queue WHERE event_type = 'system_alert' AND data->>'operation' = 'worker_health'`)
			return nil
		})
	})
}

// awaitJobs drains n terminal events from the subscription.
func awaitJobs(t *testing.T, events <-chan *river.Event, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-events:
		case <-time.After(60 * time.Second):
			t.Fatalf("timed out waiting for river job event %d/%d", i+1, n)
		}
	}
}

func workerHealthRow(t *testing.T, dbi *db.DB, kind string) (lastSuccess, lastError *time.Time, lastErrMsg *string, streak int32) {
	t.Helper()
	ctx := context.Background()
	err := dbi.Qx(ctx).QueryRow(ctx,
		`SELECT last_success_at, last_error_at, last_error, consecutive_failures FROM openrails.worker_health WHERE worker_kind = $1`, kind).
		Scan(&lastSuccess, &lastError, &lastErrMsg, &streak)
	require.NoError(t, err, "worker_health row for %s", kind)
	return
}

func countWorkerHealthAlerts(t *testing.T, dbi *db.DB, kind string) int {
	t.Helper()
	mctx := dbtest.WithTestMerchant(context.Background())
	var n int
	require.NoError(t, dbi.RunInMerchantConn(mctx, func(ctx context.Context) error {
		return dbi.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM openrails.notification_queue
			 WHERE merchant_id = $1 AND event_type = 'system_alert'
			   AND data->>'operation' = 'worker_health' AND data->>'worker_kind' = $2`,
			dbtest.TestMerchantID.UUID(), kind).Scan(&n)
	}))
	return n
}

func alertReason(t *testing.T, dbi *db.DB, kind string) string {
	t.Helper()
	mctx := dbtest.WithTestMerchant(context.Background())
	var reason string
	require.NoError(t, dbi.RunInMerchantConn(mctx, func(ctx context.Context) error {
		return dbi.Qx(ctx).QueryRow(ctx,
			`SELECT data->>'reason' FROM openrails.notification_queue
			 WHERE merchant_id = $1 AND event_type = 'system_alert'
			   AND data->>'operation' = 'worker_health' AND data->>'worker_kind' = $2
			 ORDER BY created_at DESC LIMIT 1`,
			dbtest.TestMerchantID.UUID(), kind).Scan(&reason)
	}))
	return reason
}

func TestWorkerHealth_MiddlewareRecordsAndCheckerAlerts(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbtest.SharedMerchantPool(t, dbtest.TestMerchantID.UUID())
	dbtest.EnsureTestMerchant(dbtest.WithTestMerchant(context.Background()), t, dbtest.SharedMerchantPool(t, dbtest.TestMerchantID.UUID()))
	cleanupWorkerHealth(t, dbi, whFailingKind, whHealthyKind, KindWorkerHealthCheck)

	regs := NewWorkerRegistrations()
	regs.NoteKind(whFailingKind)
	regs.NoteKind(whHealthyKind)
	regs.NoteKind(KindWorkerHealthCheck)

	checker := &WorkerHealthCheckWorker{
		DB:               dbi,
		Registrations:    regs,
		FailureThreshold: 3,
		MinStale:         time.Hour, // keep staleness out of this test
	}

	workers := river.NewWorkers()
	require.NoError(t, river.AddWorkerSafely(workers, &whFailingWorker{}))
	require.NoError(t, river.AddWorkerSafely(workers, &whHealthyWorker{}))
	require.NoError(t, river.AddWorkerSafely(workers, checker))

	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues:            map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 4}},
		Workers:           workers,
		Middleware:        []rivertype.Middleware{NewWorkerHealthMiddleware(dbi)},
		FetchCooldown:     10 * time.Millisecond,
		FetchPollInterval: 50 * time.Millisecond,
	})
	require.NoError(t, err)

	events, cancelSub := client.Subscribe(
		river.EventKindJobCompleted, river.EventKindJobFailed, river.EventKindJobCancelled)
	t.Cleanup(cancelSub)

	ctx := context.Background()
	require.NoError(t, client.Start(ctx))
	t.Cleanup(func() {
		stopCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		_ = client.Stop(stopCtx)
	})

	// Three failing runs + one healthy run, worked by the REAL client.
	for i := 0; i < 3; i++ {
		_, err := client.Insert(ctx, whFailingArgs{}, &river.InsertOpts{MaxAttempts: 1})
		require.NoError(t, err)
	}
	_, err = client.Insert(ctx, whHealthyArgs{}, &river.InsertOpts{MaxAttempts: 1})
	require.NoError(t, err)
	awaitJobs(t, events, 4)

	// Middleware bookkeeping: the failing kind carries the streak + error, the
	// healthy kind a success and a zero streak.
	failSuccess, failErrAt, failErrMsg, failStreak := workerHealthRow(t, dbi, whFailingKind)
	require.Nil(t, failSuccess, "failing worker never succeeded")
	require.NotNil(t, failErrAt)
	require.NotNil(t, failErrMsg)
	require.Contains(t, *failErrMsg, "boom")
	require.EqualValues(t, 3, failStreak)

	okSuccess, _, _, okStreak := workerHealthRow(t, dbi, whHealthyKind)
	require.NotNil(t, okSuccess, "healthy worker recorded a success")
	require.EqualValues(t, 0, okStreak)

	// The checker (run through the client like any worker) trips the streak rule
	// for the failing kind and stays quiet for the healthy one.
	_, err = client.Insert(ctx, WorkerHealthCheckArgs{}, &river.InsertOpts{MaxAttempts: 1})
	require.NoError(t, err)
	awaitJobs(t, events, 1)

	require.Equal(t, 1, countWorkerHealthAlerts(t, dbi, whFailingKind), "failing kind alerts within threshold")
	require.Equal(t, "consecutive_failures", alertReason(t, dbi, whFailingKind))
	require.Equal(t, 0, countWorkerHealthAlerts(t, dbi, whHealthyKind), "healthy kind never alerts")

	// Re-running the checker within the re-alert window does NOT duplicate.
	_, err = client.Insert(ctx, WorkerHealthCheckArgs{}, &river.InsertOpts{MaxAttempts: 1})
	require.NoError(t, err)
	awaitJobs(t, events, 1)
	require.Equal(t, 1, countWorkerHealthAlerts(t, dbi, whFailingKind), "alert deduped while incident persists")

	// The checker heartbeats through the same table (visible if IT wedges).
	chkSuccess, _, _, _ := workerHealthRow(t, dbi, KindWorkerHealthCheck)
	require.NotNil(t, chkSuccess, "checker recorded its own heartbeat")
}

// TestWorkerHealth_NeverSucceededMerchantRequire reproduces #673: a periodic
// worker whose Work() errors immediately on merchant.Require (bare job context)
// trips never-succeeded — even before the failure-streak threshold is reached.
func TestWorkerHealth_NeverSucceededMerchantRequire(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbtest.SharedMerchantPool(t, dbtest.TestMerchantID.UUID())
	dbtest.EnsureTestMerchant(dbtest.WithTestMerchant(context.Background()), t, dbtest.SharedMerchantPool(t, dbtest.TestMerchantID.UUID()))
	cleanupWorkerHealth(t, dbi, whNoMerchKind, KindWorkerHealthCheck)

	regs := NewWorkerRegistrations()
	regs.NoteKind(whNoMerchKind)
	regs.NotePeriod(whNoMerchKind, time.Second) // "hourly" in miniature
	regs.NoteKind(KindWorkerHealthCheck)

	checker := &WorkerHealthCheckWorker{
		DB:               dbi,
		Registrations:    regs,
		FailureThreshold: 99, // force the never-succeeded rule to be what trips
		StaleMultiplier:  1,
		MinStale:         time.Second,
	}

	workers := river.NewWorkers()
	require.NoError(t, river.AddWorkerSafely(workers, &whNoMerchWorker{}))
	require.NoError(t, river.AddWorkerSafely(workers, checker))

	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues:            map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 2}},
		Workers:           workers,
		Middleware:        []rivertype.Middleware{NewWorkerHealthMiddleware(dbi)},
		FetchCooldown:     10 * time.Millisecond,
		FetchPollInterval: 50 * time.Millisecond,
	})
	require.NoError(t, err)

	events, cancelSub := client.Subscribe(
		river.EventKindJobCompleted, river.EventKindJobFailed, river.EventKindJobCancelled)
	t.Cleanup(cancelSub)

	ctx := context.Background()
	require.NoError(t, client.Start(ctx))
	t.Cleanup(func() {
		stopCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		_ = client.Stop(stopCtx)
	})

	// One real run, one real failure — exactly how #673 looked after deploy.
	_, err = client.Insert(ctx, whNoMerchArgs{}, &river.InsertOpts{MaxAttempts: 1})
	require.NoError(t, err)
	awaitJobs(t, events, 1)

	_, _, errMsg, streak := workerHealthRow(t, dbi, whNoMerchKind)
	require.NotNil(t, errMsg)
	require.Contains(t, *errMsg, "merchant")
	require.EqualValues(t, 1, streak)

	// First checker pass seeds the row; backdate registration past the grace
	// window (in production the row simply ages), then re-run the checker.
	_, err = client.Insert(ctx, WorkerHealthCheckArgs{}, &river.InsertOpts{MaxAttempts: 1})
	require.NoError(t, err)
	awaitJobs(t, events, 1)
	_, execErr := dbi.Qx(ctx).Exec(ctx,
		`UPDATE openrails.worker_health SET registered_at = now() - interval '1 hour' WHERE worker_kind = $1`, whNoMerchKind)
	require.NoError(t, execErr)

	_, err = client.Insert(ctx, WorkerHealthCheckArgs{}, &river.InsertOpts{MaxAttempts: 1})
	require.NoError(t, err)
	awaitJobs(t, events, 1)

	require.Equal(t, 1, countWorkerHealthAlerts(t, dbi, whNoMerchKind))
	require.Equal(t, "never_succeeded", alertReason(t, dbi, whNoMerchKind))
}
