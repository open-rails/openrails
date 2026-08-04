//go:build integration

package riverjobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	riverpgxv5 "github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

// #689/#895 end-to-end through a REAL River client: the middleware records every
// worked job's outcome in openrails.worker_health, and ProgressMonitor — which
// is NOT a River job (#895) — routes unhealthy kinds to the existing
// repair-alert channel (notification_queue system alerts). The failure mode of
// #673 (a worker failing 100% of its runs since birth) becomes a durable alert
// instead of log wallpaper, and unlike the retired WorkerHealthCheckWorker the
// evaluation runs even when River itself never worked a job.

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
	mctx := dbtest.WithTestMerchant(context.Background())
	systemCustomerID := db.SystemCustomerID(dbtest.TestMerchantID.UUID())
	var systemCustomerExisted bool
	require.NoError(t, dbi.RunInMerchantConn(mctx, func(ctx context.Context) error {
		return dbi.Qx(ctx).QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM openrails.customers WHERE id = $1)`, systemCustomerID,
		).Scan(&systemCustomerExisted)
	}))
	t.Cleanup(func() {
		ctx := context.Background()
		for _, kind := range kinds {
			_, _ = dbi.Qx(ctx).Exec(ctx, `DELETE FROM openrails.worker_health WHERE worker_kind = $1`, kind)
		}
		mctx := dbtest.WithTestMerchant(ctx)
		_ = dbi.RunInMerchantConn(mctx, func(ctx context.Context) error {
			_, _ = dbi.Qx(ctx).Exec(ctx, `DELETE FROM openrails.notification_queue WHERE event_type = 'system_alert' AND data->>'operation' = 'river_progress'`)
			if !systemCustomerExisted {
				_, _ = dbi.Qx(ctx).Exec(ctx,
					`DELETE FROM openrails.customers c
					 WHERE c.id = $1
					   AND NOT EXISTS (SELECT 1 FROM openrails.notification_queue nq WHERE nq.customer_id = c.id)`,
					systemCustomerID,
				)
			}
			return nil
		})
	})
}

// cleanupNewFanoutSystemCustomers removes system subjects created for active
// merchants that predated this test. Per-test merchants are removed by each
// test's primary cleanup; this helper prevents shared fixture pollution.
func cleanupNewFanoutSystemCustomers(t *testing.T, super *db.DB, kind string) {
	t.Helper()
	ctx := context.Background()
	rows, err := super.Pool().Query(ctx,
		`SELECT id FROM openrails.merchants WHERE status = 'active'`)
	require.NoError(t, err)
	defer rows.Close()

	var createdCandidates []uuid.UUID
	for rows.Next() {
		var merchantID uuid.UUID
		require.NoError(t, rows.Scan(&merchantID))
		systemCustomerID := db.SystemCustomerID(merchantID)
		var existed bool
		require.NoError(t, super.Pool().QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM openrails.customers WHERE id = $1)`, systemCustomerID,
		).Scan(&existed))
		if !existed {
			createdCandidates = append(createdCandidates, systemCustomerID)
		}
	}
	require.NoError(t, rows.Err())

	t.Cleanup(func() {
		_, err := super.Pool().Exec(ctx,
			`DELETE FROM openrails.notification_queue WHERE data->>'worker_kind' = $1`, kind)
		require.NoError(t, err)
		for _, customerID := range createdCandidates {
			_, err = super.Pool().Exec(ctx,
				`DELETE FROM openrails.customers c
				 WHERE c.id = $1
				   AND NOT EXISTS (SELECT 1 FROM openrails.notification_queue nq WHERE nq.customer_id = c.id)`,
				customerID,
			)
			require.NoError(t, err)
		}
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
	return countWorkerHealthAlertsForMerchant(t, dbi, dbtest.TestMerchantID, kind)
}

func countWorkerHealthAlertsForMerchant(t *testing.T, dbi *db.DB, merchantID merchant.ID, kind string) int {
	t.Helper()
	mctx := merchant.WithID(context.Background(), merchantID)
	var n int
	require.NoError(t, dbi.RunInMerchantConn(mctx, func(ctx context.Context) error {
		return dbi.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM openrails.notification_queue
			 WHERE merchant_id = $1 AND event_type = 'system_alert'
			   AND data->>'operation' = 'river_progress' AND data->>'worker_kind' = $2`,
			merchantID.UUID(), kind).Scan(&n)
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
			   AND data->>'operation' = 'river_progress' AND data->>'worker_kind' = $2
			 ORDER BY created_at DESC LIMIT 1`,
			dbtest.TestMerchantID.UUID(), kind).Scan(&reason)
	}))
	return reason
}

