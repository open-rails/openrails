//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/hosttools"
	"github.com/open-rails/openrails/internal/merchantbootstrap"
	"github.com/open-rails/openrails/internal/reconcile"
)

// armedAtWorldClock dates the destructive arming and this deployment's first
// pull on the world's clock, so provider evidence dated after them by that
// clock counts as observed here.
func (w *world) armedAtWorldClock() {
	w.t.Helper()
	_, err := w.pool.Exec(w.t.Context(), w.q(`UPDATE billing.merchant_destructive_policy SET enforce_armed_at = $1, first_pull_completed_at = $1`), w.clock.Now())
	require.NoError(w.t, err)
}

// cliPull is the operator's `openrails pull-provider` against this world: a
// one-off process over the same database, armed from the merchant manifest,
// talking to the same NMI. overwrite applies what it decides.
func (w *world) cliPull(provider string, overwrite bool) {
	w.t.Helper()
	require.NoError(w.t, w.cliPullAs(config.ProviderWriteModeFull, provider, false, overwrite), "CLI pull")
}

// cliPullAs is cliPull from a process started with the given
// provider_write_mode and mutation flags; it answers the command's error.
func (w *world) cliPullAs(writeMode, provider string, insert, overwrite bool) error {
	w.t.Helper()
	cfg := &config.Config{
		ProviderWriteMode: writeMode,
		DB:                &config.DBConfig{URL: w.dsn}, Database: config.DatabaseConfig{Schema: w.schema},
	}
	manifest := &merchantbootstrap.BillingConfig{Merchants: map[string]openrails.MerchantDeclaration{
		w.slug: {DisplayName: w.slug, PSPs: w.psps},
	}}
	var out strings.Builder
	err := hosttools.PullProvider(w.t.Context(), hosttools.PullProviderOptions{
		Config:           cfg,
		PGXPool:          w.pool,
		MerchantID:       w.client[embedded].MerchantID(),
		MerchantManifest: manifest,
		NMITransport:     http.RoundTripper(w.nmi),
		Providers:        []string{provider},
		// The world's clock, not the process's: the window it reads.
		Since:     w.clock.Now().Add(-120 * day).UTC().Format(time.RFC3339),
		Until:     w.clock.Now().UTC().Format(time.RFC3339),
		Insert:    insert,
		Overwrite: overwrite,
		Out:       &out,
	})
	w.t.Logf("CLI pull:\n%s", out.String())
	return err
}

// pullRuns counts the merchant's provider-pull passes, the operator's and the
// scheduled pull's alike.
func (w *world) pullRuns() (n int) {
	w.t.Helper()
	require.NoError(w.t, w.pool.QueryRow(w.t.Context(), w.q(`SELECT count(*) FROM billing.maintenance_runs WHERE kind = 'reconciliation' AND NOT rails @> '{self}'`)).Scan(&n))
	return n
}

// The CLI pull parks an NMI member whose renewal NMI declined for good; the
// verifier's per-schedule read cancels it and deletes the schedule once. A pull
// alone never cancels: NMI's bulk report carries no schedule id.
func TestCLIPullNMIDeclineEndsOnce(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.armDestructive()
	l := importLegacy(t, w, "nmi", embedded)
	w.converge()
	w.armedAtWorldClock()
	// NMI's own renewal is declined for good (a stolen card); no webhook
	// reaches OpenRails, so only a provider read can find it.
	w.nmi.SetDecline(visa.Last4, "252")
	w.advance(l.periodEnd().Sub(w.clock.Now()) + time.Hour)
	w.nmi.RunDue()
	w.advance(20 * day)
	require.Equal(t, billing.SubscriptionActive, w.subscription(embedded, l.sub).Status, "nothing told OpenRails yet")

	w.cliPull("nmi", true)
	w.refreshProviders() // the verifier's read
	w.until(func() bool { return w.subscription(embedded, l.sub).Status == "canceled" }, "the verifier ends the parked membership")
	require.False(t, l.c.entitled(l.ent))
	// The delete waits out its cooling-off window, then runs.
	sub := w.subscription(embedded, l.sub)
	require.NotNil(t, sub.DeletionScheduledAt, "the provider cancel is queued")
	w.advanceHealthyTo(sub.DeletionScheduledAt.Add(time.Minute))
	w.wake()
	w.until(func() bool { return w.nmi.ScheduleDeletes(l.railSub) > 0 }, "the NMI schedule delete")
	w.advance(time.Hour)
	w.wake()
	w.cliPull("nmi", true)
	w.wake()
	require.Equal(t, 1, w.nmi.ScheduleDeletes(l.railSub), "exactly one NMI delete")
	require.False(t, w.nmi.ScheduleLive(l.railSub))
}

