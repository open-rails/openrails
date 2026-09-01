package intents

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
)

// fakeLedger is an in-memory ledger recording the transition the Runner
// applied to each intent.
type fakeLedger struct {
	due            []gen.OpenrailsRailIntent
	dueVerify      []gen.OpenrailsRailIntent
	accounts       map[uuid.UUID]gen.OpenrailsPsp
	transition     map[uuid.UUID]string // id -> applied transition
	reasons        map[uuid.UUID]string
	nextAt         map[uuid.UUID]time.Time
	evidence       map[uuid.UUID]map[string]any
	pruned         []uuid.UUID // ids the Runner pruned post-success (#607)
	prunedTerminal []uuid.UUID
	logs           []MutationLogParams
	logErr         error

	// Synchronous-path state: Enqueue returns enqueued (scripted conflict
	// row); ClaimByID claims it unless claimByIDRefused.
	enqueued        gen.OpenrailsRailIntent
	claimByIDRefuse bool
	enqueueCalls    int
	claimByIDCalls  int
}

func newFakeLedger() *fakeLedger {
	return &fakeLedger{
		transition: map[uuid.UUID]string{},
		accounts:   map[uuid.UUID]gen.OpenrailsPsp{},
		reasons:    map[uuid.UUID]string{},
		nextAt:     map[uuid.UUID]time.Time{},
		evidence:   map[uuid.UUID]map[string]any{},
	}
}

func (f *fakeLedger) GetPSP(_ context.Context, id uuid.UUID) (gen.OpenrailsPsp, error) {
	account, ok := f.accounts[id]
	if !ok {
		return gen.OpenrailsPsp{}, pgx.ErrNoRows
	}
	return account, nil
}

func (f *fakeLedger) Enqueue(context.Context, EnqueueParams) (gen.OpenrailsRailIntent, error) {
	f.enqueueCalls++
	return f.enqueued, nil
}

func (f *fakeLedger) Get(_ context.Context, id uuid.UUID) (gen.OpenrailsRailIntent, error) {
	row := f.enqueued
	row.ID = id
	if status, ok := f.transition[id]; ok {
		row.Status = status
	}
	if reason, ok := f.reasons[id]; ok {
		r := reason
		row.LastFailureReason = &r
	}
	return row, nil
}

func (f *fakeLedger) ClaimByID(_ context.Context, id uuid.UUID, _, _ time.Time) (gen.OpenrailsRailIntent, bool, error) {
	f.claimByIDCalls++
	if f.claimByIDRefuse {
		return gen.OpenrailsRailIntent{}, false, nil
	}
	row := f.enqueued
	row.ID = id
	row.Status = StatusInFlight
	row.Attempts++
	return row, true, nil
}

