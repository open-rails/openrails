//go:build e2e && integration

package subscriptions_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/merchantarchive"
	"github.com/open-rails/openrails/internal/nmimock"
	riverjobs "github.com/open-rails/openrails/internal/river"
)

// exportArchive is the merchant's archive as an operator exports it.
func (w *world) exportArchive() error {
	w.t.Helper()
	source, err := db.NewWithPGXPool(w.pool, w.schema)
	require.NoError(w.t, err)
	var archive bytes.Buffer
	return merchantarchive.Export(w.t.Context(), source, w.client[embedded].MerchantID(), &archive)
}

// runningRuns counts the merchant's maintenance runs still marked running;
// rails narrows them to runs over that rail.
func (w *world) runningRuns(rails ...string) int {
	w.t.Helper()
	if rails == nil {
		rails = []string{}
	}
	var n int
	require.NoError(w.t, w.pool.QueryRow(w.t.Context(), w.q(`SELECT count(*) FROM billing.maintenance_runs WHERE status = 'running' AND rails @> $1`), rails).Scan(&n))
	return n
}

// insertingRun reports a reconciliation run insert of this world still
// running, sleeping in slowRunInserts' trigger.
func (w *world) insertingRun() bool {
	var n int
	require.NoError(w.t, w.pool.QueryRow(w.t.Context(), `SELECT count(*) FROM pg_stat_activity
		WHERE pid <> pg_backend_pid() AND wait_event = 'PgSleep' AND query ILIKE '%maintenance_runs%' AND query ILIKE '%' || $1 || '%'`, w.schema).Scan(&n))
	return n > 0
}

// slowRunInserts makes each reconciliation run insert take two seconds and
// finish even when its client cancels: the run commits after the pass that
// started it gave up, as an insert does whose answer a canceled client
// abandoned.
func (w *world) slowRunInserts() (remove func()) {
	w.t.Helper()
	_, err := w.pool.Exec(w.t.Context(), w.q(`CREATE FUNCTION billing.e2e_slow_run_insert() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			PERFORM pg_sleep(2);
			RETURN NEW;
		EXCEPTION WHEN query_canceled THEN
			RETURN NEW;
		END $$;
		CREATE TRIGGER e2e_slow_run_insert AFTER INSERT ON billing.maintenance_runs FOR EACH ROW WHEN (NEW.kind = 'reconciliation') EXECUTE FUNCTION billing.e2e_slow_run_insert();`))
	require.NoError(w.t, err)
	return func() {
		_, err := w.pool.Exec(context.Background(), w.q(`DROP TRIGGER e2e_slow_run_insert ON billing.maintenance_runs; DROP FUNCTION billing.e2e_slow_run_insert();`))
		require.NoError(w.t, err)
	}
}

// A provider refresh pass refuses the merchant archive while it runs; one
// stopped while it was still recording its run finishes that run, so the
// stopped deployment exports.
func TestArchiveAfterAStoppedRefreshPass(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := t.Context()
	refresh := func() {
		t.Helper()
		_, _, err := riverjobs.EnqueueMerchantRefresh(ctx, w.jobs, w.client[embedded].MerchantID().UUID(), openrails.QueueBilling)
		require.NoError(t, err)
	}
	w.settle()

	// A pass in flight: its run is running, and the archive refuses it.
	held := w.nmi.Hold(func(r *http.Request) bool { return true }, nmimock.HoldRequest)
	refresh()
	require.Eventually(t, func() bool { return w.runningRuns("nmi") > 0 }, 20*time.Second, 20*time.Millisecond, "the pass records its NMI run")
	var refused *merchantarchive.Error
	require.True(t, errors.As(w.exportArchive(), &refused), "an in-flight pass refuses the archive")
	require.Equal(t, "unsupported_state", refused.Code)
	require.Equal(t, "maintenance_runs", refused.Table)
	held.Release()
	w.nmi.ClearIntercepts()
	require.Eventually(t, func() bool { return w.runningRuns() == 0 }, 30*time.Second, 20*time.Millisecond, "the pass finishes its run")
	w.advance(48 * time.Hour) // new provider windows to read

	// The deployment stops while a pass's run insert is in flight; the
	// insert commits after River canceled the pass.
	restore := w.slowRunInserts()
	refresh()
	require.Eventually(t, w.insertingRun, 20*time.Second, 20*time.Millisecond, "the pass's run insert is in flight")
	w.stop()
	require.Eventually(t, func() bool { return !w.insertingRun() }, 20*time.Second, 20*time.Millisecond)
	restore()
	require.Zero(t, w.runningRuns(), "no run outlives its pass")
	require.NoError(t, w.exportArchive())
}
