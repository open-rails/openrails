//go:build integration

package riverjobs

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
)

func TestWorkerStateConcurrentHealthAndCursorPreserveFields(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.SharedPGXPool(t)
	q := gen.New(pool)
	kind := newWorkerStateKind(t, pool, "state")
	cursor := uuid.New()
	period := int64(60)
	now := time.Now().UTC().Truncate(time.Microsecond)
	start := make(chan struct{})
	results := make(chan error, 3)
	go func() {
		<-start
		_, err := q.SaveSweepCursor(ctx, gen.SaveSweepCursorParams{WorkerKind: kind, CursorMerchantID: &cursor})
		results <- err
	}()
	go func() {
		<-start
		results <- q.RecordWorkerSuccess(ctx, gen.RecordWorkerSuccessParams{WorkerKind: kind, Now: now})
	}()
	go func() {
		<-start
		results <- q.SeedWorkerHealth(ctx, gen.SeedWorkerHealthParams{WorkerKind: kind, ExpectedPeriodSeconds: &period})
	}()
	close(start)
	for range 3 {
		require.NoError(t, <-results)
	}
	// Reconstruct the reader as a restarted process would, then mutate the
	// opposite half of the row and verify both owners retain their state.
	restarted := gen.New(pool)
	failure := "failure after restart"
	stored, err := restarted.GetSweepCursor(ctx, kind)
	require.NoError(t, err)
	require.Equal(t, &cursor, stored.CursorMerchantID)
	require.EqualValues(t, 1, stored.CursorVersion)
	require.NoError(t, restarted.RecordWorkerFailure(ctx, gen.RecordWorkerFailureParams{WorkerKind: kind, Now: now, LastError: &failure}))
	n, err := restarted.SaveSweepCursor(ctx, gen.SaveSweepCursorParams{WorkerKind: kind, ExpectedCursorVersion: stored.CursorVersion})
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	row := readWorkerState(t, pool, kind)
	require.Nil(t, row.CursorMerchantID)
	require.EqualValues(t, 2, row.CursorVersion)
	require.NotNil(t, row.LastSuccessAt)
	require.True(t, now.Equal(*row.LastSuccessAt))
	require.NotNil(t, row.LastErrorAt)
	require.True(t, now.Equal(*row.LastErrorAt))
	require.EqualValues(t, 1, row.ConsecutiveFailures)
	require.Equal(t, &period, row.ExpectedPeriodSeconds)
}

// healthOutcome is one job completion as the middleware records it: the
// completion time it observed and, for a failure, the error text.
type healthOutcome struct {
	at  time.Time
	err string // "" = success
}

func (o healthOutcome) apply(ctx context.Context, q *gen.Queries, kind string) error {
	if o.err == "" {
		return q.RecordWorkerSuccess(ctx, gen.RecordWorkerSuccessParams{WorkerKind: kind, Now: o.at})
	}
	msg := o.err
	return q.RecordWorkerFailure(ctx, gen.RecordWorkerFailureParams{WorkerKind: kind, Now: o.at, LastError: &msg})
}

type healthExpectation struct {
	lastSuccessAt, lastErrorAt, updatedAt *time.Time
	lastError                             *string
	consecutiveFailures                   int32
}

func requireHealth(t *testing.T, row gen.OpenrailsWorkerState, want healthExpectation) {
	t.Helper()
	requireTimePtr(t, want.lastSuccessAt, row.LastSuccessAt, "last_success_at")
	requireTimePtr(t, want.lastErrorAt, row.LastErrorAt, "last_error_at")
	require.Equal(t, want.lastError, row.LastError, "last_error")
	require.Equal(t, want.consecutiveFailures, row.ConsecutiveFailures, "consecutive_failures")
	if want.updatedAt != nil {
		require.True(t, want.updatedAt.Equal(row.UpdatedAt), "updated_at: want %s got %s", want.updatedAt, row.UpdatedAt)
	}
}

func requireTimePtr(t *testing.T, want, got *time.Time, field string) {
	t.Helper()
	if want == nil {
		require.Nil(t, got, field)
		return
	}
	require.NotNil(t, got, field)
	require.True(t, want.Equal(*got), "%s: want %s got %s", field, want, got)
}

