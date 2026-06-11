package intents

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
)

// fakeLedger is an in-memory ledger recording the transition the Runner
// applied to each intent.
type fakeLedger struct {
	due        []gen.BillingProviderIntent
	dueVerify  []gen.BillingProviderIntent
	transition map[uuid.UUID]string // id -> applied transition
	reasons    map[uuid.UUID]string
	nextAt     map[uuid.UUID]time.Time
	evidence   map[uuid.UUID]map[string]any
}

func newFakeLedger() *fakeLedger {
	return &fakeLedger{
		transition: map[uuid.UUID]string{},
		reasons:    map[uuid.UUID]string{},
		nextAt:     map[uuid.UUID]time.Time{},
		evidence:   map[uuid.UUID]map[string]any{},
	}
}

func (f *fakeLedger) ClaimDue(context.Context, time.Time, time.Time, int64) ([]gen.BillingProviderIntent, error) {
	return f.due, nil
}
func (f *fakeLedger) ClaimDueVerify(context.Context, time.Time, time.Time, int64) ([]gen.BillingProviderIntent, error) {
	return f.dueVerify, nil
}
func (f *fakeLedger) ExpireOverdue(context.Context, time.Time) (int64, error) { return 0, nil }
func (f *fakeLedger) MarkSucceeded(_ context.Context, id uuid.UUID, _ time.Time, ev map[string]any) error {
	f.transition[id] = StatusSucceeded
	f.evidence[id] = ev
	return nil
}
func (f *fakeLedger) MarkFailedRetryable(_ context.Context, id uuid.UUID, next time.Time, reason string) error {
	f.transition[id] = StatusFailedRetryable
	f.reasons[id] = reason
	f.nextAt[id] = next
	return nil
}
func (f *fakeLedger) MarkUnknown(_ context.Context, id uuid.UUID, next time.Time, reason string) error {
	f.transition[id] = StatusUnknownNeedsVerify
	f.reasons[id] = reason
	f.nextAt[id] = next
	return nil
}
func (f *fakeLedger) MarkFailedTerminal(_ context.Context, id uuid.UUID, reason string) error {
	f.transition[id] = StatusFailedTerminal
	f.reasons[id] = reason
	return nil
}
func (f *fakeLedger) Park(_ context.Context, id uuid.UUID, next time.Time, reason string) error {
	f.transition[id] = StatusPending // parked = back to pending
	f.reasons[id] = reason
	f.nextAt[id] = next
	return nil
}
func (f *fakeLedger) MarkSuperseded(_ context.Context, id uuid.UUID, reason string) error {
	f.transition[id] = StatusSuperseded
	f.reasons[id] = reason
	return nil
}

// fakeHandler scripts one intent type's behavior.
type fakeHandler struct {
	typ       string
	relevance Relevance
	relErr    error
	execute   Outcome
	verify    Outcome
	executed  int
	verified  int
}

func (h *fakeHandler) Type() string { return h.typ }
func (h *fakeHandler) CheckRelevance(context.Context, gen.BillingProviderIntent) (Relevance, error) {
	return h.relevance, h.relErr
}
func (h *fakeHandler) Execute(context.Context, gen.BillingProviderIntent) Outcome {
	h.executed++
	return h.execute
}
func (h *fakeHandler) Verify(context.Context, gen.BillingProviderIntent) Outcome {
	h.verified++
	return h.verify
}
func (h *fakeHandler) Backoff(attempts int32) time.Duration {
	return time.Duration(attempts) * time.Minute
}

func testIntent(typ string, origin Origin, attempts int32) gen.BillingProviderIntent {
	return gen.BillingProviderIntent{
		ID:         uuid.New(),
		IntentType: typ,
		Provider:   "mobius",
		Origin:     string(origin),
		Attempts:   attempts,
		Status:     StatusInFlight,
	}
}

