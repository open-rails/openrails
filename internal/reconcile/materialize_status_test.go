package reconcile

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
)

// or#893 phase 2. Remote provider vocabulary keeps `expired` — Stripe, CCBill
// and the declared-import all report it, and the #665 decider reads it as
// "the provider says this is dead". LOCAL storage does not: there is no local
// status meaning "the date passed", because as a terminal state that
// contradicts the rebill doctrine (NMI rebills forever; a lapsed
// next_billing_date is the normal state of every dunning customer). The
// boundary is this mapping, and it is deliberately partial.
func TestRemoteExpiredHasNoLocalLifecycleState(t *testing.T) {
	for _, tc := range []struct {
		remote SubscriptionStatus
		local  models.SubscriptionStatus
		ok     bool
	}{
		{SubscriptionStatusActive, models.StatusActive, true},
		{SubscriptionStatusPastDue, models.StatusPastDue, true},
		{SubscriptionStatusExpired, "", false},
		{SubscriptionStatusCancelled, "", false},
		{SubscriptionStatusUnknown, "", false},
		{SubscriptionStatus("something-new"), "", false},
	} {
		got, ok := LocalMaterializeStatus(tc.remote)
		require.Equal(t, tc.ok, ok, "remote %q", tc.remote)
		require.Equal(t, tc.local, got, "remote %q", tc.remote)
	}
}

// The mapping is not decorative: PS-1 materialization consults it, so a remote
// subscription the local vocabulary cannot express is BLOCKED with the reason
// recorded, not minted with an invented status. (Belt and braces with the
// remoteLive() gate above it — a future caller that reaches makePS1 by another
// route still cannot create an unrepresentable row.)
func TestMaterializeRefusesARemoteStateLocalVocabularyCannotExpress(t *testing.T) {
	now := time.Now().UTC()
	next := now.Add(24 * time.Hour)
	priceID, productID, customerID := uuid.New(), uuid.New(), uuid.New()

	// A resolvable identity (vault match) so the ONLY thing separating the two
	// cases below is the remote lifecycle state.
	newIdx := func() *localIndex {
		return &localIndex{
			byPSID:  map[string]*LocalSubscription{},
			byEmail: map[string][]*LocalSubscription{},
			pmByRailCustomerRef: map[string]*LocalPaymentMethod{
				"vault-1": {CustomerID: customerID},
			},
		}
	}
	planIdx := map[string][]planLink{
		"plan-1": {{railName: "nmi", price: &LocalPrice{ID: priceID, ProductID: productID}}},
	}
	snap := &RemoteSnapshot{Provider: ProviderNMI, FetchedAt: now}
	opts := diffOptions{Materialize: true}

	remote := func(status SubscriptionStatus) *RemoteSubscription {
		return &RemoteSubscription{
			RailSubscriptionID: "sub-1",
			Status:             status,
			PlanID:             "plan-1",
			CustomerID:         "vault-1",
			NextBillingAt:      &next,
		}
	}

	live := makePS1(ProviderNMI, remote(SubscriptionStatusActive), newIdx(), planIdx, snap, opts)
	require.NotNil(t, live.Apply, "a live remote with resolvable identity + plan materializes: %v", live.RemoteEvidence["materialize_blocked"])
	require.NotNil(t, live.Apply.Materialize)
	require.Equal(t, models.StatusActive, live.Apply.Materialize.Status,
		"a live remote materializes with the canonical local state")

	dead := makePS1(ProviderNMI, remote(SubscriptionStatusExpired), newIdx(), planIdx, snap, opts)
	require.Nil(t, dead.Apply, "a remote state with no local equivalent must not plan a write")
	require.Contains(t, dead.RemoteEvidence["materialize_blocked"], "no canonical local lifecycle state",
		"the refusal must be recorded for the operator, not silent")
}
