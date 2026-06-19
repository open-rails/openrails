package reconcile

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeIntentSource mirrors ListStuckProviderIntents' cutoff semantics over an
// in-memory ledger, so the engine's threshold computation (24h action / 2h
// verify) is exercised end to end.
type fakeIntentSource struct {
	intents []StuckIntent
	err     error
}

func (s *fakeIntentSource) ListStuck(ctx context.Context, actionCutoff, verifyCutoff time.Time) ([]StuckIntent, error) {
	if s.err != nil {
		return nil, s.err
	}
	var out []StuckIntent
	for _, si := range s.intents {
		switch si.Status {
		case "pending", "failed_retryable":
			if !si.CreatedAt.After(actionCutoff) {
				out = append(out, si)
			}
		case "in_flight", "unknown_needs_verify":
			if !si.CreatedAt.After(verifyCutoff) {
				out = append(out, si)
			}
		}
	}
	return out, nil
}

// newStuckIntentEngine builds a fetcher-less engine: PS-10 is
// provider-independent, so a run with zero providers still emits it.
func newStuckIntentEngine(src StuckIntentSource) (*Engine, *memStore) {
	store := newMemStore()
	return &Engine{
		Fetchers: map[Provider]ProcessorFetcher{},
		Store:    store,
		Local:    &fakeLocal{},
		Intents:  src,
		Now:      func() time.Time { return testNow },
	}, store
}

func stuckTestIntent(status string, age time.Duration, reason string) StuckIntent {
	subID := uuid.New()
	return StuckIntent{
		ID:                uuid.New(),
		Provider:          "mobius",
		IntentType:        "nmi_delete_subscription",
		SubscriptionID:    &subID,
		Status:            status,
		Attempts:          3,
		NextAttemptAt:     testNow.Add(-5 * time.Minute),
		Origin:            "system",
		LastFailureReason: reason,
		CreatedAt:         testNow.Add(-age),
	}
}

func TestStuckIntentFindings(t *testing.T) {
	oldPending := stuckTestIntent("pending", 26*time.Hour, "nmi error: connection refused")
	parked := stuckTestIntent("pending", 26*time.Hour,
		"nmi client is read-only (mode=readonly)")
	freshPending := stuckTestIntent("pending", time.Hour, "")
	oldUnknown := stuckTestIntent("unknown_needs_verify", 3*time.Hour, "ambiguous: timeout after send")
	freshUnknown := stuckTestIntent("unknown_needs_verify", 30*time.Minute, "ambiguous: timeout after send")

	src := &fakeIntentSource{intents: []StuckIntent{oldPending, parked, freshPending, oldUnknown, freshUnknown}}
	eng, store := newStuckIntentEngine(src)

	res, err := eng.Run(context.Background(), RunParams{Mode: ModeAdvisory})
	require.NoError(t, err)
	assert.Equal(t, "completed", res.Status)

	bySubject := map[string]FindingRecord{}
	for _, f := range res.Findings {
		require.Equal(t, FindingStuckIntent, f.Type, "fetcher-less run emits only PS-10")
		bySubject[f.SubjectKey] = f
	}
	require.Len(t, bySubject, 3, "old pending + parked + old unknown; fresh intents are not stuck")

	// Old pending with a genuine failure reason: admin-queued.
	rec, ok := bySubject[oldPending.ID.String()]
	require.True(t, ok)
	assert.Equal(t, FindingStatusAdminPending, rec.Status)
	assert.True(t, rec.RequiresAdmin)
	assert.Equal(t, SeverityHigh, rec.Severity)
	assert.Equal(t, Provider("mobius"), rec.Provider)
	assert.Equal(t, "nmi_delete_subscription", rec.IntentEvidence["intent_type"])
	assert.Equal(t, "pending", rec.IntentEvidence["status"])
	assert.Equal(t, oldPending.SubscriptionID.String(), rec.IntentEvidence["subscription_id"])
	assert.Equal(t, "nmi error: connection refused", rec.IntentEvidence["last_failure_reason"])
	assert.Equal(t, "26h0m0s", rec.IntentEvidence["age"])

	// Mode/kill-switch park: informational, NOT admin-queued.
	rec, ok = bySubject[parked.ID.String()]
	require.True(t, ok)
	assert.Equal(t, FindingStatusOpen, rec.Status)
	assert.False(t, rec.RequiresAdmin)
	assert.Equal(t, SeverityLow, rec.Severity)
	assert.Contains(t, rec.RecommendedAction, "no admin action needed")

	// Unknown beyond the 2h verify threshold: admin-queued (a healthy
	// verifier resolves unknowns in minutes).
	rec, ok = bySubject[oldUnknown.ID.String()]
	require.True(t, ok)
	assert.Equal(t, FindingStatusAdminPending, rec.Status)
	assert.True(t, rec.RequiresAdmin)

	// Summary section.
	stuck := res.Summary.StuckIntents
	require.NotNil(t, stuck)
	assert.Equal(t, 3, stuck.Total)
	assert.Equal(t, 2, stuck.AdminQueued)
	assert.Equal(t, 1, stuck.ModeParked)
	assert.Equal(t, 3, stuck.ByIntentType["nmi_delete_subscription"])
	assert.Len(t, stuck.Intents, 3)
	assert.Equal(t, 3, res.Summary.Totals.Findings)
	assert.Equal(t, 2, res.Summary.Totals.AdminPending)

	// Recovery: the unknown intent resolves, the pending one executes; only
	// the parked one is still stuck. The vanished findings auto-resolve.
	src.intents = []StuckIntent{parked}
	res2, err := eng.Run(context.Background(), RunParams{Mode: ModeAdvisory})
	require.NoError(t, err)
	require.NotNil(t, res2.Summary.StuckIntents)
	assert.Equal(t, 1, res2.Summary.StuckIntents.Total)
	assert.Equal(t, int64(2), res2.Summary.StuckIntents.AutoResolved)

	resolved := store.record(Provider("mobius"), FindingStuckIntent, oldPending.ID.String())
	require.NotNil(t, resolved)
	assert.Equal(t, FindingStatusResolved, resolved.Status)
	assert.Equal(t, "auto_vanished", resolved.Resolution)

	still := store.record(Provider("mobius"), FindingStuckIntent, parked.ID.String())
	require.NotNil(t, still)
	assert.Equal(t, FindingStatusOpen, still.Status)
	assert.Equal(t, 2, still.OccurrenceCount, "stable identity across runs")
}

