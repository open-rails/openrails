//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/alerting"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/open-rails/openrails/internal/providerrecovery"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"
)

// A PSP without pull credentials has nothing to read, so it never makes a
// merchant's refresh incomplete: an NMI PSP beside a CCBill PSP declared with
// no credentials at all completes, and the CCBill PSP joins the first pass
// after it is armed. A card PSP that loses its credentials is skipped the same
// way while its recovery stays pending.
func TestProviderRefreshSkipsUnarmedPSPs(t *testing.T) {
	t.Parallel()
	dl := newDataLinkFake(t)
	w := prepareWorld(t, 12, func(c *config.Config) {
		c.ProviderSandbox = &config.ProviderSandboxConfig{CCBillDataLinkURL: dl.URL}
	})
	declare := func(ccbill, nmi map[string]string) func(map[string]openrails.PSPConfig) {
		return func(psps map[string]openrails.PSPConfig) {
			delete(psps, "stripe")
			for key, secrets := range map[string]map[string]string{"ccbill": ccbill, "nmi": nmi} {
				if secrets != nil {
					psp := psps[key]
					psp.Secrets = secrets
					psps[key] = psp
				}
			}
		}
	}
	w.declare = declare(map[string]string{}, nil)
	w.start()
	enroll(t, w, "nmi", embedded)
	member := importCCBill(t, w)
	mid := w.client[embedded].MerchantID()
	ctx := merchant.WithID(t.Context(), mid)
	db := engine.Graph(w.rt).Runtime.DB

	job := w.refreshProviders()
	require.Empty(t, job.Errors, "an unarmed CCBill PSP is not an incomplete provider")
	for _, domain := range []string{"events", "applied_events", "completed_events"} {
		require.NotNil(t, w.refreshWatermark(w.psp["nmi"], domain), "the armed NMI PSP refreshes: %s", domain)
	}
	require.Nil(t, w.refreshWatermark(w.psp["ccbill"], "events"), "nothing reads the unarmed CCBill PSP")
	require.NoError(t, providerrecovery.CheckPSP(ctx, db, mid.UUID(), w.psp["nmi"].UUID(), w.clock.Now()))
	require.Zero(t, w.workerFailures(riverjobs.KindProviderRefreshMerchant))

	nmiSecrets := w.psps["nmi"].Secrets
	armed := map[string]string{"salt": "e2e-ccbill-salt", "datalink_username": "e2e-datalink", "datalink_password": "e2e-datalink-password"}
	w.declare = declare(armed, nil)
	w.restart()
	db = engine.Graph(w.rt).Runtime.DB
	dl.list(member.railSub, ccbillDate(member.paidThrough), "")
	w.refreshProviders()
	require.NotNil(t, w.refreshWatermark(w.psp["ccbill"], "events"), "the armed CCBill PSP joins the next pass")

	unarmedNMI := map[string]string{}
	for key, value := range nmiSecrets {
		if key != "security_key" {
			unarmedNMI[key] = value
		}
	}
	w.declare = declare(armed, unarmedNMI)
	w.restart()
	db = engine.Graph(w.rt).Runtime.DB
	w.advance(providerrecovery.StaleAfter + time.Hour)
	nmiReads := w.nmi.Reads()["query:transaction"]
	job = w.refreshProviders()
	require.Empty(t, job.Errors, "an NMI PSP without its security key is not refreshable, not incomplete")
	require.Equal(t, nmiReads, w.nmi.Reads()["query:transaction"], "nothing reads the unarmed NMI PSP")
	require.ErrorIs(t, providerrecovery.CheckPSP(ctx, db, mid.UUID(), w.psp["nmi"].UUID(), w.clock.Now()), providerrecovery.ErrPending,
		"its book stays held until it is read")

	w.declare = declare(armed, nmiSecrets)
	w.restart()
	db = engine.Graph(w.rt).Runtime.DB
	w.refreshProviders()
	require.NoError(t, providerrecovery.CheckPSP(ctx, db, mid.UUID(), w.psp["nmi"].UUID(), w.clock.Now()), "re-armed, it recovers on the next pass")
	require.Zero(t, w.workerFailures(riverjobs.KindProviderRefreshMerchant))
}