func TestWorkerHealth_MiddlewareRecordsAndCheckerAlerts(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbtest.SharedPGXPool(t)
	dbtest.EnsureTestMerchant(dbtest.WithTestMerchant(context.Background()), t, dbi.Pool())
	cleanupWorkerHealth(t, dbi, whFailingKind, whHealthyKind)

	regs := NewWorkerRegistrations()
	regs.NoteKind(whFailingKind)
	regs.NoteKind(whHealthyKind)

	// #895: the detector is a plain object, not a worker. It is never registered
	// with River and never enqueued — it is simply called.
	monitor := &ProgressMonitor{
		DB:               dbi,
		Registrations:    regs,
		FailureThreshold: 3,
		MinStale:         time.Hour, // keep staleness out of this test
	}

	workers := river.NewWorkers()
	require.NoError(t, river.AddWorkerSafely(workers, &whFailingWorker{}))
	require.NoError(t, river.AddWorkerSafely(workers, &whHealthyWorker{}))

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

	// The monitor trips the streak rule for the failing kind and stays quiet for
	// the healthy one — evaluated OUT of band, with nothing enqueued.
	report, err := monitor.Check(ctx)
	require.NoError(t, err)
	require.NoError(t, monitor.RaiseAlerts(ctx, report))

	require.Equal(t, 1, countWorkerHealthAlerts(t, dbi, whFailingKind), "failing kind alerts within threshold")
	require.Equal(t, "consecutive_failures", alertReason(t, dbi, whFailingKind))
	require.Equal(t, 0, countWorkerHealthAlerts(t, dbi, whHealthyKind), "healthy kind never alerts")

	// Re-running the monitor within the re-alert window does NOT duplicate.
	report, err = monitor.Check(ctx)
	require.NoError(t, err)
	require.NoError(t, monitor.RaiseAlerts(ctx, report))
	require.Equal(t, 1, countWorkerHealthAlerts(t, dbi, whFailingKind), "alert deduped while incident persists")
}

// TestWorkerHealth_NeverSucceededMerchantRequire reproduces #673: a periodic
// worker whose Work() errors immediately on merchant.Require (bare job context)
// trips never-succeeded — even before the failure-streak threshold is reached.
func TestWorkerHealth_NeverSucceededMerchantRequire(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbtest.SharedPGXPool(t)
	dbtest.EnsureTestMerchant(dbtest.WithTestMerchant(context.Background()), t, dbi.Pool())
	cleanupWorkerHealth(t, dbi, whNoMerchKind)

	regs := NewWorkerRegistrations()
	regs.NoteKind(whNoMerchKind)
	regs.NotePeriod(whNoMerchKind, time.Second) // "hourly" in miniature

	monitor := &ProgressMonitor{
		DB:               dbi,
		Registrations:    regs,
		FailureThreshold: 99, // force the never-succeeded rule to be what trips
		StaleMultiplier:  1,
		MinStale:         time.Second,
	}

	workers := river.NewWorkers()
	require.NoError(t, river.AddWorkerSafely(workers, &whNoMerchWorker{}))

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

	// First monitor pass seeds the row; backdate registration past the grace
	// window (in production the row simply ages), then re-run the monitor.
	report, err := monitor.Check(ctx)
	require.NoError(t, err)
	require.NoError(t, monitor.RaiseAlerts(ctx, report))
	_, execErr := dbi.Qx(ctx).Exec(ctx,
		`UPDATE openrails.worker_health SET registered_at = now() - interval '1 hour' WHERE worker_kind = $1`, whNoMerchKind)
	require.NoError(t, execErr)

	report, err = monitor.Check(ctx)
	require.NoError(t, err)
	require.NoError(t, monitor.RaiseAlerts(ctx, report))

	require.Equal(t, 1, countWorkerHealthAlerts(t, dbi, whNoMerchKind))
	require.Equal(t, "never_succeeded", alertReason(t, dbi, whNoMerchKind))
}

