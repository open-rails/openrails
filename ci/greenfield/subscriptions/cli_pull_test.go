//go:build greenfield && integration

package subscriptions_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/hosttools"
)

// armedAtWorldClock dates the destructive arming and this deployment's first
// pull on the world's clock, so provider evidence dated after them by that
// clock counts as observed here (#835).
func (w *world) armedAtWorldClock() {
	w.t.Helper()
	_, err := w.pool.Exec(w.t.Context(), w.q(`UPDATE openrails.merchant_destructive_policy SET enforce_armed_at = $1, first_pull_completed_at = $1`), w.clock.Now())
	require.NoError(w.t, err)
}

// cliPull is the operator's `openrails pull-provider` against this world: a
// one-off process over the same database, armed from the merchant manifest,
// talking to the same NMI. overwrite applies what it decides.
func (w *world) cliPull(provider string, overwrite bool) {
	w.t.Helper()
	cfg := &config.Config{
		TestMode:          config.CredentialPostureSandbox,
		ProviderWriteMode: config.ProviderWriteModeFull,
		DB:                &config.DBConfig{URL: w.dsn, Schema: w.schema},
	}
	manifest := &hosttools.BillingConfig{Merchants: map[string]embed.MerchantConfig{
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
		Overwrite: overwrite,
		Out:       &out,
	})
	w.t.Logf("CLI pull:\n%s", out.String())
	require.NoError(w.t, err, "CLI pull")
}

// The operator's CLI pull finds an NMI member whose renewal NMI declined for
// good and parks it; the worker's verifier reads that schedule from NMI,
// cancels the membership and deletes the NMI schedule exactly once (#1102).
// A pull alone never cancels a live NMI schedule: NMI's bulk report carries
// no schedule id, so only the per-schedule read attributes the decline.
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
	require.Equal(t, "active", w.subscription(embedded, l.sub).Status, "nothing told OpenRails yet")

	w.cliPull("nmi", true)
	w.until(func() bool { return w.subscription(embedded, l.sub).Status == "cancelled" }, "the verifier ends the parked membership")
	require.False(t, l.c.entitled(l.ent))
	// The delete waits out its cooling-off window, then runs.
	sub := w.subscription(embedded, l.sub)
	require.NotNil(t, sub.DeletionScheduledAt, "the provider cancel is queued")
	w.advance(sub.DeletionScheduledAt.Sub(w.clock.Now()) + time.Minute)
	w.wake()
	w.until(func() bool { return w.nmi.ScheduleDeletes(l.railSub) > 0 }, "the NMI schedule delete")
	w.advance(time.Hour)
	w.wake()
	w.cliPull("nmi", true)
	w.wake()
	require.Equal(t, 1, w.nmi.ScheduleDeletes(l.railSub), "exactly one NMI delete")
	require.False(t, w.nmi.ScheduleLive(l.railSub))
}
