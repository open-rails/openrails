//go:build e2e && integration

package subscriptions_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/hosttools"
)

// undoRun reverses one destructive run the way `openrails undo-run` does: it
// reads the plan, then applies it with the planned row count typed back.
func (w *world) undoRun(run uuid.UUID) map[string]any {
	w.t.Helper()
	cfg := &config.Config{TestMode: config.CredentialPostureSandbox, ProviderWriteMode: config.ProviderWriteModeFull,
		DB: &config.DBConfig{URL: w.dsn}, Database: config.DatabaseConfig{Schema: w.schema}}
	opts := hosttools.UndoRunOptions{Config: cfg, PGXPool: w.pool, MerchantID: w.client[embedded].MerchantID(), RunID: run.String(), Actor: "e2e", Format: "json"}
	var plan strings.Builder
	opts.Out = &plan
	require.NoError(w.t, hosttools.UndoRun(w.t.Context(), opts))
	var decoded map[string]any
	require.NoError(w.t, json.Unmarshal([]byte(plan.String()), &decoded))
	var rows int64
	for _, n := range decoded["restorable"].(map[string]any) {
		rows += int64(n.(float64))
	}
	opts.Apply, opts.ExpectRows, opts.Out = true, rows, &strings.Builder{}
	require.NoError(w.t, hosttools.UndoRun(w.t.Context(), opts))
	return decoded
}

// Undoing a converge run never rewrites a subscription that moved after it:
// the restore would make a later lifecycle (here a verified cancel whose NMI
// delete is queued) live again and a paid period due again (tracker 1137).
func TestUndoRunKeepsLaterLifecycle(t *testing.T) {
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
	w.cliPull("nmi", true)
	w.until(func() bool { return w.subscription(embedded, l.sub).Status == billing.SubscriptionCanceled }, "the verifier ends the parked membership")
	var run uuid.UUID
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT destructive_run_id FROM billing.destructive_run_before_images WHERE row_id = $1 AND table_name = 'subscriptions'`), l.sub.UUID()).Scan(&run))

	plan := w.undoRun(run)
	sub := w.subscription(embedded, l.sub)
	require.Equal(t, billing.SubscriptionCanceled, sub.Status, "the later cancel is not rewritten")
	require.NotNil(t, sub.CanceledAt)
	require.EqualValues(t, 1, plan["subscriptions_changed"], "the plan names the row it keeps")
}

// Under mode=limited the due pass never ends a membership locally: one whose
// wait for a new card outlived its window is held with a finding, as the
// documented mode table says (tracker 1137).
func TestLimitedModeHoldsAwaitingMethodExpiry(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.armDestructive()
	e := enroll(t, w, "nmi", embedded)
	e.setDecline(visa.Last4, "expired_card", "223")
	e.toFreshPeriodEnd()
	w.runRenewals()
	sub := w.subscription(embedded, e.sub)
	require.Equal(t, billing.SubscriptionAwaitingMethod, sub.Status)
	require.NotNil(t, sub.GraceEndsAt)
	w.cfg = func(c *config.Config) { c.ProviderWriteMode = config.ProviderWriteModeLimited }
	w.restart()
	w.advanceHealthyTo(sub.GraceEndsAt.Add(time.Hour))
	w.runRenewals()
	require.Equal(t, billing.SubscriptionAwaitingMethod, w.subscription(embedded, e.sub).Status, "limited mode cancels nothing locally")
	require.Contains(t, w.openFindings("life.terminal_outcome.held"), e.sub.UUID().String())
}
