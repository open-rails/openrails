package reconcile

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
)

// #837/#834: all-or-nothing pass brakes; small books still converge.
func TestCancelBudgetAndRosterBreaker(t *testing.T) {
	var b CancelBudget
	for live, want := range map[int]int{0: 3, 4: 4, 5: 5, 6: 3, 9: 3, 20: 3, 100: 5, 500: 25, 1000: 25, 10000: 25} {
		require.Equal(t, want, b.Limit(live), "Limit(%d)", live)
	}
	over, why := b.Exceeded(9, 9)
	require.True(t, over, "a nine-subscriber book losing all nine trips the cap")
	require.Contains(t, why, "NONE were applied")
	over, _ = b.Exceeded(2, 2)
	require.False(t, over, "two genuine cancellations on a tiny book converge")
	over, _ = b.Exceeded(4, 4)
	require.False(t, over, "a four-subscriber book whose schedules all ended converges")
	over, _ = b.Exceeded(6, 6)
	require.True(t, over, "no pass cancels a book above the tiny-book size entirely")
	over, _ = b.Exceeded(25, 1000)
	require.False(t, over)

	var r RosterBreaker
	for _, c := range []struct {
		remote, local int
		want          bool
	}{{0, 0, false}, {0, 1, true}, {0, 9, true}, {9, 100, true}, {10, 100, false}, {100, 100, false}} {
		got, _ := r.Implausible(ProviderNMI, c.remote, c.local)
		require.Equal(t, c.want, got, "remote=%d local=%d", c.remote, c.local)
	}
}

// or#858: an exhaustive declared book is an absence proof; a count must confirm it.
func TestDeclaredCoverageRefusesUnconfirmedExhaustiveBook(t *testing.T) {
	n := func(i int) *int { return &i }
	for _, c := range []struct {
		cov     DeclaredCoverage
		facts   int
		wantErr string
	}{
		{DeclaredCoverage{}, 3, ""},
		{DeclaredCoverage{}, 0, ""},
		{DeclaredCoverage{SubscriptionsExhaustive: true}, 3, "must declare expected_subscriptions"},
		{DeclaredCoverage{SubscriptionsExhaustive: true, ExpectedSubscriptions: n(40)}, 3, "says 40 but the call carries 3"},
		{DeclaredCoverage{SubscriptionsExhaustive: true, ExpectedSubscriptions: n(0)}, 0, "zero subscriptions"},
		{DeclaredCoverage{SubscriptionsExhaustive: true, ExpectedSubscriptions: n(3)}, 3, ""},
	} {
		err := c.cov.validate(c.facts)
		if c.wantErr == "" {
			require.NoError(t, err, "%+v/%d", c.cov, c.facts)
		} else {
			require.ErrorContains(t, err, c.wantErr)
		}
	}
}

func TestParseAmountCentsIsExact(t *testing.T) {
	for in, want := range map[string]int64{"9.99": 999, "23.99": 2399, "0.00": 0, "": 0, "23": 2300, "5.5": 550, "-5.00": -500, "+1.00": 100, ".99": 99} {
		got, err := parseAmountCents(in)
		require.NoError(t, err, in)
		require.Equal(t, want, got, in)
	}
	for _, in := range []string{"1.999", "abc", "1.2.3", "1,00", "- 1"} {
		_, err := parseAmountCents(in)
		require.Error(t, err, "%q must be refused, never truncated", in)
	}
}

func TestParseRebillOrderID(t *testing.T) {
	id := uuid.New()
	for in, want := range map[string]bool{
		fmt.Sprintf("rebill-%s-1765432100", id): true,
		id.String():                             true,
		"rebill-not-a-uuid-123":                 false,
		fmt.Sprintf("rebill-%s", id):            false,
		"":                                      false,
		"upgrade-12345678-87654321":             false,
	} {
		got, ok := parseRebillOrderID(in)
		require.Equal(t, want, ok, in)
		if want {
			require.Equal(t, id, got, in)
		}
	}
}

func TestCCBillPlanIndexUsesRecurringBillingOption(t *testing.T) {
	priceID := uuid.New()
	idx := buildPlanIndex(ProviderCCBill, []LocalPrice{{ID: priceID, ProductID: uuid.New(),
		PSPLinks: map[string]map[string]string{"ccbill": {"rail": "ccbill", "recurring_billing_option_id": "0000007498"}}}})
	require.Len(t, idx["0000007498"], 1)
	require.Equal(t, priceID, idx["0000007498"][0].price.ID)
	require.Equal(t, "ccbill", idx["0000007498"][0].railName)
}

// or#893: remote `expired` has no local lifecycle state, so PS-1 blocks
// instead of minting an unrepresentable row.
func TestRemoteStatesWithoutALocalStateNeverMaterialize(t *testing.T) {
	for remote, local := range map[SubscriptionStatus]models.SubscriptionStatus{
		SubscriptionStatusActive: models.StatusActive, SubscriptionStatusPastDue: models.StatusPastDue,
		SubscriptionStatusExpired: "", SubscriptionStatusCancelled: "", SubscriptionStatusUnknown: "", "something-new": "",
	} {
		got, ok := LocalMaterializeStatus(remote)
		require.Equal(t, local != "", ok, remote)
		require.Equal(t, local, got, remote)
	}

	next := time.Now().UTC().Add(oneDay)
	customerID := uuid.New()
	planIdx := map[string][]planLink{"plan-1": {{railName: "nmi", price: &LocalPrice{ID: uuid.New(), ProductID: uuid.New()}}}}
	ps1 := func(status SubscriptionStatus) Finding {
		idx := &localIndex{byPSID: map[string]*LocalSubscription{}, byEmail: map[string][]*LocalSubscription{},
			pmByRailCustomerRef: map[string]*LocalPaymentMethod{"vault-1": {CustomerID: customerID}}}
		remote := &RemoteSubscription{RailSubscriptionID: "sub-1", Status: status, PlanID: "plan-1", CustomerID: "vault-1", NextBillingAt: &next}
		return makePS1(ProviderNMI, remote, idx, planIdx, &RemoteSnapshot{Provider: ProviderNMI, FetchedAt: time.Now().UTC()}, diffOptions{Materialize: true})
	}
	live := ps1(SubscriptionStatusActive)
	require.NotNil(t, live.Apply, "%v", live.RemoteEvidence["materialize_blocked"])
	require.Equal(t, models.StatusActive, live.Apply.Materialize.Status)
	dead := ps1(SubscriptionStatusExpired)
	require.Nil(t, dead.Apply)
	require.Contains(t, dead.RemoteEvidence["materialize_blocked"], "no canonical local lifecycle state")
}
