//go:build integration

package riverjobs

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	riverpgxv5 "github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
)

// #895 deliverable (B): a live progress detector that does NOT live in River.
//
// These tests exercise the case the retired WorkerHealthCheckWorker could not
// reach by construction — River genuinely not progressing — against a REAL
// Postgres, a REAL river.Client and REAL river_job rows. The detector is the
// same object internal/app runs; only its thresholds are shrunk so the test
// does not take half an hour.

type progArgs struct{}

func (progArgs) Kind() string { return progKind }

const progKind = "test.progress_periodic"

type progWorker struct {
	river.WorkerDefaults[progArgs]
}

func (progWorker) Work(context.Context, *river.Job[progArgs]) error { return nil }

func newProgressMonitor(t *testing.T, dbi *db.DB, regs *WorkerRegistrations) *ProgressMonitor {
	t.Helper()
	return &ProgressMonitor{
		DB:               dbi,
		Pool:             dbtest.SharedPGXPool(t),
		RiverSchema:      "public",
		Registrations:    regs,
		FailureThreshold: 99, // keep the streak rule out of these tests
		StaleMultiplier:  1,
		MinStale:         time.Second,
		ReAlertEvery:     time.Hour,
	}
}

// countRiverJobs reports how many rows exist for the test kind in a given state.
func countRiverJobs(t *testing.T, state string) int {
	t.Helper()
	ctx := context.Background()
	var n int
	require.NoError(t, dbtest.SharedPGXPool(t).QueryRow(ctx,
		`SELECT count(*) FROM public.river_job WHERE kind = $1 AND state::text = $2`, progKind, state).Scan(&n))
	return n
}

func clearRiverJobs(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	_, err := dbtest.SharedPGXPool(t).Exec(ctx, `DELETE FROM public.river_job WHERE kind = $1`, progKind)
	require.NoError(t, err)
}

// TestProgressMonitor_DetectsFleetNeverStarted is the headline #895 proof: a
// host wires the fleet (workers registered, periodic jobs scheduled, client
// constructed) and never starts it. Nothing is enqueued, nothing completes —
// and the detector still fires, because it is not a job.
func TestProgressMonitor_DetectsFleetNeverStarted(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	dbtest.EnsureTestMerchant(dbtest.WithTestMerchant(ctx), t, dbi.Pool())
	cleanupWorkerHealth(t, dbi, progKind, "openrails.river_fleet")
	clearRiverJobs(t)
	t.Cleanup(func() { clearRiverJobs(t) })

	regs := NewWorkerRegistrations()
	regs.NoteKind(progKind)
	regs.NotePeriod(progKind, time.Second)
	monitor := newProgressMonitor(t, dbi, regs)

	// A REAL client, fully wired — and deliberately never started. This is
	// exactly the state that used to be invisible: the in-River detector could
	// not run either, so "nothing is running" looked identical to "healthy".
	workers := river.NewWorkers()
	require.NoError(t, river.AddWorkerSafely(workers, &progWorker{}))
	client, err := river.NewClient(riverpgxv5.New(dbtest.SharedPGXPool(t)), &river.Config{
		Queues:            map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 2}},
		Workers:           workers,
		FetchCooldown:     10 * time.Millisecond,
		FetchPollInterval: 50 * time.Millisecond,
	})
	require.NoError(t, err)
	client.PeriodicJobs().Add(river.NewPeriodicJob(
		river.PeriodicInterval(time.Second),
		func() (river.JobArgs, *river.InsertOpts) { return progArgs{}, nil },
		&river.PeriodicJobOpts{RunOnStart: true},
	))

	// First Check anchors the boot grace window; wait past it, then evaluate.
	first, err := monitor.Check(ctx)
	require.NoError(t, err)
	require.True(t, first.Progressing, "inside the boot grace window nothing is late yet")

	time.Sleep(2 * time.Second)
	stalled, err := monitor.Check(ctx)
	require.NoError(t, err)

	// NEGATIVE CONTROL, in-test: at the instant the detector fires, ZERO jobs
	// have run. Any detector implemented as a River job had exactly zero
	// opportunities to produce this signal.
	require.Equal(t, 0, countRiverJobs(t, "completed"), "no job has completed")
	require.Equal(t, 0, countRiverJobs(t, "available"), "nothing was even enqueued")

	require.False(t, stalled.Progressing, "a fleet that never started is not progressing")
	require.Equal(t, ProgressNotScheduling, stalled.Reason)
	require.Nil(t, stalled.FleetLastEnqueuedAt)

	// The stall reaches the operator through the durable repair-alert channel.
	require.NoError(t, monitor.RaiseAlerts(ctx, stalled))
	require.Equal(t, 1, countWorkerHealthAlerts(t, dbi, "openrails.river_fleet"))
	require.Equal(t, ProgressNotScheduling, alertReason(t, dbi, "openrails.river_fleet"))

	// Now start the SAME client. River schedules and works the periodic job, and
	// the detector flips back on its own — no job ran to tell it so.
	require.NoError(t, client.Start(ctx))
	t.Cleanup(func() { _ = client.Stop(context.Background()) })

	require.Eventually(t, func() bool {
		report, err := monitor.Check(ctx)
		return err == nil && report.Progressing
	}, 30*time.Second, 250*time.Millisecond, "detector must clear once River actually progresses")
}

