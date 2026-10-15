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
// never seen. Foreign classification requires proof that no local customer
// uses it. Its finding never blocks unrelated subscription operations.
func TestForeignScheduleClassificationDoesNotGateRenewals(t *testing.T) {
	t.Parallel()
	t.Run("nothing local names it", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		e := enroll(t, w, "nmi", embedded)
		other := w.nmi.AddSchedule(nmimock.Schedule{Vault: w.nmi.AddVault(visa), Plan: "another_environment_plan", Amount: "4.99", NextBilling: w.clock.Now().Add(10 * day), Book: w.bookID()})
		w.advisoryPull()
		p := w.proposalFor("pull.subscription.missing", other)

		_, err := w.foreign("", p.id)
		require.ErrorContains(t, err, "notes are required")
		res, err := w.foreign("staging's schedule on the shared sandbox gateway", p.id)
		require.NoError(t, err)
		require.Equal(t, billing.ProposalForeign, outcomes(res)[p.id], "%+v", res)
		got := w.proposalFor("pull.subscription.missing", other)
		require.Equal(t, []string{"ignored", "foreign", "cli:e2e"}, []string{got.status, got.resolution, got.resolvedBy})

		w.advisoryPull()
		require.Equal(t, "foreign", w.proposalFor("pull.subscription.missing", other).resolution, "reading the same record again keeps it foreign")
		e.toFreshPeriodEnd()
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
		before := len(e.providerLedger())
		e.toFreshPeriodEnd()
		w.runRenewals()
		require.Len(t, e.providerLedger(), before+1, "the unresolved finding does not gate a valid renewal")

		res, err = w.dismiss("muted while we investigate", p.id)
		require.NoError(t, err)
		require.Equal(t, billing.ProposalDismissed, outcomes(res)[p.id])
	})
}

// Foreign classification can revisit a dismissed finding; the prior operator
// decision remains in its evidence, and local customer ownership still refuses it.
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

		res, err = w.foreign("staging's schedule on the shared sandbox gateway", p.id)
		require.NoError(t, err)
		require.Equal(t, billing.ProposalForeign, outcomes(res)[p.id], "%+v", res)
		got := w.proposalFor("pull.subscription.missing", other)
		require.Equal(t, []string{"ignored", "foreign", "cli:e2e"}, []string{got.status, got.resolution, got.resolvedBy})
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
	})
}