func TestWorkerHealth_AlertFanoutReachesEveryMerchantUnderRLS(t *testing.T) {
	ctx := context.Background()
	superDSN, appDSN := dbtest.SharedRLSPostgres(t)
	super := dbtest.OpenAppDB(t, superDSN)
	appDB := dbtest.OpenAppDB(t, appDSN)

	posture, err := appDB.CheckRLSPosture(ctx)
	require.NoError(t, err)
	require.True(t, posture.Enforcing, "test must use the RLS-enforcing app role")

	suffix := uuid.NewString()[:8]
	merchantA := merchant.ID(uuid.New())
	merchantB := merchant.ID(uuid.New())
	kind := "test.wh_multi_merchant_" + suffix
	t.Cleanup(func() {
		_, err := super.Pool().Exec(ctx,
			`DELETE FROM openrails.notification_queue WHERE data->>'worker_kind' = $1`, kind)
		require.NoError(t, err)
		_, err = super.Pool().Exec(ctx,
			`DELETE FROM openrails.customers WHERE merchant_id IN ($1, $2)`,
			merchantA.UUID(), merchantB.UUID(),
		)
		require.NoError(t, err)
		_, err = super.Pool().Exec(ctx,
			`DELETE FROM openrails.merchants WHERE id IN ($1, $2)`,
			merchantA.UUID(), merchantB.UUID(),
		)
		require.NoError(t, err)
	})
	for label, merchantID := range map[string]merchant.ID{"a": merchantA, "b": merchantB} {
		_, err := super.Pool().Exec(ctx,
			`INSERT INTO openrails.merchants (id, slug, status) VALUES ($1, $2, 'active')`,
			merchantID.UUID(), "wh-"+suffix+"-"+label,
		)
		require.NoError(t, err)
	}
	cleanupNewFanoutSystemCustomers(t, super, kind)

	monitor := &ProgressMonitor{DB: appDB}
	err = monitor.raiseAlert(ctx, gen.OpenrailsWorkerHealth{WorkerKind: kind}, "stale", time.Now().UTC(), ProgressReport{})
	require.NoError(t, err)

	for _, merchantID := range []merchant.ID{merchantA, merchantB} {
		require.Equal(t, 1, countWorkerHealthAlertsForMerchant(t, appDB, merchantID, kind))

		mctx := merchant.WithID(ctx, merchantID)
		require.NoError(t, appDB.RunInMerchantConn(mctx, func(ctx context.Context) error {
			var customerID uuid.UUID
			err := appDB.Qx(ctx).QueryRow(ctx,
				`SELECT customer_id FROM openrails.notification_queue
				 WHERE data->>'worker_kind' = $1`, kind,
			).Scan(&customerID)
			if err != nil {
				return err
			}
			require.Equal(t, db.SystemCustomerID(merchantID.UUID()), customerID)
			return nil
		}))
	}
	require.NotEqual(t, db.SystemCustomerID(merchantA.UUID()), db.SystemCustomerID(merchantB.UUID()))
}

func TestWorkerHealth_AlertFanoutReportsPartialFailure(t *testing.T) {
	ctx := context.Background()
	superDSN, appDSN := dbtest.SharedRLSPostgres(t)
	super := dbtest.OpenAppDB(t, superDSN)
	appDB := dbtest.OpenAppDB(t, appDSN)

	suffix := uuid.NewString()[:8]
	merchantA := merchant.ID(uuid.New())
	merchantB := merchant.ID(uuid.New())
	kind := "test.wh_partial_failure_" + suffix
	t.Cleanup(func() {
		_, err := super.Pool().Exec(ctx,
			`DELETE FROM openrails.notification_queue WHERE data->>'worker_kind' = $1`, kind)
		require.NoError(t, err)
		_, err = super.Pool().Exec(ctx,
			`DELETE FROM openrails.customers WHERE merchant_id IN ($1, $2)`,
			merchantA.UUID(), merchantB.UUID(),
		)
		require.NoError(t, err)
		_, err = super.Pool().Exec(ctx,
			`DELETE FROM openrails.merchants WHERE id IN ($1, $2)`,
			merchantA.UUID(), merchantB.UUID(),
		)
		require.NoError(t, err)
	})
	for label, merchantID := range map[string]merchant.ID{"a": merchantA, "b": merchantB} {
		_, err := super.Pool().Exec(ctx,
			`INSERT INTO openrails.merchants (id, slug, status) VALUES ($1, $2, 'active')`,
			merchantID.UUID(), "wh-partial-"+suffix+"-"+label,
		)
		require.NoError(t, err)
	}
	cleanupNewFanoutSystemCustomers(t, super, kind)

	// Corrupt B's derived system-customer identity by assigning it to A. The
	// RLS-enforcing B connection cannot see or update that conflicting row.
	conflictingID := db.SystemCustomerID(merchantB.UUID())
	_, err := super.Pool().Exec(ctx,
		`INSERT INTO openrails.customers (id, merchant_id, subject) VALUES ($1, $2, $3)`,
		conflictingID, merchantA.UUID(), conflictingID.String(),
	)
	require.NoError(t, err)

	monitor := &ProgressMonitor{DB: appDB}
	row := gen.OpenrailsWorkerHealth{WorkerKind: kind, RegisteredAt: time.Now().Add(-time.Hour).UTC()}
	now := time.Now().UTC()
	err = monitor.raiseAlert(ctx, row, "stale", now, ProgressReport{})
	require.Error(t, err)
	require.ErrorContains(t, err, merchantB.String())
	require.Equal(t, 1, countWorkerHealthAlertsForMerchant(t, appDB, merchantA, kind),
		"successful merchant delivery remains durable")
	require.Equal(t, 0, countWorkerHealthAlertsForMerchant(t, appDB, merchantB, kind),
		"failed merchant delivery must not be reported as successful")

	// The monitor re-evaluates on its next tick after a partial fan-out failure.
	// The successful merchant must not receive another copy of the same incident.
	err = monitor.raiseAlert(ctx, row, "stale", now.Add(time.Minute), ProgressReport{})
	require.Error(t, err)
	require.ErrorContains(t, err, merchantB.String())
	require.Equal(t, 1, countWorkerHealthAlertsForMerchant(t, appDB, merchantA, kind),
		"retry must reuse the incident ID for deliveries that already succeeded")
	require.Equal(t, 0, countWorkerHealthAlertsForMerchant(t, appDB, merchantB, kind))
}