func (f *fakeLedger) ClaimDue(context.Context, time.Time, time.Time, int64) ([]gen.OpenrailsRailIntent, error) {
	return f.due, nil
}
func (f *fakeLedger) ClaimDueVerify(context.Context, time.Time, time.Time, int64) ([]gen.OpenrailsRailIntent, error) {
	return f.dueVerify, nil
}
func (f *fakeLedger) RenewClaim(context.Context, uuid.UUID, time.Time, time.Time) (bool, error) {
	return true, nil
}
func (f *fakeLedger) ExpireOverdue(context.Context, time.Time) (int64, error) { return 0, nil }
func (f *fakeLedger) LogExternalMutation(_ context.Context, p MutationLogParams) error {
	if f.logErr != nil {
		return f.logErr
	}
	f.logs = append(f.logs, p)
	return nil
}
func (f *fakeLedger) MarkSucceeded(_ context.Context, id uuid.UUID, _ time.Time, ev map[string]any) error {
	f.transition[id] = StatusSucceeded
	f.evidence[id] = ev
	return nil
}
func (f *fakeLedger) PruneSucceeded(_ context.Context, id uuid.UUID, _ map[string]any, _, _ bool) error {
	f.pruned = append(f.pruned, id)
	return nil
}
func (f *fakeLedger) PruneTerminalPayload(_ context.Context, id uuid.UUID) error {
	f.prunedTerminal = append(f.prunedTerminal, id)
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
func (f *fakeLedger) MarkFailedTerminal(_ context.Context, id uuid.UUID, reason string, ev map[string]any) error {
	f.transition[id] = StatusFailedTerminal
	f.reasons[id] = reason
	f.evidence[id] = ev
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
	typ                  string
	relevance            Relevance
	relErr               error
	execute              Outcome
	verify               Outcome
	pruneTerminalPayload bool
	executed             int
	verified             int
}

func (h *fakeHandler) Type() string { return h.typ }
func (h *fakeHandler) CheckRelevance(context.Context, gen.OpenrailsRailIntent) (Relevance, error) {
	return h.relevance, h.relErr
}
func (h *fakeHandler) Execute(context.Context, gen.OpenrailsRailIntent) Outcome {
	h.executed++
	return h.execute
}
func (h *fakeHandler) Verify(context.Context, gen.OpenrailsRailIntent) Outcome {
	h.verified++
	return h.verify
}
func (h *fakeHandler) Backoff(attempts int32) time.Duration {
	return time.Duration(attempts) * time.Minute
}
func (h *fakeHandler) PruneTerminalPayload() bool { return h.pruneTerminalPayload }

func testIntent(typ string, origin Origin, attempts int32) gen.OpenrailsRailIntent {
	return gen.OpenrailsRailIntent{
		ID:             uuid.New(),
		MerchantID:     uuid.New(),
		IntentType:     typ,
		Rail:           "mobius",
		IdempotencyKey: "intent-" + uuid.NewString(),
		Origin:         string(origin),
		Attempts:       attempts,
		Status:         StatusInFlight,
	}
}

func runOnce(t *testing.T, ledger *fakeLedger, handler Handler, cfg ModeView) Stats {
	t.Helper()
	r := &Runner{Store: ledger, Registry: NewRegistry(handler), Config: cfg}
	stats, err := r.RunExecuteOnce(context.Background())
	require.NoError(t, err)
	return stats
}

func TestRunnerRetryableUsesHandlerBackoff(t *testing.T) {
	ledger := newFakeLedger()
	h := &fakeHandler{typ: "t", relevance: StillRelevant(), execute: Retryable("provider down")}
	intent := testIntent("t", OriginUser, 3)
	ledger.due = []gen.OpenrailsRailIntent{intent}

	// Config is required: a Runner with no mode view now parks every intent
	// rather than executing (or#865 — the gate no longer fails open).
	r := &Runner{Store: ledger, Registry: NewRegistry(h), Config: modeFull()}
	before := time.Now()
	_, err := r.RunExecuteOnce(context.Background())
	require.NoError(t, err)

	// fakeHandler backoff = attempts minutes (attempts=3 at claim time).
	next := ledger.nextAt[intent.ID]
	assert.WithinDuration(t, before.Add(3*time.Minute), next, 5*time.Second)
}

func TestRunnerRelevanceErrorParksWithoutExecuting(t *testing.T) {
	ledger := newFakeLedger()
	h := &fakeHandler{typ: "t", relErr: assert.AnError, execute: Succeeded(nil)}
	intent := testIntent("t", OriginUser, 1)
	ledger.due = []gen.OpenrailsRailIntent{intent}

	runOnce(t, ledger, h, modeFull())
	assert.Equal(t, StatusPending, ledger.transition[intent.ID], "relevance read failure keeps the intent pending")
	assert.Zero(t, h.executed)
}

func TestRunnerMissingHandlerParks(t *testing.T) {
	ledger := newFakeLedger()
	intent := testIntent("from_a_newer_build", OriginUser, 1)
	ledger.due = []gen.OpenrailsRailIntent{intent}

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
			ledger.dueVerify = []gen.OpenrailsRailIntent{intent}

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
	ledger.dueVerify = []gen.OpenrailsRailIntent{intent}

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

// TestEnqueueAndExecuteConflictRowReturnedUntouched: an unclaimable conflict
// (already succeeded) is handed back without execution — the caller acts on
// the durable prior outcome (the dunning worker's repair path).
func TestEnqueueAndExecuteConflictRowReturnedUntouched(t *testing.T) {
	for _, status := range []string{StatusSucceeded, StatusFailedTerminal, StatusUnknownNeedsVerify, StatusInFlight} {
		t.Run(status, func(t *testing.T) {
			ledger := newFakeLedger()
			h := &fakeHandler{typ: "t", relevance: StillRelevant(), execute: Succeeded(nil)}
			ledger.enqueued = testIntent("t", OriginSystem, 1)
			ledger.enqueued.Status = status

			r := &Runner{Store: ledger, Registry: NewRegistry(h), Config: modeFull()}
			row, err := r.EnqueueAndExecute(context.Background(), EnqueueParams{IntentType: "t", IdempotencyKey: "k"})
			require.NoError(t, err)
			assert.Zero(t, h.executed)
			assert.Zero(t, ledger.claimByIDCalls, "unclaimable statuses are never claimed")
			assert.Equal(t, status, row.Status)
		})
	}
}

// TestEnqueueAndExecuteClaimRaceFallsBackToLedger: a pending row that races
// into unclaimable (expiry sweep, another executor) is left to the scheduled
// pipeline.
func TestEnqueueAndExecuteClaimRaceFallsBackToLedger(t *testing.T) {
	ledger := newFakeLedger()
	h := &fakeHandler{typ: "t", relevance: StillRelevant(), execute: Succeeded(nil)}
	ledger.enqueued = testIntent("t", OriginUser, 0)
	ledger.enqueued.Status = StatusPending
	ledger.claimByIDRefuse = true

	r := &Runner{Store: ledger, Registry: NewRegistry(h), Config: modeFull()}
	row, err := r.EnqueueAndExecute(context.Background(), EnqueueParams{IntentType: "t", IdempotencyKey: "k"})
	require.NoError(t, err)
	assert.Zero(t, h.executed)
	assert.Equal(t, 1, ledger.claimByIDCalls)
	assert.Equal(t, StatusPending, row.Status)
}

// TestRunnerTerminalEvidencePersisted: terminal outcomes carry their evidence
// (decline response codes) onto the ledger.
func TestRunnerTerminalEvidencePersisted(t *testing.T) {
	ledger := newFakeLedger()
	h := &fakeHandler{typ: "t", relevance: StillRelevant(),
		execute: TerminalWithEvidence("rebill declined", map[string]any{"response_code": 252})}
	intent := testIntent("t", OriginSystem, 1)
	ledger.due = []gen.OpenrailsRailIntent{intent}

	runOnce(t, ledger, h, modeFull())
	assert.Equal(t, StatusFailedTerminal, ledger.transition[intent.ID])
	assert.Equal(t, map[string]any{"response_code": 252}, ledger.evidence[intent.ID])
}

func TestRunnerPrunesTerminalPayloadWhenHandlerOptsIn(t *testing.T) {
	ledger := newFakeLedger()
	h := &fakeHandler{
		typ:                  "t",
		relevance:            StillRelevant(),
		execute:              Terminal("single-use credential rejected"),
		pruneTerminalPayload: true,
	}
	intent := testIntent("t", OriginUser, 1)
	ledger.due = []gen.OpenrailsRailIntent{intent}

	runOnce(t, ledger, h, modeFull())

	assert.Equal(t, StatusFailedTerminal, ledger.transition[intent.ID])
	assert.Equal(t, []uuid.UUID{intent.ID}, ledger.prunedTerminal)
}

func TestRunnerDoesNotLogReadOnlyVerify(t *testing.T) {
	ledger := newFakeLedger()
	h := &fakeHandler{typ: "t", relevance: StillRelevant(), verify: Succeeded(nil)}
	intent := testIntent("t", OriginUser, 2)
	intent.Status = StatusUnknownNeedsVerify
	ledger.dueVerify = []gen.OpenrailsRailIntent{intent}

	r := &Runner{Store: ledger, Registry: NewRegistry(h)}
	_, err := r.RunVerifyOnce(context.Background())
	require.NoError(t, err)

	assert.Equal(t, 1, h.verified)
	assert.Empty(t, ledger.logs)
}

func TestRunnerDoesNotExecuteWhenAttemptLogFails(t *testing.T) {
	ledger := newFakeLedger()
	ledger.logErr = errors.New("write failed")
	h := &fakeHandler{typ: "t", relevance: StillRelevant(), execute: Succeeded(nil)}
	intent := testIntent("t", OriginUser, 1)
	ledger.due = []gen.OpenrailsRailIntent{intent}

	runOnce(t, ledger, h, modeFull())

	assert.Zero(t, h.executed)
	assert.Equal(t, StatusPending, ledger.transition[intent.ID])
	assert.Contains(t, ledger.reasons[intent.ID], "mutation log unavailable")
}

func TestMutationLogScrubsSensitiveFields(t *testing.T) {
	got := scrubMutationEvidence(map[string]any{
		"remote_id":      "sub_123",
		"security_key":   "raw-secret",
		"message":        "provider failed security_key=raw-secret card_number=4111111111111111",
		"nested":         map[string]any{"authorization": "Bearer token", "status": "failed"},
		"privateKeyFile": "/tmp/key.pem",
	})

	body, err := json.Marshal(got)
	require.NoError(t, err)
	text := string(body)
	assert.NotContains(t, text, "raw-secret")
	assert.NotContains(t, text, "4111111111111111")
	assert.NotContains(t, text, "Bearer token")
	assert.NotContains(t, text, "/tmp/key.pem")
	assert.Contains(t, text, "sub_123")
}

// fakeDestructiveGate is a canned #836 kill switch.
type fakeDestructiveGate struct {
	allow  bool
	reason string
	calls  int
}

func (g *fakeDestructiveGate) AllowDestructive(ctx context.Context, merchantID uuid.UUID) (bool, string) {
	g.calls++
	return g.allow, g.reason
}

// #836: an operator flipping the DB-backed kill switch must stop destructive
// provider writes on every node, without a deploy — and the intent must PARK,
// not fail, so flipping it back resumes exactly where it stopped.
func TestRunnerDestructiveKillSwitch(t *testing.T) {
	cases := []struct {
		name     string
		typ      string
		allow    bool
		executes bool
		consults bool
	}{
		// Rows for "blocked by the switch" / "allowed when armed" live in
		// internal/river/provider_intents_rls_integration_test.go against the
		// real DB-backed gate.
		// Non-destructive work (refunds, captures) is not what the switch is
		// for; halting money movement would be its own incident.
		{"non-destructive type is not gated", "not_destructive", false, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ledger := newFakeLedger()
			h := &fakeHandler{typ: tc.typ, relevance: StillRelevant(), execute: Succeeded(nil)}
			intent := testIntent(tc.typ, OriginSystem, 1)
			ledger.due = []gen.OpenrailsRailIntent{intent}

			gate := &fakeDestructiveGate{allow: tc.allow, reason: "instance kill switch is OFF"}
			r := &Runner{Store: ledger, Registry: NewRegistry(h), Config: modeFull(), Destructive: gate}
			_, err := r.RunExecuteOnce(context.Background())
			require.NoError(t, err)

			if tc.executes {
				assert.Equal(t, 1, h.executed)
			} else {
				assert.Zero(t, h.executed, "the kill switch must stop the provider write, not just record it")
				assert.Equal(t, StatusPending, ledger.transition[intent.ID], "parked, never failed: flipping the switch back must resume it")
				assert.Contains(t, ledger.reasons[intent.ID], "kill switch")
			}
			if tc.consults {
				assert.Equal(t, 1, gate.calls)
			} else {
				assert.Zero(t, gate.calls)
			}
		})
	}
}
