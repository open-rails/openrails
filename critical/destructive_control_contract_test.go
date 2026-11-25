//go:build integration

package critical_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
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
		revoked, err := a.alphaRemote.RevokeCreditGrant(t.Context(), "", grants[i].ID, billing.RevokeCreditGrantParams{Reason: "operator correction"})
		require.NoError(t, err)
		require.Equal(t, billing.CreditGrantRevoked, revoked.State)
	}
	_, err = a.alphaRemote.RevokeCreditGrant(t.Context(), "", grants[5].ID, billing.RevokeCreditGrantParams{Reason: "operator correction"})
	require.Error(t, err)
	holds, err := a.alpha.ListDestructiveHolds(t.Context(), billing.DestructiveHoldListParams{})
	require.NoError(t, err)
	require.Len(t, holds.Items, 1)
	balance(t, a.alpha, customer, 200, 0, 200)
	return a, grants, holds.Items[0]
}

// holdEvidence is the actor whose action opened hold, its target, and who
// cleared it: the private review the standalone server's operator lists.
func holdEvidence(t *testing.T, a *authorityWorld, hold billing.DestructiveHold) (trigger auth.Invoker, target string, clearedBy *auth.Invoker) {
	t.Helper()
	var issuer, id *string
	require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT actor_issuer, actor_id, target, cleared_by_issuer, cleared_by_id FROM "+pgx.Identifier{a.schema, "destructive_holds"}.Sanitize()+" WHERE merchant_id = $1 AND id = $2",
		hold.MerchantID.UUID(), hold.ID.UUID()).Scan(&trigger.Issuer, &trigger.ID, &target, &issuer, &id))
	if issuer != nil && id != nil {
		clearedBy = &auth.Invoker{Issuer: *issuer, ID: *id}
	}
	return trigger, target, clearedBy
}

