package reconcile

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestDuplicateSubscriptionsRespectProductScope(t *testing.T) {
	customer, first, second := uuid.New(), uuid.New(), uuid.New()
	for _, tc := range []struct {
		name          string
		secondProduct uuid.UUID
		groups        [2]string
		duplicate     bool
	}{
		{name: "independent products", secondProduct: second},
		{name: "same product", secondProduct: first, duplicate: true},
		{name: "exclusive tier group", secondProduct: second, groups: [2]string{"membership", "membership"}, duplicate: true},
		{name: "distinct tier groups", secondProduct: second, groups: [2]string{"membership", "storage"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			local := &LocalState{Subscriptions: []LocalSubscription{
				{ID: uuid.New(), CustomerID: customer, ProductID: first, TierGroup: tc.groups[0], RailSubscriptionID: "first"},
				{ID: uuid.New(), CustomerID: customer, ProductID: tc.secondProduct, TierGroup: tc.groups[1], RailSubscriptionID: "second"},
			}}
			remote := &RemoteSnapshot{Subscriptions: []RemoteSubscription{
				{RailSubscriptionID: "first", Status: SubscriptionStatusActive},
				{RailSubscriptionID: "second", Status: SubscriptionStatusActive},
			}}
			findings := diffDuplicates(ProviderNMI, buildLocalIndex(local), buildRemoteIndex(remote))
			if tc.duplicate {
				require.Len(t, findings, 1)
				require.Equal(t, FindingDuplicateSubscriptions, findings[0].Type)
			} else {
				require.Empty(t, findings)
			}
		})
	}
}