func runOnce(t *testing.T, ledger *fakeLedger, handler Handler, cfg ModeView) Stats {
	t.Helper()
	r := &Runner{Store: ledger, Registry: NewRegistry(handler), Config: cfg}
	stats, err := r.RunExecuteOnce(context.Background())
	require.NoError(t, err)
	return stats
}

func TestRunnerOutcomeClassification(t *testing.T) {
	cases := []struct {
		name           string
		outcome        Outcome
		wantTransition string
	}{
		{"success", Succeeded(map[string]any{"deleted": true}), StatusSucceeded},
		{"retryable failure", Retryable("declined"), StatusFailedRetryable},
		{"ambiguous parks for verifier", Ambiguous("timeout mid-write"), StatusUnknownNeedsVerify},
		{"terminal", Terminal("unsupported"), StatusFailedTerminal},
		{"parked stays pending", Parked("kill switch"), StatusPending},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ledger := newFakeLedger()
			h := &fakeHandler{typ: "t", relevance: StillRelevant(), execute: tc.outcome}
			intent := testIntent("t", OriginUser, 1)
			ledger.due = []gen.BillingProviderIntent{intent}

			runOnce(t, ledger, h, modeFull())
			assert.Equal(t, tc.wantTransition, ledger.transition[intent.ID])
			if tc.outcome.Reason != "" {
				assert.Equal(t, tc.outcome.Reason, ledger.reasons[intent.ID], "the reason is recorded on the intent")
			}
		})
	}
}

func TestRunnerRetryableUsesHandlerBackoff(t *testing.T) {
	ledger := newFakeLedger()
	h := &fakeHandler{typ: "t", relevance: StillRelevant(), execute: Retryable("provider down")}
	intent := testIntent("t", OriginUser, 3)
	ledger.due = []gen.BillingProviderIntent{intent}

	r := &Runner{Store: ledger, Registry: NewRegistry(h)}
	before := time.Now()
	_, err := r.RunExecuteOnce(context.Background())
	require.NoError(t, err)

	// fakeHandler backoff = attempts minutes (attempts=3 at claim time).
	next := ledger.nextAt[intent.ID]
	assert.WithinDuration(t, before.Add(3*time.Minute), next, 5*time.Second)
}

func TestRunnerRelevanceSupersedes(t *testing.T) {
	ledger := newFakeLedger()
	h := &fakeHandler{typ: "t", relevance: SupersededBy("subscription resumed"), execute: Succeeded(nil)}
	intent := testIntent("t", OriginUser, 1)
	ledger.due = []gen.BillingProviderIntent{intent}

	stats := runOnce(t, ledger, h, modeFull())
	assert.Equal(t, StatusSuperseded, ledger.transition[intent.ID])
	assert.Equal(t, "subscription resumed", ledger.reasons[intent.ID])
	assert.Equal(t, 1, stats.Superseded)
	assert.Zero(t, h.executed, "a superseded intent must never execute")
}

func TestRunnerRelevanceErrorParksWithoutExecuting(t *testing.T) {
	ledger := newFakeLedger()
	h := &fakeHandler{typ: "t", relErr: assert.AnError, execute: Succeeded(nil)}
	intent := testIntent("t", OriginUser, 1)
	ledger.due = []gen.BillingProviderIntent{intent}

	runOnce(t, ledger, h, modeFull())
	assert.Equal(t, StatusPending, ledger.transition[intent.ID], "relevance read failure keeps the intent pending")
	assert.Zero(t, h.executed)
}

