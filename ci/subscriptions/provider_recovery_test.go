//go:build e2e && integration

package subscriptions_test

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/merchantarchive"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/open-rails/openrails/internal/providerrecovery"
	"github.com/stretchr/testify/require"
)

// A restored observation cursor is not evidence of an applied financial book.
// Startup readonly refresh catches provider receipts up while leaving both the
// provider and policy-held local subscription lifecycle untouched.
func TestReadonlyProviderRecoveryAppliesReceiptsWithoutDestruction(t *testing.T) {
	w := prepareWorld(t, 12, func(c *config.Config) { c.ProviderWriteMode = config.ProviderWriteModeReadOnly })
	w.declare = func(psps map[string]openrails.PSPConfig) { delete(psps, "stripe"); delete(psps, "ccbill") }
	w.start()
	w.jobs.PeriodicJobs().Clear()
	l := importLegacy(t, w, "nmi", embedded)
	end := l.periodEnd()
	mid := w.client[embedded].MerchantID()
	w.stop()
	w.advance(end.Add(time.Hour).Sub(w.clock.Now()))
	// This legacy provider schedule carries an exact local subscription order;
	// its receipt remains attributable even after the remote schedule ends.
	w.nmi.EditSchedule(l.railSub, func(schedule *nmimock.Schedule) { schedule.Order = l.sub.UUID().String() })
	_ = l.providerRenewal(true)
	renewal := w.nmi.LastSale().TransactionID
	w.nmi.DeleteSchedule(l.railSub) // provider truth may differ; readonly must not cancel locally
	w.advance(5 * 24 * time.Hour)
	_, err := w.pool.Exec(t.Context(), w.q(`INSERT INTO billing.psp_refresh_watermarks(merchant_id,psp_id,event_domain,watermark_at) VALUES($1,$2,'events',$3) ON CONFLICT(merchant_id,psp_id,event_domain) DO UPDATE SET watermark_at=EXCLUDED.watermark_at`), mid.UUID(), w.psp["nmi"].UUID(), w.clock.Now())
	require.NoError(t, err)
	before := len(w.nmi.Calls())
	readsBefore := w.nmi.Reads()
	w.start() // real RunOnStart, including bounded catch-up continuation
	require.Eventually(t, func() bool {
		var count int
		err := w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.payments WHERE transaction_id=$1 AND psp_id=$2`), renewal, w.psp["nmi"].UUID()).Scan(&count)
		return err == nil && count == 1
	}, 60*time.Second, 50*time.Millisecond, "startup readonly refresh records the provider's paid receipt")
	require.Eventually(t, func() bool {
		var covered time.Time
		err := w.pool.QueryRow(t.Context(), w.q(`SELECT watermark_at FROM billing.psp_refresh_watermarks WHERE merchant_id=$1 AND psp_id=$2 AND event_domain='completed_events'`), mid.UUID(), w.psp["nmi"].UUID()).Scan(&covered)
		return err == nil && !covered.Before(w.clock.Now().Add(-5*time.Minute))
	}, 60*time.Second, 50*time.Millisecond, "only complete applied windows advance financial coverage")
	sub := w.subscription(embedded, l.sub)
	require.Equal(t, billing.SubscriptionActive, sub.Status, "provider cancellation stays policy-held")
	require.Nil(t, sub.CanceledAt)
	require.Zero(t, w.nmi.ScheduleDeletes(l.railSub), "no remote delete in readonly")
	require.Zero(t, len(w.nmi.Attempts()), "no recovery charge in readonly")
	require.Equal(t, before, len(w.nmi.Calls()), "read-only performs no provider mutations")
	require.Greater(t, w.nmi.Reads()["query:transaction"], readsBefore["query:transaction"], "provider transactions were actually read")
	require.Len(t, completed(w.payments(embedded, l.c.id)), 2)
	require.Empty(t, w.nmi.Unexpected())
}

// The backup is a real stopped merchant archive restored into another PostgreSQL
// database. Normal full-mode workers must not collect from its stale book while
// the bulk provider refresh is unavailable, even though exact-order lookup works.
func TestRestoredBookWaitsForProviderCatchupBeforeRenewal(t *testing.T) {
	for _, alreadyPaid := range []bool{false, true} {
		name := "unpaid_obligation"
		if alreadyPaid {
			name = "provider_paid_after_backup"
		}
		t.Run(name, func(t *testing.T) { testRestoredProviderBook(t, alreadyPaid) })
	}
}

func testRestoredProviderBook(t *testing.T, alreadyPaid bool) {
	source := prepareWorld(t, 12)
	source.declare = func(psps map[string]openrails.PSPConfig) { delete(psps, "stripe"); delete(psps, "ccbill") }
	source.start()
	member := enroll(t, source, "nmi", embedded)
	mid := source.client[embedded].MerchantID()
	end := member.periodEnd()
	source.settle()
	source.checkMoneyInvariants()
	for {
		events, err := source.client[embedded].ListHostEvents(t.Context(), billing.HostEventListParams{})
		require.NoError(t, err)
		if len(events.Items) == 0 {
			break
		}
		for _, event := range events.Items {
			_, err := source.client[embedded].AcknowledgeHostEvent(t.Context(), event.ID)
			require.NoError(t, err)
		}
	}
	source.stop()
	source.waive("recorded", "the stopped backup retains pre-recovery payments; the restored destination owns subsequent provider activity")
	sourceDB, err := db.NewWithPGXPool(source.pool, source.schema)
	require.NoError(t, err)
	var archive bytes.Buffer
	require.NoError(t, merchantarchive.Export(t.Context(), sourceDB, mid, &archive))
	if alreadyPaid {
		// The original live database charges through its normal worker after
		// the backup. Its accepted operation is therefore absent from restore.
		source.advance(end.Add(time.Minute).Sub(source.clock.Now()))
		source.start()
		source.pull()
		source.runRenewals()
		source.until(func() bool { return len(source.nmi.Ledger("")) == 2 }, "original runtime collects after the backup")
		source.stop()
	}
	target := handoffTarget(t, source)
	target.declare = source.declare
	targetDB, err := db.NewWithPGXPool(target.pool, target.schema)
	require.NoError(t, err)
	directory, err := merchants.NewDirectoryService(targetDB.DataPool())
	require.NoError(t, err)
	_, _, err = directory.RegisterForRestore(t.Context(), mid, target.slug)
	require.NoError(t, err)
	_, err = merchantarchive.Restore(t.Context(), targetDB, mid, bytes.NewReader(archive.Bytes()))
	require.NoError(t, err)
	target.advance(end.Add(5 * 24 * time.Hour).Sub(target.clock.Now()))
	target.nmi.Intercept(func(r *http.Request) bool {
		if !strings.HasSuffix(r.URL.Path, "/query.php") {
			return false
		}
		form, err := url.ParseQuery(readBody(r))
		return err == nil && form.Get("report_type") == "transaction" && form.Get("order_id") == "" && form.Get("transaction_id") == ""
	}, func(r *http.Request, _ func() *http.Response) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("provider history temporarily unavailable")), Request: r}, nil
	})
	t.Cleanup(target.nmi.ClearIntercepts)
	tick := time.NewTicker(100 * time.Millisecond)
	stopped := make(chan struct{})
	go func() {
		for {
			select {
			case <-tick.C:
				target.advance(100 * time.Millisecond)
			case <-stopped:
				return
			}
		}
	}()
	t.Cleanup(func() { tick.Stop(); close(stopped) })
	target.start()
	require.Eventually(t, func() bool {
		var count int
		err := target.pool.QueryRow(t.Context(), target.q(`SELECT count(*) FROM billing.maintenance_runs WHERE kind='reconciliation' AND status='failed'`)).Scan(&count)
		return err == nil && count > 0
	}, 30*time.Second, 30*time.Millisecond, "normal startup attempted real provider catch-up")
	require.Eventually(t, func() bool {
		var count int
		err := target.pool.QueryRow(t.Context(), target.q(`SELECT count(*) FROM billing.river_job WHERE kind='openrails.dunning' AND state='completed'`)).Scan(&count)
		return err == nil && count > 0
	}, 30*time.Second, 30*time.Millisecond, "normal due worker evaluated the restored obligation")
	expectedCharges := 1
	if alreadyPaid {
		expectedCharges = 2
		require.Eventually(t, func() bool {
			var count int
			err := target.pool.QueryRow(t.Context(), target.q(`SELECT count(*) FROM billing.payments WHERE subscription_id=$1 AND status='completed'`), subUUID(member.sub)).Scan(&count)
			return err == nil && count == 2
		}, 30*time.Second, 30*time.Millisecond, "exact canonical readback settles the existing payment without a local submission fence")
		var fenced int
		require.NoError(t, target.pool.QueryRow(t.Context(), target.q(`SELECT count(*) FROM billing.provider_intents WHERE subscription_id=$1 AND intent_type='subscription_collection' AND result_evidence ? 'submitted_at'`), subUUID(member.sub)).Scan(&fenced))
		require.Zero(t, fenced, "restored receipt recovery never fabricates submission")
	} else {
		require.True(t, end.Equal(*target.subscription(embedded, member.sub).CurrentPeriodEndsAt))
	}
	require.Len(t, target.nmi.Attempts(), expectedCharges, "working exact-order lookup cannot authorize a new charge before bulk catch-up")
	require.ErrorIs(t, providerrecovery.CheckPSP(t.Context(), targetDB, mid.UUID(), target.psp["nmi"].UUID(), target.clock.Now()), providerrecovery.ErrPending)
	target.nmi.ClearIntercepts()
	require.Eventually(t, func() bool {
		var count int
		err := target.pool.QueryRow(t.Context(), target.q(`SELECT count(*) FROM billing.payments WHERE subscription_id=$1 AND status='completed'`), subUUID(member.sub)).Scan(&count)
		return err == nil && count == 2
	}, 100*time.Second, 50*time.Millisecond, "configured full mode automatically resumes after verified catch-up")
	require.Len(t, target.nmi.Attempts(), 2)
	require.Len(t, target.nmi.Ledger(""), 2)
	require.Eventually(t, func() bool {
		return providerrecovery.CheckPSP(t.Context(), targetDB, mid.UUID(), target.psp["nmi"].UUID(), target.clock.Now()) == nil
	}, 60*time.Second, 50*time.Millisecond, "catch-up completes after the provider is reachable")
	require.True(t, end.Add(monthHours*time.Hour).Equal(*target.subscription(embedded, member.sub).CurrentPeriodEndsAt))
	require.Empty(t, target.nmi.Unexpected())
}

func TestRecoveryGateRequiresCompletedAccountCoverage(t *testing.T) {
	w := prepareWorld(t, 12)
	w.declare = func(psps map[string]openrails.PSPConfig) { delete(psps, "stripe"); delete(psps, "ccbill") }
	w.start()
	_ = enroll(t, w, "nmi", embedded)
	mid := w.client[embedded].MerchantID()
	psp := w.psp["nmi"]
	w.stop()
	w.advance(5 * 24 * time.Hour)
	database, err := db.NewWithPGXPool(w.pool, w.schema)
	require.NoError(t, err)
	_, err = w.pool.Exec(t.Context(), w.q(`DELETE FROM billing.psp_refresh_watermarks WHERE merchant_id=$1`), mid.UUID())
	require.NoError(t, err)
	check := func() error {
		return providerrecovery.CheckPSP(t.Context(), database, mid.UUID(), psp.UUID(), w.clock.Now())
	}
	require.ErrorIs(t, check(), providerrecovery.ErrPending)
	stamp := func(domain string, at time.Time) {
		_, err := w.pool.Exec(t.Context(), w.q(`INSERT INTO billing.psp_refresh_watermarks(merchant_id,psp_id,event_domain,watermark_at) VALUES($1,$2,$3,$4) ON CONFLICT(merchant_id,psp_id,event_domain) DO UPDATE SET watermark_at=EXCLUDED.watermark_at`), mid.UUID(), psp.UUID(), domain, at)
		require.NoError(t, err)
	}
	stamp("events", w.clock.Now())
	stamp("applied_events", w.clock.Now().Add(-10*time.Minute))
	require.ErrorIs(t, check(), providerrecovery.ErrPending, "an advisory or partial applied cursor never authorizes a write")
	stamp("completed_events", w.clock.Now().Add(-5*time.Minute))
	require.NoError(t, check())
	// Starting another ordinary client does not clear shared completed coverage.
	w.start()
	require.NoError(t, check())
	w.stop()
	stamp("completed_events", w.clock.Now().Add(time.Hour))
	require.ErrorIs(t, check(), providerrecovery.ErrPending, "future observation timestamps are not freshness")
	stamp("completed_events", w.clock.Now().Add(-5*time.Minute))
	_, err = w.pool.Exec(t.Context(), w.q(`INSERT INTO billing.reconciliation_findings(merchant_id,psp_id,rail,finding_type,subject_key,severity,status,resolved_at,resolution,recommended_action,evidence) VALUES($1,$2,'nmi','pull.charge.missing','unqualified-invoice','high','ignored',now(),'ignored','qualify actual receipt','{}')`), mid.UUID(), psp.UUID())
	require.NoError(t, err)
	require.ErrorIs(t, check(), providerrecovery.ErrPending, "new unresolved financial evidence overrides even recent complete coverage")
	require.Len(t, w.nmi.Ledger(""), 1)
}