// A refresh that read every window but found receipts only an operator can
// settle has done its work: the job completes, no stall is raised (a standing
// one resolves), the findings stay open and recovery stays pending, so a
// payment on the PSP is still refused. A failed read still fails the job.
func TestProviderRefreshCompletesWhileFindingsAwaitOperator(t *testing.T) {
	t.Parallel()
	w := prepareWorld(t, 12)
	w.declare = func(psps map[string]openrails.PSPConfig) { delete(psps, "stripe"); delete(psps, "ccbill") }
	w.start()
	w.refreshProviders()
	mid, psp := w.client[embedded].MerchantID(), w.psp["nmi"]
	ctx := merchant.WithID(t.Context(), mid)
	rt := engine.Graph(w.rt).Runtime
	require.NoError(t, providerrecovery.CheckPSP(ctx, rt.DB, mid.UUID(), psp.UUID(), w.clock.Now()))
	c := w.newCustomer()
	method := c.saveCard("nmi", visa)
	invoice := observedInvoice(t, w, c, 50_000_000)

	// The stall the old accounting raised on every retry stands open.
	kind := riverjobs.KindProviderRefreshMerchant
	_, err := w.pool.Exec(t.Context(), w.q(`UPDATE billing.worker_state SET last_error_at=$2, last_error='provider refresh incomplete', consecutive_failures=14 WHERE worker_kind=$1`), kind, w.clock.Now())
	require.NoError(t, err)
	monitor := &riverjobs.ProgressMonitor{DB: rt.DB, Clock: w.clock}
	require.NoError(t, monitor.RaiseAlerts(t.Context(), riverjobs.ProgressReport{CheckedAt: w.clock.Now(), Progressing: true,
		Kinds: []riverjobs.KindProgress{{Kind: kind, Reason: riverjobs.ProgressConsecutiveFailures}}}))
	require.Contains(t, w.openFindings(string(alerting.FindingWorkerStalled)), kind)

	// NMI bills a schedule the book never had; the operator dismisses it.
	tier := w.bookTier("stray", 999, 30)
	stray := w.nmi.AddSchedule(nmimock.Schedule{Vault: w.nmi.AddVault(visa), Plan: tier.plan, Amount: tier.amount, Days: 30, NextBilling: w.clock.Now().Add(5 * day)})
	w.armDestructive()
	completed := w.refreshWatermark(psp, "completed_events")
	w.advance(time.Hour)
	job := w.refreshProviders()
	require.Empty(t, job.Errors, "operator-actionable findings are not a failed read")
	require.Contains(t, w.openFindings(strayFinding), stray)
	findings, err := w.client[embedded].ListFindings(t.Context(), billing.FindingListParams{Type: strayFinding})
	require.NoError(t, err)
	require.Len(t, findings.Items, 1)
	_, err = w.dismiss("stray sandbox schedule", findings.Items[0].ID.String())
	require.NoError(t, err)
	require.NotContains(t, w.openFindings(strayFinding), stray)
	w.advance(time.Hour)
	job = w.refreshProviders()
	require.Empty(t, job.Errors, "a dismissed stray schedule still holds recovery without failing the job")

	require.Equal(t, completed, w.refreshWatermark(psp, "completed_events"), "held recovery never advances completed coverage")
	require.ErrorIs(t, providerrecovery.CheckPSP(ctx, rt.DB, mid.UUID(), psp.UUID(), w.clock.Now()), providerrecovery.ErrPending)
	answer := payNMIInvoice(t.Context(), w, c, invoice, method, "held-recovery")
	require.NoError(t, answer.err)
	require.Equal(t, http.StatusServiceUnavailable, answer.status, string(answer.body))
	require.Contains(t, string(answer.body), "billing is paused while provider recovery completes")
	require.Empty(t, w.nmi.Attempts(), "no charge while recovery is held")

	require.Zero(t, w.workerFailures(kind), "a completed refresh ends the failure streak")
	report, err := rt.RiverProgress(t.Context())
	require.NoError(t, err)
	var health *riverjobs.KindProgress
	for i := range report.Kinds {
		if report.Kinds[i].Kind == kind {
			health = &report.Kinds[i]
		}
	}
	require.NotNil(t, health)
	require.Empty(t, health.Reason)
	require.NoError(t, monitor.RaiseAlerts(t.Context(), riverjobs.ProgressReport{CheckedAt: w.clock.Now(), Progressing: true, Kinds: []riverjobs.KindProgress{*health}}))
	require.NotContains(t, w.openFindings(string(alerting.FindingWorkerStalled)), kind, "the stall resolves once the refresh completes")

	w.readUnavailable("nmi", true)
	w.advance(time.Hour)
	id, _, err := riverjobs.EnqueueMerchantRefresh(t.Context(), w.jobs, mid.UUID(), openrails.QueueBilling)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		job, err := w.jobs.JobGet(t.Context(), id)
		return err == nil && job.State == rivertype.JobStateRetryable
	}, time.Minute, 20*time.Millisecond, "a failed provider read still fails the job")
	failed, err := w.jobs.JobGet(t.Context(), id)
	require.NoError(t, err)
	require.Contains(t, failed.Errors[0].Error, "provider refresh incomplete: provider=1")
	require.Positive(t, w.workerFailures(kind), "a failed read is a failure")
	w.readUnavailable("nmi", false)
}

const strayFinding = "pull.subscription.missing"

// refreshWatermark is the PSP's refresh cursor for domain; nil when none.
func (w *world) refreshWatermark(psp billing.PSPID, domain string) *time.Time {
	w.t.Helper()
	var at *time.Time
	require.NoError(w.t, w.pool.QueryRow(w.t.Context(), w.q(`SELECT max(watermark_at) FROM billing.psp_refresh_watermarks WHERE psp_id=$1 AND event_domain=$2`), psp.UUID(), domain).Scan(&at))
	return at
}

// workerFailures is the kind's current failure streak.
func (w *world) workerFailures(kind string) int {
	w.t.Helper()
	var n int
	require.NoError(w.t, w.pool.QueryRow(w.t.Context(), w.q(`SELECT coalesce(max(consecutive_failures), 0) FROM billing.worker_state WHERE worker_kind=$1`), kind).Scan(&n))
	return n
}
