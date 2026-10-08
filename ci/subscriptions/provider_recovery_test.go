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
		// Ignoring an alert is not financial resolution. Once a later pull
		// actually sees this now-recorded receipt, it must close the finding
		// from that proof instead of leaving the account held forever.
		_, err = target.pool.Exec(t.Context(), target.q(`INSERT INTO billing.reconciliation_findings(merchant_id,psp_id,rail,finding_type,subject_key,severity,status,resolved_at,resolution,operator_notes,recommended_action,evidence) VALUES($1,$2,'nmi','pull.charge.missing',$3,'high','ignored',now(),'ignored','notification muted during outage','qualify receipt','{}') ON CONFLICT(merchant_id,psp_id,finding_type,subject_key) DO UPDATE SET status='ignored',resolved_at=now(),resolution='ignored',operator_notes=EXCLUDED.operator_notes`), mid.UUID(), target.psp["nmi"].UUID(), target.nmi.LastSale().TransactionID)
		require.NoError(t, err)
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
	if alreadyPaid {
		var status, notes string
		require.NoError(t, target.pool.QueryRow(t.Context(), target.q(`SELECT status,operator_notes FROM billing.reconciliation_findings WHERE merchant_id=$1 AND psp_id=$2 AND finding_type='pull.charge.missing' AND subject_key=$3`), mid.UUID(), target.psp["nmi"].UUID(), target.nmi.LastSale().TransactionID).Scan(&status, &notes))
		require.Equal(t, "fixed", status)
		require.Equal(t, "notification muted during outage", notes)
	}
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

func TestStartupRecoversInvoiceOnlyBackupWithoutCharging(t *testing.T) {
	for _, mode := range []string{config.ProviderWriteModeReadOnly, config.ProviderWriteModeFull} {
		t.Run(string(mode), func(t *testing.T) {
			source := prepareWorld(t, 12)
			source.declare = func(psps map[string]openrails.PSPConfig) { delete(psps, "stripe"); delete(psps, "ccbill") }
			source.start()
			customer := source.newCustomer()
			method := customer.saveCard("nmi", visa)
			invoice := observedInvoice(t, source, customer, 50_000_000)
			mid := source.client[embedded].MerchantID()
			source.settle()
			source.stop()
			sourceDB, err := db.NewWithPGXPool(source.pool, source.schema)
			require.NoError(t, err)
			var archive bytes.Buffer
			require.NoError(t, merchantarchive.Export(t.Context(), sourceDB, mid, &archive))
			source.start()
			source.pull()
			status, body := customer.call(http.MethodPost, "/invoices/"+invoice.String()+"/pay-now", "after-backup", map[string]string{"payment_method_id": method})
			require.Equal(t, http.StatusOK, status, body)
			source.settle()
			require.Len(t, source.nmi.Ledger(""), 1)
			source.stop()
			target := handoffTarget(t, source)
			target.declare = source.declare
			target.cfg = func(cfg *config.Config) { cfg.ProviderWriteMode = mode }
			targetDB, err := db.NewWithPGXPool(target.pool, target.schema)
			require.NoError(t, err)
			directory, err := merchants.NewDirectoryService(targetDB.DataPool())
			require.NoError(t, err)
			_, _, err = directory.RegisterForRestore(t.Context(), mid, target.slug)
			require.NoError(t, err)
			_, err = merchantarchive.Restore(t.Context(), targetDB, mid, bytes.NewReader(archive.Bytes()))
			require.NoError(t, err)
			target.advance(5 * 24 * time.Hour)
			require.ErrorIs(t, providerrecovery.CheckPSP(t.Context(), targetDB, mid.UUID(), source.psp["nmi"].UUID(), target.clock.Now()), providerrecovery.ErrPending, "old unpaid invoice alone is an established book")
			target.start()
			require.Eventually(t, func() bool {
				paid, err := target.client[embedded].GetInvoice(t.Context(), invoice)
				return err == nil && paid.Status == billing.InvoicePaid && paid.AmountDue == 0
			}, 60*time.Second, 50*time.Millisecond, "normal startup refresh allocates the verified receipt without inventing an operation")
			require.Eventually(t, func() bool {
				return providerrecovery.CheckPSP(t.Context(), targetDB, mid.UUID(), target.psp["nmi"].UUID(), target.clock.Now()) == nil
			}, 60*time.Second, 50*time.Millisecond, "positive invoice recovery completes financial catch-up")
			var operations, allocations int
			require.NoError(t, target.pool.QueryRow(t.Context(), target.q(`SELECT count(*) FROM billing.provider_intents WHERE intent_type='invoice_collection'`)).Scan(&operations))
			require.Zero(t, operations)
			require.NoError(t, target.pool.QueryRow(t.Context(), target.q(`SELECT count(*) FROM billing.ledger_transfers WHERE operation='invoice_payment' AND invoice_id=$1`), invoice.UUID()).Scan(&allocations))
			require.Equal(t, 1, allocations)
			require.Len(t, target.nmi.Attempts(), 1)
			require.Len(t, target.nmi.Ledger(""), 1)
			require.Empty(t, target.nmi.Unexpected())
		})
	}
}