// The operator's --overwrite is held by what holds the scheduled pull's: the
// destructive switch and the merchant's write posture. Refused, it reads no
// provider and changes nothing; with neither holding it, it overwrites.
func TestCLIPullOverwriteRefusedWhileSwitchOffOrReadOnly(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.armDestructive()
	l := importLegacy(t, w, "nmi", embedded)
	w.converge()
	w.armedAtWorldClock()
	w.nmi.SetDecline(visa.Last4, "252")
	w.advance(l.periodEnd().Sub(w.clock.Now()) + time.Hour)
	w.nmi.RunDue()
	w.advance(20 * day)
	setSwitch := func(enabled bool) {
		_, err := w.pool.Exec(t.Context(), w.q(`UPDATE billing.destructive_action_switch SET enabled = $1, updated_by = 'e2e'`), enabled)
		require.NoError(t, err)
	}
	refused := func(writeMode, reason string) {
		t.Helper()
		runs := w.pullRuns()
		err := w.cliPullAs(writeMode, "nmi", false, true)
		require.ErrorContains(t, err, "--overwrite refused")
		require.ErrorContains(t, err, reason)
		require.Equal(t, runs, w.pullRuns(), "a refused pull starts no pass")
		require.Equal(t, billing.SubscriptionActive, w.subscription(embedded, l.sub).Status)
	}

	setSwitch(false)
	refused(config.ProviderWriteModeFull, "kill switch is OFF")
	setSwitch(true)
	refused(config.ProviderWriteModeReadOnly, "provider_write_mode=readonly")

	w.cliPull("nmi", true)
	require.NotEqual(t, billing.SubscriptionActive, w.subscription(embedded, l.sub).Status, "with neither holding it, the pull applies what it decides")
}

// A mutating CLI pull takes the merchant's pull lock, the one a scheduled pull
// holds while it runs: it waits while the lock is held and runs once it is
// released, so the two never work the same PSP at once.
func TestCLIPullWaitsForTheMerchantPullLock(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	mid := w.client[embedded].MerchantID().UUID()
	// The world's own scheduled pull may hold it for a moment.
	var release func()
	for deadline := time.Now().Add(30 * time.Second); release == nil; time.Sleep(250 * time.Millisecond) {
		unlock, held, err := reconcile.LockMerchantPull(t.Context(), w.pool, mid)
		require.NoError(t, err)
		if held {
			release = unlock
			break
		}
		require.True(t, time.Now().Before(deadline), "the merchant's pull lock never came free")
	}
	runs := w.pullRuns()
	done := make(chan error, 1)
	go func() { done <- w.cliPullAs(config.ProviderWriteModeFull, "nmi", true, false) }()
	select {
	case err := <-done:
		release()
		t.Fatalf("the CLI pull ran beside the pull holding the lock (error: %v)", err)
	case <-time.After(3 * time.Second):
	}
	require.Equal(t, runs, w.pullRuns(), "no pass starts while the lock is held")
	release()
	require.NoError(t, <-done)
	require.Greater(t, w.pullRuns(), runs, "the pull ran once the lock was released")
}