// TestProgressMonitor_DetectsQueueNotDraining covers the other half of "wired
// but not progressing": River IS running and inserting, but the queue the jobs
// land on is not configured on the client, so nothing is ever worked. Every
// read API stays correct while the money stops.
func TestProgressMonitor_DetectsQueueNotDraining(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	dbtest.EnsureTestMerchant(dbtest.WithTestMerchant(ctx), t, dbi.Pool())
	cleanupWorkerHealth(t, dbi, progKind, "openrails.river_fleet")
	clearRiverJobs(t)
	t.Cleanup(func() { clearRiverJobs(t) })

	regs := NewWorkerRegistrations()
	regs.NoteKind(progKind)
	regs.NotePeriod(progKind, time.Second)
	monitor := newProgressMonitor(t, dbi, regs)

	unservedQueue := "test_unserved_" + uuid.NewString()[:8]
	workers := river.NewWorkers()
	require.NoError(t, river.AddWorkerSafely(workers, &progWorker{}))
	client, err := river.NewClient(riverpgxv5.New(dbtest.SharedPGXPool(t)), &river.Config{
		// Only the default queue is served; the periodic job lands elsewhere.
		Queues:            map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 2}},
		Workers:           workers,
		FetchCooldown:     10 * time.Millisecond,
		FetchPollInterval: 50 * time.Millisecond,
	})
	require.NoError(t, err)
	client.PeriodicJobs().Add(river.NewPeriodicJob(
		river.PeriodicInterval(time.Second),
		func() (river.JobArgs, *river.InsertOpts) {
			return progArgs{}, &river.InsertOpts{Queue: unservedQueue}
		},
		&river.PeriodicJobOpts{RunOnStart: true},
	))

	_, err = monitor.Check(ctx) // anchor the grace window
	require.NoError(t, err)

	require.NoError(t, client.Start(ctx))
	t.Cleanup(func() { _ = client.Stop(context.Background()) })

	require.Eventually(t, func() bool {
		return countRiverJobs(t, "available") > 0
	}, 30*time.Second, 100*time.Millisecond, "River must be inserting the periodic job")

	time.Sleep(2 * time.Second)
	report, err := monitor.Check(ctx)
	require.NoError(t, err)

	require.Positive(t, countRiverJobs(t, "available"), "work is queued")
	require.Equal(t, 0, countRiverJobs(t, "completed"), "and none of it is being worked")

	require.False(t, report.Progressing, "inserting without completing is not progress")
	require.Equal(t, ProgressNotCompleting, report.Reason)
	require.NotNil(t, report.FleetLastEnqueuedAt, "River IS alive — it is the workers that are not")
	require.Error(t, report.Err(), "the report renders as a host-facing error")
}

// TestProgressReport_ErrRendersStall pins the host-facing rendering used by
// health endpoints.
func TestProgressReport_ErrRendersStall(t *testing.T) {
	require.NoError(t, ProgressReport{Progressing: true}.Err())
	err := ProgressReport{Reason: ProgressNotScheduling}.Err()
	require.Error(t, err)
	require.ErrorContains(t, err, ProgressNotScheduling)
}