func TestReadonlyStripeRecoveryRetainsObservedCandidate(t *testing.T) {
	a, b, _ := copiedStripeBook(t)
	a.w.stripe.setClock(a.w.clock.Now)
	end := a.periodEnd()
	a.toPeriodEnd()
	a.w.pull()
	a.w.runRenewals()
	require.Len(t, a.providerLedger(), 2)
	payment := a.providerLedger()[1].ID
	provider := a.w.stripe
	a.w.stop()
	provider.mu.Lock()
	provider.chargesDown = true
	provider.intents[payment]["status"] = "processing"
	provider.mu.Unlock()
	t.Cleanup(func() { provider.mu.Lock(); provider.intents[payment]["status"] = "succeeded"; provider.mu.Unlock() })
	b.w.stop()
	b.w.advance(end.Add(time.Second).Sub(b.w.clock.Now()))
	b.w.start()
	b.w.runRenewals()
	require.Eventually(t, func() bool {
		var candidate string
		err := b.w.pool.QueryRow(t.Context(), b.w.q(`SELECT result_evidence->'collection_candidate'->>'transaction_id' FROM billing.provider_intents WHERE intent_type='subscription_collection' AND subscription_id=$1`), b.sub.UUID()).Scan(&candidate)
		return err == nil && candidate == payment
	}, 30*time.Second, 30*time.Millisecond, "an observed processing payment is retained before later reads")
	b.w.stop()
	b.w.cfg = func(cfg *config.Config) { cfg.ProviderWriteMode = config.ProviderWriteModeReadOnly }
	b.w.start()
	provider.mu.Lock()
	provider.chargesDown = false
	provider.visibleAt[payment] = provider.now().Add(48 * time.Hour)
	provider.intents[payment]["status"] = "succeeded"
	provider.mu.Unlock()
	b.w.until(func() bool { return b.periodEnd().After(end) }, "direct read of retained candidate recovers while customer listing omits it")
	require.Equal(t, 2, b.providerAttempts())
	var submitted int
	require.NoError(t, b.w.pool.QueryRow(t.Context(), b.w.q(`SELECT count(*) FROM billing.provider_intents WHERE intent_type='subscription_collection' AND result_evidence ? 'submitted_at'`)).Scan(&submitted))
	require.Zero(t, submitted)
}

