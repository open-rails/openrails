//go:build e2e && integration

package subscriptions_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/hosttools"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/open-rails/openrails/internal/providerrecovery"
)

// foreign is `openrails proposals foreign` against this world.
func (w *world) foreign(notes string, ids ...string) (billing.ProposalResults, error) {
	w.t.Helper()
	opts := w.proposalsCLI(config.ProviderWriteModeFull)
	var out strings.Builder
	opts.Out = &out
	if err := hosttools.ProposalsForeign(w.t.Context(), opts, hosttools.ProposalSet{IDs: ids}, notes); err != nil {
		return billing.ProposalResults{}, err
	}
	var res billing.ProposalResults
	require.NoError(w.t, json.Unmarshal([]byte(out.String()), &res), out.String())
	return res, nil
}

// A gateway shared with other environments bills schedules this merchant has
// never seen. One that nothing here names, resolved as foreign, no longer holds
// provider recovery: the refresh completes and renewals run. One on a vault a
// customer here uses could be this merchant's money: foreign is refused, and
// dismissing it keeps the hold.
func TestForeignScheduleReleasesProviderRecovery(t *testing.T) {
	t.Parallel()
	t.Run("nothing local names it", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		e := enroll(t, w, "nmi", embedded)
		other := w.nmi.AddSchedule(nmimock.Schedule{Vault: w.nmi.AddVault(visa), Plan: "another_environment_plan", Amount: "4.99", NextBilling: w.clock.Now().Add(10 * day), Book: w.bookID()})
		w.advisoryPull()
		p := w.proposalFor("pull.subscription.missing", other)
		require.ErrorIs(t, w.recoveryCheck("nmi"), providerrecovery.ErrPending, "an unknown schedule holds the PSP")

		_, err := w.foreign("", p.id)
		require.ErrorContains(t, err, "notes are required")
		res, err := w.foreign("staging's schedule on the shared sandbox gateway", p.id)
		require.NoError(t, err)
		require.Equal(t, billing.ProposalForeign, outcomes(res)[p.id], "%+v", res)
		got := w.proposalFor("pull.subscription.missing", other)
		require.Equal(t, []string{"ignored", "foreign", "cli:e2e"}, []string{got.status, got.resolution, got.resolvedBy})
		require.NoError(t, w.recoveryCheck("nmi"), "a foreign schedule settles the gate")

		w.advisoryPull()
		require.Equal(t, "foreign", w.proposalFor("pull.subscription.missing", other).resolution, "reading the same record again keeps it foreign")
		e.toFreshPeriodEnd() // the refresh completes: nothing holds the PSP
		w.runRenewals()
		require.Len(t, e.providerLedger(), 2, "the renewal is charged")
		require.True(t, e.c.entitled(e.ent))
	})
	t.Run("a customer's vault", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		e := enroll(t, w, "nmi", embedded)
		mine := w.nmi.AddSchedule(nmimock.Schedule{Vault: w.vaultOf(e.method), Plan: "unlinked_plan", Amount: "9.99", NextBilling: w.clock.Now().Add(10 * day)})
		w.advisoryPull()
		p := w.proposalFor("pull.subscription.missing", mine)

		res, err := w.foreign("looks like another environment", p.id)
		require.NoError(t, err)
		require.Len(t, res.Data, 1)
		require.Equal(t, billing.ProposalFailed, res.Data[0].Outcome)
		require.Equal(t, "finding_not_actionable", res.Data[0].Error.Code)
		require.Contains(t, w.openFindings("pull.subscription.missing"), mine, "a schedule on a customer's vault stays open")
		require.ErrorIs(t, w.recoveryCheck("nmi"), providerrecovery.ErrPending)

		res, err = w.dismiss("muted while we investigate", p.id)
		require.NoError(t, err)
		require.Equal(t, billing.ProposalDismissed, outcomes(res)[p.id])
		require.ErrorIs(t, w.recoveryCheck("nmi"), providerrecovery.ErrPending, "dismissing silences the alert; it does not settle money that could be ours")
	})
}

// A stray schedule an operator dismissed earlier still holds provider
// recovery, so foreign must reach it: released when nothing local names it,
// refused when something does, and answered already_resolved once foreign.
func TestForeignResolvesDismissedFindings(t *testing.T) {
	t.Parallel()
	t.Run("nothing local names it", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		e := enroll(t, w, "nmi", embedded)
		other := w.nmi.AddSchedule(nmimock.Schedule{Vault: w.nmi.AddVault(visa), Plan: "another_environment_plan", Amount: "4.99", NextBilling: w.clock.Now().Add(10 * day), Book: w.bookID()})
		w.advisoryPull()
		p := w.proposalFor("pull.subscription.missing", other)
		res, err := w.dismiss("sandbox noise", p.id)
		require.NoError(t, err)
		require.Equal(t, billing.ProposalDismissed, outcomes(res)[p.id])
		require.ErrorIs(t, w.recoveryCheck("nmi"), providerrecovery.ErrPending, "a dismissed finding still holds the PSP")

		res, err = w.foreign("staging's schedule on the shared sandbox gateway", p.id)
		require.NoError(t, err)
		require.Equal(t, billing.ProposalForeign, outcomes(res)[p.id], "%+v", res)
		got := w.proposalFor("pull.subscription.missing", other)
		require.Equal(t, []string{"ignored", "foreign", "cli:e2e"}, []string{got.status, got.resolution, got.resolvedBy})
		require.NoError(t, w.recoveryCheck("nmi"), "foreign settles the gate")
		var prev string
		require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT evidence->'resolution'->'previous'->>'resolution' || ':' || (evidence->'resolution'->'previous'->>'notes') FROM billing.reconciliation_findings WHERE finding_type = 'pull.subscription.missing' AND subject_key = $1`), other).Scan(&prev))
		require.Equal(t, "dismissed:sandbox noise", prev, "the dismissal is kept in the evidence")

		res, err = w.foreign("again", p.id)
		require.NoError(t, err)
		require.Equal(t, billing.ProposalAlreadyResolved, outcomes(res)[p.id], "an already-foreign finding is settled")

		e.toFreshPeriodEnd()
		w.runRenewals()
		require.Len(t, e.providerLedger(), 2, "the renewal is charged")
	})
	t.Run("a customer's vault", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		e := enroll(t, w, "nmi", embedded)
		mine := w.nmi.AddSchedule(nmimock.Schedule{Vault: w.vaultOf(e.method), Plan: "unlinked_plan", Amount: "9.99", NextBilling: w.clock.Now().Add(10 * day)})
		w.advisoryPull()
		p := w.proposalFor("pull.subscription.missing", mine)
		_, err := w.dismiss("muted", p.id)
		require.NoError(t, err)

		res, err := w.foreign("looks like another environment", p.id)
		require.NoError(t, err)
		require.Equal(t, billing.ProposalFailed, res.Data[0].Outcome)
		require.Equal(t, "finding_not_actionable", res.Data[0].Error.Code)
		got := w.proposalFor("pull.subscription.missing", mine)
		require.Equal(t, []string{"ignored", "dismissed"}, []string{got.status, got.resolution}, "a refused finding stays dismissed")
		require.ErrorIs(t, w.recoveryCheck("nmi"), providerrecovery.ErrPending)
	})
}
