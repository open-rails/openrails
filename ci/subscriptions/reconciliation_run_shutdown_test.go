//go:build e2e && integration

package subscriptions_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/failpoint"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchantarchive"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/reconcile/converge"
	"github.com/stretchr/testify/require"
)

// Cancel a real SQL operation on the merchant-pinned connection. Finalization
// must recover the canceled pin, not merely replace the context deadline.
type canceledRunFetcher struct {
	database *db.DB
	query    string
}

func (f canceledRunFetcher) Name() string { return "nmi" }
func (f canceledRunFetcher) Capabilities() reconcile.Capabilities {
	return reconcile.Capabilities{}
}
func (f canceledRunFetcher) Fetch(ctx context.Context, _ reconcile.FetchParams) (*reconcile.RemoteSnapshot, error) {
	var unused any
	err := f.database.Qx(ctx).QueryRow(ctx, f.query).Scan(&unused)
	return nil, err
}

func TestCanceledReconciliationRunDoesNotStrandArchive(t *testing.T) {
	w := newWorld(t)
	e := enroll(t, w, "nmi", embedded)
	w.refreshProviders()
	w.settleCollectionScans()
	w.settle()
	mid, psp := w.client[embedded].MerchantID(), w.psp["nmi"]
	for {
		events, err := w.client[embedded].ListHostEvents(t.Context(), billing.HostEventListParams{})
		require.NoError(t, err)
		if len(events.Items) == 0 {
			break
		}
		for _, event := range events.Items {
			_, err = w.client[embedded].AcknowledgeHostEvents(t.Context(), []billing.HostEventID{event.ID})
			require.NoError(t, err)
		}
	}
	require.NoError(t, w.jobs.Stop(t.Context()))
	w.stop()
	database, err := db.NewWithPGXPool(w.pool, w.schema)
	require.NoError(t, err)
	query := "SELECT pg_sleep(30) /* maintenance_finish_" + uuid.NewString() + " */"
	reader := reconcile.NewEngine(database, nil, map[reconcile.Provider]reconcile.RailFetcher{reconcile.ProviderNMI: canceledRunFetcher{database: database, query: query}}, nil)
	ctx, cancel := context.WithCancel(merchant.WithID(t.Context(), mid))
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- database.RunInMerchantConn(ctx, func(ctx context.Context) error {
			_, err := reader.Run(ctx, reconcile.RunParams{Mode: reconcile.ModeAdvisory, PSPs: map[reconcile.Provider]reconcile.PSPBinding{reconcile.ProviderNMI: {ID: psp.UUID(), Rail: "nmi", AccountID: nmiAcct}}})
			return err
		})
	}()
	require.Eventually(t, func() bool {
		var running bool
		err := w.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE query=$1 AND state='active' AND wait_event='PgSleep')`, query).Scan(&running)
		return err == nil && running
	}, 10*time.Second, 10*time.Millisecond)
	var run uuid.UUID
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT id FROM billing.maintenance_runs WHERE kind='reconciliation' AND status='running'`)).Scan(&run))
	var live bytes.Buffer
	err = merchantarchive.Export(t.Context(), database, mid, &live)
	var refusal *merchantarchive.Error
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, "maintenance_runs", refusal.Table, "a genuinely live run must still block export")
	cancel()
	select {
	case err := <-done:
		require.ErrorContains(t, err, "context canceled")
	case <-time.After(10 * time.Second):
		t.Fatal("canceled run did not finish its bounded bookkeeping")
	}
	var status, reason string
	var finished bool
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT status,finished_at IS NOT NULL,COALESCE(error,'') FROM billing.maintenance_runs WHERE id=$1`), run).Scan(&status, &finished, &reason))
	require.Equal(t, "failed", status)
	require.True(t, finished)
	require.Contains(t, reason, "context canceled")
	var archived bytes.Buffer
	require.NoError(t, merchantarchive.Export(t.Context(), database, mid, &archived))
	require.NotEmpty(t, archived.Bytes())
	var payments int
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.payments WHERE subscription_id=$1`), e.sub.UUID()).Scan(&payments))
	require.Equal(t, 1, payments, "run finalization changes no financial history")
	require.Equal(t, 1, e.providerAttempts())
}

func TestConvergenceFailureFinishesItsRun(t *testing.T) {
	w := newWorld(t)
	l := importLegacy(t, w, "nmi", embedded)
	require.NoError(t, w.jobs.Stop(t.Context()))
	runtime := engine.Graph(w.rt).Runtime
	runtime.Verifier.Close()
	w.advance(l.periodEnd().Sub(w.clock.Now()) + 3*day)
	mid := w.client[embedded].MerchantID()
	for _, canceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "repair_error", true: "cancellation"}[canceled], func(t *testing.T) {
			ctx, cancel := context.WithCancel(merchant.WithID(t.Context(), mid))
			defer cancel()
			failure := errors.New("deliberate repair failure")
			remove := failpoint.Set(func(_ context.Context, site failpoint.Site) error {
				if site.Point == failpoint.BeforeRepair && site.Subscription == l.sub.UUID() {
					if canceled {
						cancel()
						return ctx.Err()
					}
					return failure
				}
				return nil
			})
			defer remove()
			var result converge.ConvergeResult
			err := runtime.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
				var err error
				result, err = converge.NewConvergeEngine(runtime.DB, w.clock).Converge(ctx, converge.Scope{Merchant: mid})
				return err
			})
			if canceled {
				require.ErrorIs(t, err, context.Canceled)
			} else {
				require.ErrorIs(t, err, failure)
			}
			require.NotNil(t, result.RunID)
			var status, reason string
			require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT status,COALESCE(error,'') FROM billing.maintenance_runs WHERE id=$1 AND finished_at IS NOT NULL`), *result.RunID).Scan(&status, &reason))
			require.Equal(t, "failed", status)
			require.Contains(t, reason, err.Error())
			require.Equal(t, "active", w.lifeRow(l.sub).status, "failed bookkeeping does not certify or run a repair")
		})
	}
}
