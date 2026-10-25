//go:build integration

package critical_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

func hostHeldCredits(t *testing.T) (*authorityWorld, []billing.CreditGrant, billing.DestructiveHold) {
	t.Helper()
	a := newAuthorityWorld(t)
	customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
	var in []billing.CreateCreditGrantParams
	for i := range 7 {
		in = append(in, billing.CreateCreditGrantParams{CustomerID: customer, Currency: "USD", Amount: 100, Source: "critical", SourceID: fmt.Sprintf("hold-%d", i)})
	}
	grants, err := a.alpha.CreateCreditGrants(t.Context(), in)
	require.NoError(t, err)
	require.Len(t, grants, 7)
	for i := range 5 {
		revoked, err := a.alphaRemote.RevokeCreditGrant(t.Context(), grants[i].ID, billing.RevokeCreditGrantParams{Reason: "operator correction"})
		require.NoError(t, err)
		require.Equal(t, billing.CreditGrantRevoked, revoked.State)
	}
	_, err = a.alphaRemote.RevokeCreditGrant(t.Context(), grants[5].ID, billing.RevokeCreditGrantParams{Reason: "operator correction"})
	require.Error(t, err)
	holds, err := a.alpha.ListDestructiveHolds(t.Context(), billing.DestructiveHoldListParams{})
	require.NoError(t, err)
	require.Len(t, holds.Items, 1)
	balance(t, a.alpha, customer, 200, 0, 200)
	return a, grants, holds.Items[0]
}
func TestDestructiveControls(t *testing.T) {
	t.Run("OR-J34-S01/ceiling-persists-original-actor-target-and-denied-effect", func(t *testing.T) {
		a, grants, hold := hostHeldCredits(t)
		require.Equal(t, "actor", hold.Scope)
		require.Equal(t, "revoke_credit", hold.Action)
		require.Equal(t, "hour", hold.Window)
		require.EqualValues(t, 5, hold.Observed)
		require.EqualValues(t, 1, hold.Requested)
		require.EqualValues(t, 5, hold.Ceiling)
		require.Nil(t, hold.ClearedAt)
		reviews, err := a.engine.ListOpenDestructiveHolds(t.Context(), 100, billing.DestructiveHoldID{})
		require.NoError(t, err)
		require.Len(t, reviews, 1)
		require.Equal(t, hold.ID, reviews[0].Hold.ID)
		require.NotEmpty(t, reviews[0].TriggerActor.ID)
		require.NotEmpty(t, reviews[0].TriggerActor.Issuer)
		require.Equal(t, grants[5].ID.UUID().String(), reviews[0].Target)
		for range 2 {
			_, err = a.alphaRemote.RevokeCreditGrant(t.Context(), grants[5].ID, billing.RevokeCreditGrantParams{Reason: "operator correction"})
			require.Error(t, err)
		}
		current, err := a.alpha.GetCreditGrant(t.Context(), grants[5].ID)
		require.NoError(t, err)
		require.Equal(t, billing.CreditGrantActive, current.State)
		require.EqualValues(t, 100, current.RemainingAmount)
		findings, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "life.destructive.hold"})
		require.NoError(t, err)
		require.Len(t, findings.Items, 1)
		require.Equal(t, "requires_review", string(findings.Items[0].Status))
		events, err := a.alpha.ListEvents(t.Context(), billing.EventQuery{Types: []string{string(billing.HostEventDestructiveHoldOpened)}, Limit: 100})
		require.NoError(t, err)
		require.Len(t, events.Events, 1)
		require.Contains(t, string(events.Events[0].Data), hold.ID.String())
		public, _ := json.Marshal(findings.Items[0])
		require.NotContains(t, string(public), "trigger_actor")
	})
	t.Run("OR-J34-S02/noninteractive-and-foreign-authority-cannot-clear-or-forge-actor", func(t *testing.T) {
		a, grants, hold := hostHeldCredits(t)
		for _, client := range []*openrails.Client{a.alphaRemote, a.betaRemote, a.alpha} {
			_, err := client.ClearDestructiveHold(t.Context(), hold.ID, billing.ClearDestructiveHoldParams{Reason: "untrusted clear"})
			require.Error(t, err)
		}
		path := "/v1/admin/credit-grants/" + grants[5].ID.String() + "/revoke"
		status, raw := a.request(t, "POST", path, a.alphaKey.Secret, billing.RevokeCreditGrantParams{Reason: "operator correction"}, map[string]string{"X-Actor-ID": a.bob.ID, "X-Invoker-ID": a.bob.ID})
		require.Equal(t, 429, status, string(raw))
		stored, err := a.alpha.ListDestructiveHolds(t.Context(), billing.DestructiveHoldListParams{IDs: []billing.DestructiveHoldID{hold.ID}})
		require.NoError(t, err)
		require.Len(t, stored.Items, 1)
		require.Nil(t, stored.Items[0].ClearedAt)
		balance(t, a.alpha, grants[5].CustomerID, 200, 0, 200)
	})
	t.Run("OR-J34-S03/reviewed-clear-resumes-action-without-replaying-prior-effects", func(t *testing.T) {
		a, grants, hold := hostHeldCredits(t)
		cleared, err := a.engine.ClearDestructiveHold(t.Context(), a.alpha.MerchantID(), hold.ID, billing.DestructiveOperator{Issuer: "https://operator.critical.invalid", ID: "verified-human"}, "reviewed seven independent corrections")
		require.NoError(t, err)
		require.NotNil(t, cleared.ClearedAt)
		require.Equal(t, hold.ID, cleared.ID)
		for range 2 {
			result, err := a.alphaRemote.RevokeCreditGrant(t.Context(), grants[5].ID, billing.RevokeCreditGrantParams{Reason: "operator correction"})
			require.NoError(t, err)
			require.EqualValues(t, 100, result.RevokedAmount)
		}
		balance(t, a.alpha, grants[5].CustomerID, 100, 0, 100)
		untouched, err := a.alpha.GetCreditGrant(t.Context(), grants[6].ID)
		require.NoError(t, err)
		require.Equal(t, billing.CreditGrantActive, untouched.State)
		events, err := a.alpha.ListEvents(t.Context(), billing.EventQuery{Types: []string{string(billing.HostEventDestructiveHoldCleared)}, Limit: 100})
		require.NoError(t, err)
		require.Len(t, events.Events, 1)
		open, err := a.engine.ListOpenDestructiveHolds(t.Context(), 100, billing.DestructiveHoldID{})
		require.NoError(t, err)
		require.Empty(t, open)
	})
	t.Run("OR-J34-S04/finding-resolution-cannot-bypass-destructive-hold", func(t *testing.T) {
		a, grants, hold := hostHeldCredits(t)
		findings, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "life.destructive.hold"})
		require.NoError(t, err)
		require.Len(t, findings.Items, 1)
		_, err = a.alphaRemote.ResolveFinding(t.Context(), findings.Items[0].ID, billing.ResolveFindingParams{Outcome: billing.FindingApprove, Notes: "attempt bypass"})
		require.Error(t, err)
		_, err = a.alphaRemote.ResolveFinding(t.Context(), findings.Items[0].ID, billing.ResolveFindingParams{Outcome: billing.FindingIgnore, Notes: "hide incident"})
		require.Error(t, err)
		var refusal *billing.StatusError
		require.ErrorAs(t, err, &refusal)
		require.Equal(t, billing.CodePermissionRequired, refusal.Code)
		// Finding presentation operations cannot clear financial authority.
		_, err = a.alphaRemote.RevokeCreditGrant(t.Context(), grants[6].ID, billing.RevokeCreditGrantParams{Reason: "another correction"})
		require.Error(t, err)
		reviews, err := a.engine.ListOpenDestructiveHolds(t.Context(), 100, billing.DestructiveHoldID{})
		require.NoError(t, err)
		require.Len(t, reviews, 1)
		require.Equal(t, hold.ID, reviews[0].Hold.ID)
		balance(t, a.alpha, grants[5].CustomerID, 200, 0, 200)
	})
}