// holdReviewer is the person a host verified to review holds.
var holdReviewer = auth.Invoker{Issuer: "https://operator.critical.invalid", ID: "verified-human"}

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
		trigger, target, clearedBy := holdEvidence(t, a, hold)
		require.NotEmpty(t, trigger.ID)
		require.NotEmpty(t, trigger.Issuer)
		require.Equal(t, grants[5].ID.UUID().String(), target)
		require.Nil(t, clearedBy)
		var revoker *string
		require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT actor FROM "+pgx.Identifier{a.schema, "credit_grant_events"}.Sanitize()+" WHERE merchant_id = $1 AND credit_grant_id = $2 AND event = 'revoke'",
			a.alpha.MerchantID().UUID(), grants[0].ID.UUID()).Scan(&revoker))
		require.NotNil(t, revoker, "a credit grant's revoke names the staff member who made it")
		require.NotEmpty(t, *revoker)
		for range 2 {
			_, err := a.alphaRemote.RevokeCreditGrant(t.Context(), "", grants[5].ID, billing.RevokeCreditGrantParams{Reason: "operator correction"})
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
		events, err := a.alpha.ListEvents(t.Context(), billing.EventListParams{Types: []string{string(billing.HostEventDestructiveHoldOpened)}, Limit: 100})
		require.NoError(t, err)
		require.Len(t, events.Events, 1)
		require.Contains(t, string(events.Events[0].Data), hold.ID.String())
		public, _ := json.Marshal(findings.Items[0])
		require.NotContains(t, string(public), "trigger_actor")

		// A deployment's configured lower actor limit holds the third revoke.
		low := newAuthorityWorld(t, func(w *world) {
			w.cfg.DestructiveActionLimits = billing.DestructiveActionLimitsConfig{ActorPerHour: 2}
		})
		var few []billing.CreateCreditGrantParams
		for i := range 3 {
			few = append(few, billing.CreateCreditGrantParams{CustomerID: billing.CustomerID(uuid.MustParse(low.alice.ID)), Currency: "USD", Amount: 100, Source: "critical", SourceID: fmt.Sprintf("low-%d", i)})
		}
		lowGrants, err := low.alpha.CreateCreditGrants(t.Context(), few)
		require.NoError(t, err)
		for i := range 2 {
			_, err := low.alphaRemote.RevokeCreditGrant(t.Context(), "", lowGrants[i].ID, billing.RevokeCreditGrantParams{Reason: "operator correction"})
			require.NoError(t, err)
		}
		_, err = low.alphaRemote.RevokeCreditGrant(t.Context(), "", lowGrants[2].ID, billing.RevokeCreditGrantParams{Reason: "operator correction"})
		require.Error(t, err)
		lowHolds, err := low.alpha.ListDestructiveHolds(t.Context(), billing.DestructiveHoldListParams{})
		require.NoError(t, err)
		require.Len(t, lowHolds.Items, 1)
		require.Equal(t, "actor", lowHolds.Items[0].Scope)
		require.EqualValues(t, 2, lowHolds.Items[0].Ceiling)
	})
	t.Run("OR-J34-S02/noninteractive-and-foreign-authority-cannot-clear-or-forge-actor", func(t *testing.T) {
		a, grants, hold := hostHeldCredits(t)
		// The host's own handle names no person, and the person whose action
		// opened the hold cannot clear it, however the host names them.
		trigger, _, _ := holdEvidence(t, a, hold)
		self, err := a.alpha.ActingFor(trigger)
		require.NoError(t, err)
		for _, client := range []*openrails.Client{a.alphaRemote, a.betaRemote, a.alpha, self} {
			_, err := client.ClearDestructiveHold(t.Context(), hold.ID, billing.ClearDestructiveHoldParams{Reason: "untrusted clear"})
			require.Error(t, err)
		}
		// A person with merchant configuration authority still needs the
		// unlock permission; one the host never declared is held by no one.
		clear := "/v1/merchant/destructive-holds/" + hold.ID.String() + "/clear"
		status, raw := a.request(t, "POST", clear, a.aliceToken, billing.ClearDestructiveHoldParams{Reason: "untrusted clear"})
		require.Equal(t, http.StatusForbidden, status, string(raw))
		require.Contains(t, string(raw), `"code":"permission_required"`)
		undeclared := newAuthorityWorldWithAuth(t, func(c *authkit.Config) {
			roles := authkit.NewRoles(authkit.APIKeys)
			merchant := roles.Persona(billing.MerchantGroupPersona, authkit.APIKeys, authkit.CustomRoles, authkit.RemoteApplications)
			for _, permission := range billing.Permissions() {
				switch {
				case permission == billing.RootAdminRateLimitsUnlock:
				case permission.Persona() == billing.MerchantGroupPersona:
					merchant.Declare(string(permission))
				default:
					roles.Root.Declare(string(permission))
				}
			}
			c.Roles = roles
		})
		status, raw = undeclared.request(t, "POST", clear, undeclared.aliceToken, billing.ClearDestructiveHoldParams{Reason: "untrusted clear"})
		require.Equal(t, http.StatusForbidden, status, string(raw))
		require.Contains(t, string(raw), `"code":"permission_required"`)
		path := "/v1/admin/credit-grants/" + grants[5].ID.String() + "/revoke"
		status, raw = a.request(t, "POST", path, a.alphaKey.Secret, billing.RevokeCreditGrantParams{Reason: "operator correction"}, map[string]string{"X-Actor-ID": a.bob.ID, "X-Invoker-ID": a.bob.ID})
		require.Equal(t, 429, status, string(raw))
		stored, err := a.alpha.ListDestructiveHolds(t.Context(), billing.DestructiveHoldListParams{IDs: []billing.DestructiveHoldID{hold.ID}})
		require.NoError(t, err)
		require.Len(t, stored.Items, 1)
		require.Nil(t, stored.Items[0].ClearedAt)
		balance(t, a.alpha, grants[5].CustomerID, 200, 0, 200)
		// A different person holding root's permissions clears it over HTTP.
		authtest.GrantRole(t, a.auth, iam.RootGroup(), iam.UserSubject(a.alice.ID), iam.RootPersona().OwnerRole())
		cleared, err := a.remote(t, a.alpha.MerchantID(), a.aliceToken).ClearDestructiveHold(t.Context(), hold.ID, billing.ClearDestructiveHoldParams{Reason: "reviewed"})
		require.NoError(t, err)
		require.NotNil(t, cleared.ClearedAt)
	})
	t.Run("OR-J34-S03/reviewed-clear-resumes-action-without-replaying-prior-effects", func(t *testing.T) {
		a, grants, hold := hostHeldCredits(t)
		reviewer, err := a.alpha.ActingFor(holdReviewer)
		require.NoError(t, err)
		cleared, err := reviewer.ClearDestructiveHold(t.Context(), hold.ID, billing.ClearDestructiveHoldParams{Reason: "reviewed seven independent corrections"})
		require.NoError(t, err)
		require.NotNil(t, cleared.ClearedAt)
		require.Equal(t, hold.ID, cleared.ID)
		_, _, clearedBy := holdEvidence(t, a, hold)
		require.Equal(t, &holdReviewer, clearedBy, "the clear records the person the host named")
		for range 2 {
			result, err := a.alphaRemote.RevokeCreditGrant(t.Context(), "", grants[5].ID, billing.RevokeCreditGrantParams{Reason: "operator correction"})
			require.NoError(t, err)
			require.EqualValues(t, 100, result.RevokedAmount)
		}
		balance(t, a.alpha, grants[5].CustomerID, 100, 0, 100)
		untouched, err := a.alpha.GetCreditGrant(t.Context(), grants[6].ID)
		require.NoError(t, err)
		require.Equal(t, billing.CreditGrantActive, untouched.State)
		events, err := a.alpha.ListEvents(t.Context(), billing.EventListParams{Types: []string{string(billing.HostEventDestructiveHoldCleared)}, Limit: 100})
		require.NoError(t, err)
		require.Len(t, events.Events, 1)
		stored, err := a.alpha.ListDestructiveHolds(t.Context(), billing.DestructiveHoldListParams{IDs: []billing.DestructiveHoldID{hold.ID}})
		require.NoError(t, err)
		require.Len(t, stored.Items, 1)
		require.NotNil(t, stored.Items[0].ClearedAt)
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
		_, err = a.alphaRemote.RevokeCreditGrant(t.Context(), "", grants[6].ID, billing.RevokeCreditGrantParams{Reason: "another correction"})
		require.Error(t, err)
		stored, err := a.alpha.ListDestructiveHolds(t.Context(), billing.DestructiveHoldListParams{})
		require.NoError(t, err)
		require.Len(t, stored.Items, 1)
		require.Equal(t, hold.ID, stored.Items[0].ID)
		require.Nil(t, stored.Items[0].ClearedAt)
		balance(t, a.alpha, grants[5].CustomerID, 200, 0, 200)
	})
	t.Run("OR-J34-S31/a-remote-handle-refuses-acting-for", func(t *testing.T) {
		a := newAuthorityWorld(t)
		_, err := a.alphaRemote.ActingFor(holdReviewer)
		require.Error(t, err, "a remote handle's credential names its caller")
		reviewer, err := a.alpha.ActingFor(holdReviewer)
		require.NoError(t, err)
		_, err = openrails.NewGate(reviewer, openrails.GateDeps{})
		require.Error(t, err, "a gate serves every request, so it takes the merchant's own handle")
		product, err := reviewer.CreateProduct(t.Context(), billing.CreateProductParams{Key: "acted-for", DisplayName: "Acted for"})
		require.NoError(t, err)
		require.Equal(t, "acted-for", product.Key)
	})
}