func TestReadonlyStripeReversalRecordsMoneyBeforeCancellation(t *testing.T) {
	a, b, _ := copiedStripeBook(t)
	a.w.stripe.setClock(a.w.clock.Now)
	end := a.periodEnd()
	a.toPeriodEnd()
	a.w.pull()
	a.w.runRenewals()
	require.Len(t, a.providerLedger(), 2)
	transaction := a.providerLedger()[1].Charge
	var renewal billing.PaymentID
	for _, payment := range completed(a.w.payments(embedded, a.c.id)) {
		if payment.TransactionID == transaction {
			renewal = payment.ID
		}
	}
	require.NotEqual(t, billing.PaymentID{}, renewal)
	_, err := a.w.client[embedded].RefundPayment(t.Context(), renewal, billing.RefundPaymentParams{Full: true, Reason: "requested_by_customer", IdempotencyKey: "reversed-before-restore"})
	require.NoError(t, err)
	a.w.settle()
	a.w.stop()
	a.w.stripe.mu.Lock()
	a.w.stripe.chargesDown = true
	a.w.stripe.mu.Unlock()
	b.w.stop()
	b.w.advance(end.Add(time.Hour).Sub(b.w.clock.Now()))
	b.w.start()
	b.w.runRenewals()
	b.w.until(func() bool {
		var count int
		err := b.w.pool.QueryRow(t.Context(), b.w.q(`SELECT count(*) FROM billing.payments WHERE subscription_id=$1 AND transaction_id=$2`), b.sub.UUID(), transaction).Scan(&count)
		return err == nil && count == 1
	}, "confirmed money commits even though readonly holds cancellation")
	b.w.stop()
	b.w.cfg = func(cfg *config.Config) { cfg.ProviderWriteMode = config.ProviderWriteModeReadOnly }
	b.w.start()
	b.w.wake()
	require.NotEqual(t, billing.SubscriptionCanceled, b.w.subscription(embedded, b.sub).Status)
	var status string
	require.NoError(t, b.w.pool.QueryRow(t.Context(), b.w.q(`SELECT status FROM billing.provider_intents WHERE intent_type='subscription_collection' AND subscription_id=$1`), b.sub.UUID()).Scan(&status))
	require.Equal(t, "unknown_needs_verify", status, "original operation retains the pending lifecycle work")
	a.w.stripe.mu.Lock()
	a.w.stripe.chargesDown = false
	a.w.stripe.mu.Unlock()
	b.w.pull()
	b.w.stop()
	b.w.cfg = func(cfg *config.Config) { cfg.ProviderWriteMode = config.ProviderWriteModeFull }
	b.w.start()
	b.w.until(func() bool { return b.w.subscription(embedded, b.sub).Status == billing.SubscriptionCanceled }, "same retained operation finishes cancellation after policy allows it")
	require.Equal(t, 2, b.providerAttempts())
	require.NoError(t, b.w.pool.QueryRow(t.Context(), b.w.q(`SELECT status FROM billing.provider_intents WHERE intent_type='subscription_collection' AND subscription_id=$1`), b.sub.UUID()).Scan(&status))
	require.Equal(t, "succeeded", status)
}

func TestReadonlyWebhookRecordsRefundBeforeHeldCancellation(t *testing.T) {
	w := newWorld(t)
	e := enroll(t, w, "stripe", embedded)
	original := completed(w.payments(embedded, e.c.id))[0]
	charge := e.providerLedger()[0].Charge
	w.stripe.mu.Lock()
	status, raw := w.stripe.createRefund(url.Values{"charge": {charge}})
	w.stripe.mu.Unlock()
	require.Equal(t, http.StatusOK, status)
	refund := raw.(obj)
	w.cfg = func(cfg *config.Config) { cfg.ProviderWriteMode = config.ProviderWriteModeReadOnly }
	w.restart()
	notice := obj{"id": "evt_readonly_refund", "type": "refund.created", "created": time.Now().Unix(), "data": obj{"object": refund}}
	require.Equal(t, http.StatusInternalServerError, w.deliver("stripe", notice), "the provider retries policy-held lifecycle work")
	paid, err := w.client[embedded].GetPayment(t.Context(), original.ID)
	require.NoError(t, err)
	require.Equal(t, billing.PaymentRefunded, paid.Status, "authenticated financial facts committed before cancellation was held")
	require.Equal(t, paid.Amount, paid.AmountRefunded)
	require.NotEqual(t, billing.SubscriptionCanceled, w.subscription(embedded, e.sub).Status)
	w.cfg = func(cfg *config.Config) { cfg.ProviderWriteMode = config.ProviderWriteModeFull }
	w.restart()
	require.Equal(t, http.StatusOK, w.deliver("stripe", notice), "the same signed event finishes after policy permits")
	require.Equal(t, billing.SubscriptionCanceled, w.subscription(embedded, e.sub).Status)
	var refunds int
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.payments WHERE refunded_payment_id=$1`), original.ID.UUID()).Scan(&refunds))
	require.Equal(t, 1, refunds)
	require.Equal(t, 1, e.providerAttempts())
}
