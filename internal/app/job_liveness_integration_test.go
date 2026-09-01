//go:build integration

package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	riverpgxv5 "github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/require"

	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/open-rails/openrails/internal/shared/progress"
)

// xs-007 row 31, through the REAL registration path (addTrackedWorker) and a
// REAL River client: an OpenRails job is never cancelled by a clock, and a
// wedged one is cancelled by observed silence.

const (
	lvSlowKind   = "test.lv_slow_with_progress"
	lvWedgedKind = "test.lv_wedged"
	lvBusyKind   = "test.lv_busy"
)

type lvSlowArgs struct {
	RunFor time.Duration `json:"run_for"`
}

func (lvSlowArgs) Kind() string { return lvSlowKind }

// lvSlowWorker runs for RunFor, reporting progress every second, and honours
// its context — exactly the shape of a dunning pass over many merchants.
type lvSlowWorker struct {
	river.WorkerDefaults[lvSlowArgs]
}

func (lvSlowWorker) Work(ctx context.Context, job *river.Job[lvSlowArgs]) error {
	deadline := time.Now().Add(job.Args.RunFor)
	for i := 0; time.Now().Before(deadline); i++ {
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-time.After(time.Second):
		}
		progress.Mark(ctx, "unit "+time.Duration(i).String())
	}
	return nil
}

type lvWedgedArgs struct{}

func (lvWedgedArgs) Kind() string { return lvWedgedKind }

// lvWedgedWorker never reports progress and never returns on its own — a
// loop that stopped moving. It honours its context, as every ctx-aware call
// in the tree does.
type lvWedgedWorker struct {
	river.WorkerDefaults[lvWedgedArgs]
}

func (lvWedgedWorker) Work(ctx context.Context, _ *river.Job[lvWedgedArgs]) error {
	<-ctx.Done()
	return context.Cause(ctx)
}

type lvBusyArgs struct {
	RunFor time.Duration `json:"run_for"`
}

func (lvBusyArgs) Kind() string { return lvBusyKind }

// lvBusyWorker is the control for the wedged worker: same thresholds, but it
// marks progress often, so it must complete.
type lvBusyWorker struct {
	river.WorkerDefaults[lvBusyArgs]
}

func (lvBusyWorker) Work(ctx context.Context, job *river.Job[lvBusyArgs]) error {
	deadline := time.Now().Add(job.Args.RunFor)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-time.After(100 * time.Millisecond):
		}
		progress.Mark(ctx, "tick")
	}
	return nil
}

func lvCleanup(t *testing.T, rt *Runtime, kinds ...string) {
	t.Helper()
	clean := func() {
		ctx := context.Background()
		for _, kind := range kinds {
			_, _ = rt.DB.Pool().Exec(ctx, `DELETE FROM public.river_job WHERE kind = $1`, kind)
			_, _ = rt.DB.Pool().Exec(ctx, `DELETE FROM openrails.worker_health WHERE worker_kind = $1`, kind)
		}
	}
	clean()
	t.Cleanup(clean)
}

func lvStartClient(t *testing.T, rt *Runtime, workers *river.Workers, jobTimeout time.Duration) *river.Client[pgx.Tx] {
	t.Helper()
	client, err := river.NewClient(riverpgxv5.New(rt.DB.Pool()), &river.Config{
		Queues:            map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 2}},
		Workers:           workers,
		JobTimeout:        jobTimeout,
		FetchCooldown:     10 * time.Millisecond,
		FetchPollInterval: 50 * time.Millisecond,
	})
	require.NoError(t, err)
	require.NoError(t, client.Start(context.Background()))
	t.Cleanup(func() {
		stopCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		_ = client.Stop(stopCtx)
	})
	return client
}

func lvJobRow(t *testing.T, rt *Runtime, id int64) (state string, attemptedAt time.Time, errs string) {
	t.Helper()
	var at *time.Time
	require.NoError(t, rt.DB.Pool().QueryRow(context.Background(),
		`SELECT state::text, attempted_at, coalesce(errors::text, '') FROM public.river_job WHERE id = $1`, id).
		Scan(&state, &at, &errs))
	if at != nil {
		attemptedAt = *at
	}
	return
}

func lvAwaitFinal(t *testing.T, rt *Runtime, id int64, within time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		state, _, _ := lvJobRow(t, rt, id)
		switch state {
		case "completed", "discarded", "cancelled":
			return state
		}
		time.Sleep(100 * time.Millisecond)
	}
	state, _, errs := lvJobRow(t, rt, id)
	t.Fatalf("job %d not final within %s: state=%s errors=%s", id, within, state, errs)
	return ""
}