// TestStuckIntentsEmittedAlongsideProviderRun proves the pass piggybacks on a
// normal provider-filtered run (provider-independent emission) and that
// enforce behaves identically to advisory for PS-10 (no applier).
func TestStuckIntentsEmittedAlongsideProviderRun(t *testing.T) {
	local := &fakeLocal{}
	snap := &RemoteSnapshot{Provider: ProviderNMI, Capabilities: Capabilities{Subscriptions: true}}
	eng, _, _ := newTestEngine(ProviderNMI, snap, local)
	oldPending := stuckTestIntent("pending", 25*time.Hour, "")
	eng.Intents = &fakeIntentSource{intents: []StuckIntent{oldPending}}

	res, err := eng.Run(context.Background(), RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderNMI}})
	require.NoError(t, err)
	assert.Equal(t, "completed", res.Status)

	var found *FindingRecord
	for i := range res.Findings {
		if res.Findings[i].Type == FindingStuckIntent {
			found = &res.Findings[i]
		}
	}
	require.NotNil(t, found, "PS-10 emitted even though the run was provider-filtered to nmi")
	assert.Equal(t, FindingStatusAdminPending, found.Status, "enforce never auto-fixes PS-10")
	require.NotNil(t, res.Summary.StuckIntents)
	assert.Zero(t, res.Summary.Providers["nmi"].AutoFixed)
}

func TestStuckIntentSourceNilSkipsPass(t *testing.T) {
	eng, _ := newStuckIntentEngine(nil)
	eng.Intents = nil
	res, err := eng.Run(context.Background(), RunParams{Mode: ModeAdvisory})
	require.NoError(t, err)
	assert.Nil(t, res.Summary.StuckIntents)
	assert.Empty(t, res.Findings)
}

func TestIsModeParkedReason(t *testing.T) {
	parked := []string{
		"mode=readonly blocks all provider writes",
		"mode=limited blocks proactive (system-origin) provider writes",
		"nmi client is read-only (mode=readonly)",
		"nmi provider writes blocked (mode=readonly)",
	}
	for _, reason := range parked {
		assert.True(t, isModeParkedReason(reason), reason)
	}
	notParked := []string{
		"",
		"nmi client not configured for provider \"mobius\"",
		"relevance check failed: connection refused",
		"nmi error: DECLINED",
		"no handler registered for intent type stripe_refund",
	}
	for _, reason := range notParked {
		assert.False(t, isModeParkedReason(reason), reason)
	}
}

func TestRenderStuckIntentsSection(t *testing.T) {
	var sb strings.Builder
	renderStuckIntents(&sb, &StuckIntentReport{
		Total:        2,
		AdminQueued:  1,
		ModeParked:   1,
		ByIntentType: map[string]int{"nmi_delete_subscription": 2},
		Intents: []StuckIntentLine{
			{IntentID: "a", IntentType: "nmi_delete_subscription", Provider: "mobius", Status: "pending", Origin: "user", Attempts: 4, Age: "26h0m0s", LastFailureReason: "nmi error: connection refused"},
			{IntentID: "b", IntentType: "nmi_delete_subscription", Provider: "mobius", Status: "pending", Origin: "system", Attempts: 0, Age: "30h0m0s", LastFailureReason: "mode=readonly blocks all provider writes", ModeParked: true},
		},
	})
	out := sb.String()
	assert.Contains(t, out, "Stuck provider intents (2): 1 admin-queued, 1 mode-parked")
	assert.Contains(t, out, "by intent type: nmi_delete_subscription=2")
	assert.Contains(t, out, "[mode-parked: informational]")
	assert.Contains(t, out, "last failure reason: nmi error: connection refused")
}
