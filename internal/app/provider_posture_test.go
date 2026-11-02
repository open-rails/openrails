package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/providerposture"
)

func TestPosturePassRechecksOnlyUnansweredPSPs(t *testing.T) {
	armed, ccbill, silent := uuid.New(), uuid.New(), uuid.New()
	statuses := map[uuid.UUID]providerposture.Status{armed: {Verdict: providerposture.Simulated}}
	status := func(id uuid.UUID) (providerposture.Status, bool) { s, ok := statuses[id]; return s, ok }
	var mu sync.Mutex
	calls := map[uuid.UUID]int{}
	verify := func(id uuid.UUID) bool {
		mu.Lock()
		defer mu.Unlock()
		calls[id]++
		// CCBill answers without a cached verdict; silent never answers.
		return id == ccbill
	}
	known := map[uuid.UUID]bool{}
	ids := []uuid.UUID{armed, ccbill, silent}
	for range 3 {
		require.Equal(t, []uuid.UUID{silent}, posturePass(ids, known, status, verify))
	}
	require.Equal(t, map[uuid.UUID]int{ccbill: 1, silent: 3}, calls, "a settled PSP is checked at most once; an unanswered one every pass")
}

func TestPostureBackoffIsCappedAtFiveMinutes(t *testing.T) {
	require.LessOrEqual(t, postureBackoff(0), time.Second)
	var longest time.Duration
	for range 200 {
		d := postureBackoff(40)
		require.LessOrEqual(t, d, 5*time.Minute)
		longest = max(longest, d)
	}
	require.Greater(t, longest, 30*time.Second, "the cap is the posture cap, not the 30 second default")
}

func TestPostureStateReadsArmingAtOnce(t *testing.T) {
	ctx := context.Background()
	registry := providerposture.Process()
	merchant, psp, untracked := uuid.New(), uuid.New(), uuid.New()
	key := providerposture.Key{Rail: "nmi", MerchantID: merchant, PSPID: psp}
	silent := func(context.Context) (providerposture.Verdict, error) {
		return providerposture.Unknown, errors.New("no answer")
	}
	registry.Verify(ctx, key, silent)
	r := &Runtime{}
	r.postureWait.Store(&postureWait{first: true})
	require.ErrorContains(t, r.postureState(), "first verification pending")
	r.providerPosture.AddPSP(merchant, psp, key, silent)
	r.postureWait.Store(&postureWait{psps: []uuid.UUID{psp, untracked}})
	require.ErrorContains(t, r.postureState(), "2 awaiting a provider answer")

	// A mutation's Require records the provider's answer in the registry.
	registry.Verify(ctx, key, func(context.Context) (providerposture.Verdict, error) { return providerposture.Simulated, nil })
	err := r.postureState()
	require.ErrorContains(t, err, "1 awaiting a provider answer", "a PSP never verified still waits")
	require.NotContains(t, err.Error(), key.String())

	r.postureWait.Store(&postureWait{psps: []uuid.UUID{psp}})
	require.NoError(t, r.postureState())

	registry.Verify(ctx, key, func(context.Context) (providerposture.Verdict, error) { return providerposture.Live, nil })
	err = r.postureState()
	require.Error(t, err, "an answer that does not arm is never read as armed")
	require.NotContains(t, err.Error(), "awaiting")
}