// TestRunnerModeGating drives the gate through the Runner: parked intents go
// back to pending with the mode reason recorded and the handler is never
// invoked; allowed ones execute.
func TestRunnerModeGating(t *testing.T) {
	cases := []struct {
		name     string
		mode     ModeView
		origin   Origin
		executes bool
	}{
		{"user under limited executes", modeLimited(), OriginUser, true},
		{"admin under limited executes", modeLimited(), OriginAdmin, true},
		{"system under limited parks", modeLimited(), OriginSystem, false},
		{"user under readonly parks", modeReadonly(), OriginUser, false},
		{"system under readonly parks", modeReadonly(), OriginSystem, false},
		{"system under full executes", modeFull(), OriginSystem, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ledger := newFakeLedger()
			h := &fakeHandler{typ: "t", relevance: StillRelevant(), execute: Succeeded(nil)}
			intent := testIntent("t", tc.origin, 1)
			ledger.due = []gen.BillingProviderIntent{intent}

			runOnce(t, ledger, h, tc.mode)
			if tc.executes {
				assert.Equal(t, 1, h.executed)
				assert.Equal(t, StatusSucceeded, ledger.transition[intent.ID])
			} else {
				assert.Zero(t, h.executed, "gated intents must not attempt provider writes")
				assert.Equal(t, StatusPending, ledger.transition[intent.ID])
				assert.Contains(t, ledger.reasons[intent.ID], "mode=")
			}
		})
	}
}

func TestRunnerMissingHandlerParks(t *testing.T) {
	ledger := newFakeLedger()
	intent := testIntent("from_a_newer_build", OriginUser, 1)
	ledger.due = []gen.BillingProviderIntent{intent}

	r := &Runner{Store: ledger, Registry: NewRegistry()}
	_, err := r.RunExecuteOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, StatusPending, ledger.transition[intent.ID])
	assert.Contains(t, ledger.reasons[intent.ID], "no handler registered")
}

func TestRunnerVerifyResolution(t *testing.T) {
	cases := []struct {
		name           string
		verify         Outcome
		wantTransition string
	}{
		{"verified done -> succeeded", Succeeded(map[string]any{"verified_absent": true}), StatusSucceeded},
		{"verified not executed -> retryable", Retryable("still present"), StatusFailedRetryable},
		{"still inconclusive -> stays unknown", Ambiguous("read failed"), StatusUnknownNeedsVerify},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ledger := newFakeLedger()
			h := &fakeHandler{typ: "t", relevance: StillRelevant(), verify: tc.verify}
			intent := testIntent("t", OriginSystem, 2)
			intent.Status = StatusUnknownNeedsVerify
			ledger.dueVerify = []gen.BillingProviderIntent{intent}

			// readonly mode: verification is reads-only and must still run.
			r := &Runner{Store: ledger, Registry: NewRegistry(h), Config: modeReadonly()}
			_, err := r.RunVerifyOnce(context.Background())
			require.NoError(t, err)
			assert.Equal(t, 1, h.verified, "verify runs even under readonly")
			assert.Zero(t, h.executed)
			assert.Equal(t, tc.wantTransition, ledger.transition[intent.ID])
		})
	}
}

func TestRunnerVerifySupersedesIrrelevant(t *testing.T) {
	ledger := newFakeLedger()
	h := &fakeHandler{typ: "t", relevance: SupersededBy("resumed"), verify: Succeeded(nil)}
	intent := testIntent("t", OriginUser, 2)
	ledger.dueVerify = []gen.BillingProviderIntent{intent}

	r := &Runner{Store: ledger, Registry: NewRegistry(h)}
	_, err := r.RunVerifyOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, StatusSuperseded, ledger.transition[intent.ID])
	assert.Zero(t, h.verified)
}

func TestRegistrySemantics(t *testing.T) {
	h := &fakeHandler{typ: "a"}
	reg := NewRegistry(h)
	assert.Same(t, h, reg.Lookup("a").(*fakeHandler))
	assert.Nil(t, reg.Lookup("missing"))
	assert.ElementsMatch(t, []string{"a"}, reg.Types())

	assert.PanicsWithValue(t, "intents: duplicate handler for intent type a", func() {
		reg.Register(&fakeHandler{typ: "a"})
	})
	assert.Panics(t, func() { reg.Register(nil) })
}
