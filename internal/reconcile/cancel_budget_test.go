package reconcile

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCancelBudget_Limit(t *testing.T) {
	var b CancelBudget
	cases := map[int]int{
		0:     3,  // small-book allowance: a tiny merchant must still converge
		9:     3,  // 5% of 9 rounds to 0 -> allowance; all nine would still trip
		20:    3,  // 5% of 20 = 1 -> allowance
		100:   5,  // 5%
		500:   25, // 5% would be 25, equal to the absolute cap
		1000:  25, // absolute cap wins over 5% (=50)
		10000: 25,
	}
	for localLive, want := range cases {
		if got := b.Limit(localLive); got != want {
			t.Errorf("Limit(%d) = %d, want %d", localLive, got, want)
		}
	}
}

// #837: the ratio breaker's 10% threshold leaves a 90-point hole. A roster
// truncated to 15% clears it cleanly and would cancel 85% of the book. The
// absolute + percentage cancellation cap is what closes it.
func TestCancellationCapHaltsATruncatedRoster(t *testing.T) {
	ctx := context.Background()
	local := &fakeLocal{}
	const book = 200
	for i := 0; i < book; i++ {
		local.state.Subscriptions = append(local.state.Subscriptions, liveLocalSub(ProviderNMI, fmt.Sprintf("nmi-%d", i)))
	}
	// 15% of the book comes back — above the 10% ratio breaker, so the old code
	// proceeded to cancel the other 170.
	var remote []RemoteSubscription
	for i := 0; i < 30; i++ {
		s := &local.state.Subscriptions[i]
		remote = append(remote, RemoteSubscription{
			RailSubscriptionID: s.RailSubscriptionID,
			Status:             SubscriptionStatusActive,
			NextBillingAt:      tp(*s.CurrentPeriodEndsAt),
		})
	}
	snap := &RemoteSnapshot{
		Provider:      ProviderNMI,
		Capabilities:  Capabilities{Subscriptions: true},
		Coverage:      SnapshotCoverage{SubscriptionsExhaustive: true},
		Subscriptions: remote,
	}
	eng, store, writer := newTestEngine(ProviderNMI, snap, local)

	res, err := eng.Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderNMI}})
	require.Error(t, err, "170 planned cancellations must halt the pass")
	assert.Contains(t, err.Error(), "cancellation cap")
	assert.True(t, res.Summary.Providers["nmi"].Aborted)

	// NOTHING was applied — not the first 25, not one.
	assert.Zero(t, writer.calls["cancel"], "the cap is all-or-nothing")
	assert.Zero(t, writer.calls["revoke"])

	// The evidence survives: the 170 findings AND an operator-facing
	// requires_review record of the halted pass.
	assert.Len(t, findByType(res.Findings, FindingLocalActiveRemoteDead), book-30)
	capRec := store.record(ProviderNMI, FindingCancellationCapped, "cancellation_cap")
	require.NotNil(t, capRec, "a halted pass must raise a requires_review finding")
	assert.Equal(t, FindingStatusRequiresReview, capRec.Status)
}

// Below the cap, convergence still works — the guard bounds mass cancellation,
// it does not freeze the mirror.
func TestCancellationCapAllowsNormalConvergence(t *testing.T) {
	ctx := context.Background()
	local := &fakeLocal{}
	const book = 200
	for i := 0; i < book; i++ {
		local.state.Subscriptions = append(local.state.Subscriptions, liveLocalSub(ProviderNMI, fmt.Sprintf("nmi-%d", i)))
	}
	var remote []RemoteSubscription
	for i := 0; i < book-3; i++ { // three genuinely gone
		s := &local.state.Subscriptions[i]
		remote = append(remote, RemoteSubscription{
			RailSubscriptionID: s.RailSubscriptionID,
			Status:             SubscriptionStatusActive,
			NextBillingAt:      tp(*s.CurrentPeriodEndsAt),
		})
	}
	snap := &RemoteSnapshot{
		Provider:      ProviderNMI,
		Capabilities:  Capabilities{Subscriptions: true},
		Coverage:      SnapshotCoverage{SubscriptionsExhaustive: true},
		Subscriptions: remote,
	}
	eng, _, writer := newTestEngine(ProviderNMI, snap, local)

	res, err := eng.Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderNMI}})
	require.NoError(t, err)
	assert.False(t, res.Summary.Providers["nmi"].Aborted)
	assert.Equal(t, 3, writer.calls["cancel"])
}