// TestJobLiveness_LongJobWithProgressOutlivesEveryClock: a job that runs past
// the client's JobTimeout — set to 2 s here, far harsher than River's own
// 1-minute default, so the proof is that the wrapper's -1 wins over ANY host
// value — and past the 60 s River default, completes, because it keeps
// reporting progress. Along the way the liveness beat (real cadence: one
// minute) has refreshed river_job.attempted_at, which is what keeps River's
// rescuer from calling a live job stuck.
func TestJobLiveness_LongJobWithProgressOutlivesEveryClock(t *testing.T) {
	appDB, _ := testRuntimeDB(t)
	rt := &Runtime{DB: appDB}
	lvCleanup(t, rt, lvSlowKind)

	workers := river.NewWorkers()
	require.NoError(t, addTrackedWorker(rt, workers, &lvSlowWorker{}))
	client := lvStartClient(t, rt, workers, 2*time.Second)

	const runFor = 66 * time.Second
	res, err := client.Insert(context.Background(), lvSlowArgs{RunFor: runFor}, &river.InsertOpts{MaxAttempts: 1})
	require.NoError(t, err)

	// Let River pick it up, then remember when the attempt started.
	var attemptStart time.Time
	require.Eventually(t, func() bool {
		state, at, _ := lvJobRow(t, rt, res.Job.ID)
		attemptStart = at
		return state == "running"
	}, 10*time.Second, 50*time.Millisecond)

	state := lvAwaitFinal(t, rt, res.Job.ID, runFor+30*time.Second)
	_, attemptedAt, errs := lvJobRow(t, rt, res.Job.ID)
	require.Equal(t, "completed", state, "errors: %s", errs)
	require.True(t, attemptedAt.Sub(attemptStart) >= riverjobs.JobLivenessBeat-5*time.Second,
		"liveness beat should have refreshed attempted_at (start %s, now %s)", attemptStart, attemptedAt)
}

// TestJobLiveness_NoProgressIsReaped_ProgressIsNot: with a 1 s declared
// cadence and multiplier 1, a job silent for a second is cancelled and the
// job row says why; a job with the same thresholds that keeps marking runs
// three times longer and completes. Same wrapper, same client, no clock.
func TestJobLiveness_NoProgressIsReaped_ProgressIsNot(t *testing.T) {
	appDB, _ := testRuntimeDB(t)
	rt := &Runtime{DB: appDB}
	lvCleanup(t, rt, lvWedgedKind, lvBusyKind)

	regs := rt.workerHealthRegistrations()
	regs.NotePeriod(lvWedgedKind, time.Second)
	regs.NotePeriod(lvBusyKind, time.Second)
	newLiveness := func() *riverjobs.JobLivenessMiddleware {
		return &riverjobs.JobLivenessMiddleware{
			River:           rt.riverTableAccess,
			Registrations:   regs,
			Beat:            200 * time.Millisecond,
			StaleMultiplier: 1,
			MinStale:        time.Second,
		}
	}

	workers := river.NewWorkers()
	require.NoError(t, addTrackedWorkerWithLiveness(rt, workers, &lvWedgedWorker{}, newLiveness()))
	require.NoError(t, addTrackedWorkerWithLiveness(rt, workers, &lvBusyWorker{}, newLiveness()))
	client := lvStartClient(t, rt, workers, -1)

	ctx := context.Background()
	wedged, err := client.Insert(ctx, lvWedgedArgs{}, &river.InsertOpts{MaxAttempts: 1})
	require.NoError(t, err)
	busy, err := client.Insert(ctx, lvBusyArgs{RunFor: 3 * time.Second}, &river.InsertOpts{MaxAttempts: 1})
	require.NoError(t, err)

	require.Equal(t, "discarded", lvAwaitFinal(t, rt, wedged.Job.ID, 20*time.Second))
	_, _, errs := lvJobRow(t, rt, wedged.Job.ID)
	require.Contains(t, errs, "reaped for no observed progress", "the job row names the reason")
	require.Contains(t, errs, "no progress reported since start")

	require.Equal(t, "completed", lvAwaitFinal(t, rt, busy.Job.ID, 20*time.Second))

	// The health row carries the same verdict (the health middleware is
	// outermost, so it records the reaper's error, not a bare cancellation).
	var lastErr string
	var streak int32
	require.NoError(t, rt.DB.Pool().QueryRow(ctx,
		`SELECT coalesce(last_error, ''), consecutive_failures FROM openrails.worker_health WHERE worker_kind = $1`, lvWedgedKind).
		Scan(&lastErr, &streak))
	require.EqualValues(t, 1, streak)
	require.True(t, strings.Contains(lastErr, "reaped for no observed progress"), "worker_health.last_error = %q", lastErr)
}