// Job completions of one kind race the upsert (each computes its completion
// time, then writes), so writes reach worker_state in any order. Every field
// must converge on the true newest state regardless of arrival order.
func TestWorkerStateHealthWritesAreMonotonicUnderOutOfOrderJobs(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.SharedPGXPool(t)
	q := gen.New(pool)
	base := time.Now().UTC().Truncate(time.Microsecond)
	at := func(sec int) time.Time { return base.Add(time.Duration(sec) * time.Second) }
	tp := func(sec int) *time.Time { v := at(sec); return &v }
	sp := func(s string) *string { return &s }

	// Concurrent, shuffled arrival. These sequences have exactly one
	// order-independent answer (all failures after, or all before, the
	// successes), so every interleaving must land on it.
	concurrent := []struct {
		name     string
		outcomes []healthOutcome
		want     healthExpectation
	}{
		{
			name:     "failures after the last success all count",
			outcomes: []healthOutcome{{at(1), ""}, {at(2), ""}, {at(3), "e3"}, {at(4), "e4"}, {at(5), "e5"}},
			want:     healthExpectation{lastSuccessAt: tp(2), lastErrorAt: tp(5), lastError: sp("e5"), consecutiveFailures: 3, updatedAt: tp(5)},
		},
		{
			name:     "successes after the last failure clear the streak",
			outcomes: []healthOutcome{{at(1), "e1"}, {at(2), "e2"}, {at(3), ""}, {at(4), ""}},
			want:     healthExpectation{lastSuccessAt: tp(4), lastErrorAt: tp(2), lastError: sp("e2"), consecutiveFailures: 0, updatedAt: tp(4)},
		},
	}
	for _, tc := range concurrent {
		t.Run(tc.name, func(t *testing.T) {
			for round := range 8 {
				kind := newWorkerStateKind(t, pool, fmt.Sprintf("mono%d", round))
				order := rand.Perm(len(tc.outcomes))
				start := make(chan struct{})
				errs := make(chan error, len(order))
				var wg sync.WaitGroup
				for _, i := range order {
					wg.Add(1)
					go func(o healthOutcome) {
						defer wg.Done()
						<-start
						errs <- o.apply(ctx, q, kind)
					}(tc.outcomes[i])
				}
				close(start)
				wg.Wait()
				close(errs)
				for err := range errs {
					require.NoError(t, err)
				}
				requireHealth(t, readWorkerState(t, pool, kind), tc.want)
			}
		})
	}

	// Deterministic stale arrivals, one rule per case.
	sequential := []struct {
		name     string
		outcomes []healthOutcome // arrival order
		want     healthExpectation
	}{
		{
			name:     "stale success cannot move last_success_at back",
			outcomes: []healthOutcome{{at(5), ""}, {at(3), ""}},
			want:     healthExpectation{lastSuccessAt: tp(5), consecutiveFailures: 0, updatedAt: tp(5)},
		},
		{
			name:     "stale failure keeps the newest error text and time",
			outcomes: []healthOutcome{{at(5), "new"}, {at(3), "old"}},
			want:     healthExpectation{lastErrorAt: tp(5), lastError: sp("new"), consecutiveFailures: 2, updatedAt: tp(5)},
		},
		{
			name:     "stale success cannot reset a newer failure",
			outcomes: []healthOutcome{{at(5), "e5"}, {at(3), ""}},
			want:     healthExpectation{lastSuccessAt: tp(3), lastErrorAt: tp(5), lastError: sp("e5"), consecutiveFailures: 1, updatedAt: tp(5)},
		},
		{
			name:     "stale failure cannot count against a newer success",
			outcomes: []healthOutcome{{at(5), ""}, {at(3), "e3"}},
			want:     healthExpectation{lastSuccessAt: tp(5), lastErrorAt: tp(3), lastError: sp("e3"), consecutiveFailures: 0, updatedAt: tp(5)},
		},
		{
			name:     "newer success resets, newer failure counts",
			outcomes: []healthOutcome{{at(1), "e1"}, {at(2), ""}, {at(3), "e3"}, {at(4), "e4"}},
			want:     healthExpectation{lastSuccessAt: tp(2), lastErrorAt: tp(4), lastError: sp("e4"), consecutiveFailures: 2, updatedAt: tp(4)},
		},
		{
			name:     "same-instant success and failure: the later arrival decides",
			outcomes: []healthOutcome{{at(1), "e1"}, {at(1), ""}},
			want:     healthExpectation{lastSuccessAt: tp(1), lastErrorAt: tp(1), lastError: sp("e1"), consecutiveFailures: 0, updatedAt: tp(1)},
		},
	}
	for _, tc := range sequential {
		t.Run(tc.name, func(t *testing.T) {
			kind := newWorkerStateKind(t, pool, "seq")
			for _, o := range tc.outcomes {
				require.NoError(t, o.apply(ctx, q, kind))
			}
			requireHealth(t, readWorkerState(t, pool, kind), tc.want)
		})
	}

	// A late success between recorded failures cannot retract the older ones
	// it should have ended: the streak may over-count, never under-count, so a
	// consecutive-failure alert is never suppressed by reordering.
	t.Run("late success never under-counts the streak", func(t *testing.T) {
		kind := newWorkerStateKind(t, pool, "bound")
		for _, o := range []healthOutcome{{at(2), "e2"}, {at(3), "e3"}, {at(5), "e5"}, {at(4), ""}} {
			require.NoError(t, o.apply(ctx, q, kind))
		}
		row := readWorkerState(t, pool, kind)
		requireHealth(t, row, healthExpectation{lastSuccessAt: tp(4), lastErrorAt: tp(5), lastError: sp("e5"), consecutiveFailures: 3, updatedAt: tp(5)})
		require.GreaterOrEqual(t, row.ConsecutiveFailures, int32(1), "true streak since the success at t4 is 1")
	})

	t.Run("stale alert mark cannot move last_alerted_at back", func(t *testing.T) {
		kind := newWorkerStateKind(t, pool, "alert")
		require.NoError(t, q.SeedWorkerHealth(ctx, gen.SeedWorkerHealthParams{WorkerKind: kind}))
		require.NoError(t, q.MarkWorkerHealthAlerted(ctx, gen.MarkWorkerHealthAlertedParams{WorkerKind: kind, Now: at(5)}))
		require.NoError(t, q.MarkWorkerHealthAlerted(ctx, gen.MarkWorkerHealthAlertedParams{WorkerKind: kind, Now: at(3)}))
		row := readWorkerState(t, pool, kind)
		requireTimePtr(t, tp(5), row.LastAlertedAt, "last_alerted_at")
		require.True(t, at(5).Equal(row.UpdatedAt), "updated_at")
	})
}

// The sweep cursor is a ring position (it wraps inside a pass), so it cannot
// be ordered against its predecessor; a save is a compare-and-swap on the
// opaque version the pass read. A pass that finishes after a newer pass moved
// the cursor must not move it back, and health writes never disturb the version.
func TestSweepCursorSaveIsCompareAndSwapOnTheVersionRead(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.SharedPGXPool(t)
	q := gen.New(pool)
	kind := newWorkerStateKind(t, pool, "cursor")
	logger := log.WithField("test", t.Name())

	// Two passes read the same (absent) cursor and race to save.
	first, err := loadSweepCursor(ctx, q, kind)
	require.NoError(t, err)
	require.Nil(t, first.CursorMerchantID)
	require.Zero(t, first.CursorVersion)
	a, b := uuid.New(), uuid.New()
	start := make(chan struct{})
	type saved struct {
		next uuid.UUID
		rows int64
	}
	results := make(chan saved, 2)
	for _, next := range []uuid.UUID{a, b} {
		go func(next uuid.UUID) {
			<-start
			n, err := q.SaveSweepCursor(ctx, gen.SaveSweepCursorParams{WorkerKind: kind, CursorMerchantID: &next, ExpectedCursorVersion: first.CursorVersion})
			require.NoError(t, err)
			results <- saved{next, n}
		}(next)
	}
	close(start)
	var winner *uuid.UUID
	for range 2 {
		r := <-results
		if r.rows == 1 {
			require.Nil(t, winner, "exactly one racing save may apply")
			winner = &r.next
		} else {
			require.EqualValues(t, 0, r.rows)
		}
	}
	require.NotNil(t, winner, "one racing save must apply")
	second, err := loadSweepCursor(ctx, q, kind)
	require.NoError(t, err)
	require.Equal(t, winner, second.CursorMerchantID)
	require.EqualValues(t, 1, second.CursorVersion)

	// A late pass that read the pre-race cursor cannot move it back.
	stale := uuid.New()
	saveSweepCursor(ctx, q, kind, first, &stale, logger)
	require.Equal(t, second, mustLoadSweepCursor(t, ctx, q, kind))

	// The token is a counter, so its uniqueness never depends on the clock
	// advancing between saves (a timestamp token repeats when two saves share
	// a tick, or the clock steps back). Read->save cycles as fast as the
	// database answers must yield strictly consecutive versions, and the token
	// read before each save must be dead the moment that save applies.
	prev := second
	for i := range 32 {
		next := uuid.New()
		saveSweepCursor(ctx, q, kind, prev, &next, logger)
		cur := mustLoadSweepCursor(t, ctx, q, kind)
		require.Equal(t, &next, cur.CursorMerchantID, "cycle %d", i)
		require.Equal(t, prev.CursorVersion+1, cur.CursorVersion, "cycle %d", i)
		n, err := q.SaveSweepCursor(ctx, gen.SaveSweepCursorParams{WorkerKind: kind, CursorMerchantID: &stale, ExpectedCursorVersion: prev.CursorVersion})
		require.NoError(t, err)
		require.Zero(t, n, "cycle %d: the token read before the save must not apply after it", i)
		require.Equal(t, cur, mustLoadSweepCursor(t, ctx, q, kind), "cycle %d", i)
		prev = cur
	}
	second = prev
	// Every token ever issued for this kind, not just the previous one, is dead.
	for v := range second.CursorVersion {
		n, err := q.SaveSweepCursor(ctx, gen.SaveSweepCursorParams{WorkerKind: kind, CursorMerchantID: &stale, ExpectedCursorVersion: v})
		require.NoError(t, err)
		require.Zero(t, n, "version %d", v)
	}
	require.Equal(t, second, mustLoadSweepCursor(t, ctx, q, kind))

	// Health bookkeeping on the same row leaves the cursor version alone.
	require.NoError(t, q.RecordWorkerSuccess(ctx, gen.RecordWorkerSuccessParams{WorkerKind: kind, Now: time.Now().UTC()}))
	msg := "boom"
	require.NoError(t, q.RecordWorkerFailure(ctx, gen.RecordWorkerFailureParams{WorkerKind: kind, Now: time.Now().UTC(), LastError: &msg}))
	require.Equal(t, second, mustLoadSweepCursor(t, ctx, q, kind))

	// The pass that read the current version parks the ring (drained) and
	// bumps the version; the version it read is now stale for everyone else.
	saveSweepCursor(ctx, q, kind, second, nil, logger)
	third := mustLoadSweepCursor(t, ctx, q, kind)
	require.Nil(t, third.CursorMerchantID)
	require.Equal(t, second.CursorVersion+1, third.CursorVersion)
	n, err := q.SaveSweepCursor(ctx, gen.SaveSweepCursorParams{WorkerKind: kind, CursorMerchantID: &stale, ExpectedCursorVersion: second.CursorVersion})
	require.NoError(t, err)
	require.EqualValues(t, 0, n)
	require.Equal(t, third, mustLoadSweepCursor(t, ctx, q, kind))

	// The health writer creating the row first (version 0 by default) is the
	// same state as no row: the first cursor save applies and takes version 1.
	seeded := newWorkerStateKind(t, pool, "seeded")
	require.NoError(t, q.RecordWorkerSuccess(ctx, gen.RecordWorkerSuccessParams{WorkerKind: seeded, Now: time.Now().UTC()}))
	read := mustLoadSweepCursor(t, ctx, q, seeded)
	require.Zero(t, read.CursorVersion)
	n, err = q.SaveSweepCursor(ctx, gen.SaveSweepCursorParams{WorkerKind: seeded, CursorMerchantID: &a, ExpectedCursorVersion: read.CursorVersion})
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	seededCursor := mustLoadSweepCursor(t, ctx, q, seeded)
	require.Equal(t, &a, seededCursor.CursorMerchantID)
	require.EqualValues(t, 1, seededCursor.CursorVersion)
}

func mustLoadSweepCursor(t *testing.T, ctx context.Context, q *gen.Queries, kind string) gen.GetSweepCursorRow {
	t.Helper()
	row, err := loadSweepCursor(ctx, q, kind)
	require.NoError(t, err)
	return row
}

func newWorkerStateKind(t *testing.T, pool *pgxpool.Pool, label string) string {
	t.Helper()
	kind := "test." + label + "." + uuid.NewString()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM openrails.worker_state WHERE worker_kind=$1`, kind)
	})
	return kind
}

func readWorkerState(t *testing.T, pool *pgxpool.Pool, kind string) gen.OpenrailsWorkerState {
	t.Helper()
	rows, err := gen.New(pool).ListWorkerHealth(context.Background())
	require.NoError(t, err)
	for _, row := range rows {
		if row.WorkerKind == kind {
			return row
		}
	}
	t.Fatalf("worker state %s missing", kind)
	return gen.OpenrailsWorkerState{}
}
