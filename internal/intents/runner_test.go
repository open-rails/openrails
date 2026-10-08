package intents

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
)

type ledgerRec struct {
	status, reason string
	next           time.Time
	evidence       map[string]any
}

// fakeLedger is an in-memory ledger: row is what Enqueue/Get/ClaimByID see.
type fakeLedger struct {
	row                    gen.BillingProviderIntent
	due, dueVerify         []gen.BillingProviderIntent
	refuseClaim            bool
	markErr, logErr        error
	recoveryErr            error
	recs                   map[uuid.UUID]*ledgerRec
	logs                   []MutationLogParams
	pruned, prunedTerminal []uuid.UUID
	claims                 int
}

func (f *fakeLedger) set(id uuid.UUID, status, reason string, next time.Time, ev map[string]any) error {
	if f.recs == nil {
		f.recs = map[uuid.UUID]*ledgerRec{}
	}
	f.recs[id] = &ledgerRec{status, reason, next, ev}
	return nil
}

func (f *fakeLedger) Enqueue(context.Context, EnqueueParams) (gen.BillingProviderIntent, error) {
	return f.row, nil
}
func (f *fakeLedger) Get(_ context.Context, id uuid.UUID) (gen.BillingProviderIntent, error) {
	row := f.row
	row.ID = id
	if r := f.recs[id]; r != nil {
		row.Status, row.LastFailureReason = r.status, &r.reason
	}
	return row, nil
}
func (f *fakeLedger) ClaimByID(_ context.Context, id uuid.UUID, _, _ time.Time) (gen.BillingProviderIntent, bool, error) {
	f.claims++
	if f.refuseClaim {
		return gen.BillingProviderIntent{}, false, nil
	}
	row := f.row
	row.ID, row.Status = id, StatusInFlight
	row.Attempts++
	return row, true, nil
}
func (f *fakeLedger) ClaimDue(context.Context, time.Time, time.Time, int64) ([]gen.BillingProviderIntent, error) {
	return f.due, nil
}
func (f *fakeLedger) ClaimDueVerify(context.Context, time.Time, time.Time, int64) ([]gen.BillingProviderIntent, error) {
	return f.dueVerify, nil
}
func (f *fakeLedger) ClaimUnknownByID(context.Context, uuid.UUID, time.Time, time.Time) (gen.BillingProviderIntent, bool, error) {
	return gen.BillingProviderIntent{}, false, nil
}
func (f *fakeLedger) ReleaseUnknownClaim(context.Context, uuid.UUID) (bool, error) { return false, nil }
func (f *fakeLedger) RenewClaim(context.Context, uuid.UUID, string, int32, time.Time, time.Time) (bool, error) {
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
	if f.markErr != nil {
		return f.markErr
	}
	return f.set(id, StatusSucceeded, "", time.Time{}, ev)
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
	return f.set(id, StatusFailedRetryable, reason, next, nil)
}
func (f *fakeLedger) MarkUnknown(_ context.Context, id uuid.UUID, next time.Time, reason string, ev map[string]any) error {
	return f.set(id, StatusUnknownNeedsVerify, reason, next, ev)
}
func (f *fakeLedger) MarkFailedTerminal(_ context.Context, id uuid.UUID, reason string, ev map[string]any) error {
	return f.set(id, StatusFailedTerminal, reason, time.Time{}, ev)
}
func (f *fakeLedger) Park(_ context.Context, id uuid.UUID, next time.Time, reason string) error {
	return f.set(id, StatusPending, reason, next, nil)
}
func (f *fakeLedger) ParkForRecovery(_ context.Context, id uuid.UUID, next time.Time, reason string) error {
	return f.set(id, StatusPending, reason, next, RecoveryHeld(reason).Evidence)
}
func (f *fakeLedger) WakeRecoveryHeld(_ context.Context, id uuid.UUID, now time.Time) error {
	if record := f.recs[id]; record != nil {
		record.next = now
	}
	return nil
}
func (f *fakeLedger) MarkSuperseded(_ context.Context, id uuid.UUID, reason string) error {
	return f.set(id, StatusSuperseded, reason, time.Time{}, nil)
}

type fakeHandler struct {
	typ                  string
	relevance            Relevance
	relErr               error
	execute, verify      Outcome
	pruneTerminalPayload bool
	executed, verified   int
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
func (h *fakeHandler) PruneTerminalPayload() bool { return h.pruneTerminalPayload }

type fakeKillSwitch struct {
	allow bool
	calls int
}

func (g *fakeKillSwitch) AllowDestructive(context.Context, uuid.UUID) (bool, string) {
	g.calls++
	return g.allow, "instance kill switch is OFF"
}

func testIntent(typ string, origin Origin, attempts int32) gen.BillingProviderIntent {
	return gen.BillingProviderIntent{
		ID: uuid.New(), MerchantID: uuid.New(), IntentType: typ, Rail: "nmi",
		IdempotencyKey: "intent-" + uuid.NewString(), Origin: string(origin), Attempts: attempts, Status: StatusInFlight,
	}
}

// One pass of the executor: every pre-write guard parks (stays pending, never
// fails) and every outcome class lands on its ledger transition.
func TestRunnerExecuteDecisions(t *testing.T) {
	const destructive = TypeNMIDeleteSubscription
	for _, tc := range []struct {
		name       string
		typ        string // intent type; handler is always "t" unless typ == destructive
		origin     Origin
		mode       ModeView
		handler    fakeHandler
		logErr     error
		kill       *fakeKillSwitch
		status     string
		reason     string
		executed   bool
		next       time.Duration
		evidence   map[string]any
		pruneTerm  bool
		killCalled bool
	}{
		{name: "success", typ: "t", mode: modeFull, handler: fakeHandler{execute: Succeeded(map[string]any{"k": "v"})}, status: StatusSucceeded, executed: true, evidence: map[string]any{"k": "v"}},
		{name: "retryable uses handler backoff", typ: "t", mode: modeFull, handler: fakeHandler{execute: Retryable("down")}, status: StatusFailedRetryable, executed: true, next: 3 * time.Minute},
		{name: "fresh ambiguity verifies soon", typ: "t", mode: modeFull, handler: fakeHandler{execute: Ambiguous("timeout")}, status: StatusUnknownNeedsVerify, executed: true, next: VerifyDelay},
		{name: "terminal keeps evidence", typ: "t", mode: modeFull, handler: fakeHandler{execute: TerminalWithEvidence("declined", map[string]any{"response_code": 252})}, status: StatusFailedTerminal, executed: true, evidence: map[string]any{"response_code": 252}},
		{name: "terminal prunes credential payload on opt-in", typ: "t", mode: modeFull, handler: fakeHandler{execute: Terminal("rejected"), pruneTerminalPayload: true}, status: StatusFailedTerminal, executed: true, pruneTerm: true},
		{name: "handler park", typ: "t", mode: modeFull, handler: fakeHandler{execute: Parked("client not armed")}, status: StatusPending, reason: "not armed", executed: true, next: ParkRetryInterval},
		{name: "deploy skew: no handler", typ: "from_a_newer_build", mode: modeFull, status: StatusPending, reason: "no handler registered"},
		{name: "relevance read failure", typ: "t", mode: modeFull, handler: fakeHandler{relErr: errors.New("db down")}, status: StatusPending, reason: "relevance check failed"},
		{name: "irrelevant supersedes", typ: "t", mode: modeFull, handler: fakeHandler{relevance: SupersededBy("resumed")}, status: StatusSuperseded, reason: "resumed"},
		{name: "nil mode fails closed", typ: "t", status: StatusPending, reason: "operating mode is unknown"},
		{name: "readonly parks user writes", typ: "t", mode: modeReadonly, status: StatusPending, reason: "readonly"},
		{name: "limited parks system writes", typ: "t", origin: OriginSystem, mode: modeLimited, status: StatusPending, reason: "limited"},
		{name: "no attempt without a mutation log", typ: "t", mode: modeFull, logErr: errors.New("write failed"), status: StatusPending, reason: "mutation log unavailable"},
		{name: "kill switch parks destructive", typ: destructive, origin: OriginSystem, mode: modeFull, kill: &fakeKillSwitch{}, status: StatusPending, reason: "kill switch", killCalled: true},
		{name: "armed kill switch lets destructive run", typ: destructive, origin: OriginSystem, mode: modeFull, kill: &fakeKillSwitch{allow: true}, handler: fakeHandler{execute: Succeeded(nil)}, status: StatusSucceeded, executed: true, killCalled: true},
		{name: "kill switch ignores non-destructive", typ: "t", origin: OriginSystem, mode: modeFull, kill: &fakeKillSwitch{}, handler: fakeHandler{execute: Succeeded(nil)}, status: StatusSucceeded, executed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			origin := tc.origin
			if origin == "" {
				origin = OriginUser
			}
			h := tc.handler
			h.typ = "t"
			if tc.typ == destructive {
				h.typ = destructive
			}
			if h.relErr == nil && h.relevance == (Relevance{}) {
				h.relevance = StillRelevant()
			}
			intent := testIntent(tc.typ, origin, 3)
			ledger := &fakeLedger{due: []gen.BillingProviderIntent{intent}, logErr: tc.logErr}
			r := &Runner{Store: ledger, Registry: NewRegistry(&h), Config: tc.mode}
			if tc.kill != nil {
				r.Destructive = tc.kill
			}
			before := time.Now()
			stats, err := r.RunExecuteOnce(context.Background())
			require.NoError(t, err)

			rec := ledger.recs[intent.ID]
			require.NotNil(t, rec, "every claimed intent gets a transition")
			assert.Equal(t, tc.status, rec.status)
			assert.Contains(t, rec.reason, tc.reason)
			assert.Equal(t, tc.executed, h.executed == 1)
			if tc.next > 0 {
				assert.WithinDuration(t, before.Add(tc.next), rec.next, 5*time.Second)
			}
			if tc.evidence != nil {
				assert.Equal(t, tc.evidence, rec.evidence)
			}
			assert.Equal(t, tc.pruneTerm, len(ledger.prunedTerminal) == 1)
			if tc.status == StatusSucceeded {
				assert.Equal(t, 1, stats.Succeeded)
				assert.Equal(t, []uuid.UUID{intent.ID}, ledger.pruned)
			}
			if tc.kill != nil {
				assert.Equal(t, tc.killCalled, tc.kill.calls == 1)
			}
			if tc.executed {
				require.Len(t, ledger.logs, 2, "attempt is logged before the write, result after")
				assert.Equal(t, MutationLogPhaseAttempting, ledger.logs[0].Phase)
			}
		})
	}
}

func TestRunnerNeverReportsUncommittedSuccess(t *testing.T) {
	intent := testIntent("t", OriginUser, 1)
	ledger := &fakeLedger{due: []gen.BillingProviderIntent{intent}, markErr: errors.New("commit failed")}
	h := &fakeHandler{typ: "t", relevance: StillRelevant(), execute: Succeeded(nil)}
	stats, err := (&Runner{Store: ledger, Registry: NewRegistry(h), Config: modeFull}).RunExecuteOnce(context.Background())
	require.NoError(t, err)
	assert.Zero(t, stats.Succeeded)
	assert.Empty(t, ledger.pruned)
	assert.Empty(t, ledger.recs)
}

// Verification is read-only: it runs under readonly and never logs a mutation.
func TestRunnerVerifyResolution(t *testing.T) {
	for _, tc := range []struct {
		name      string
		relevance Relevance
		verify    Outcome
		status    string
		verified  bool
	}{
		{"verified done", StillRelevant(), Succeeded(map[string]any{"verified_absent": true}), StatusSucceeded, true},
		{"verified not executed", StillRelevant(), Retryable("still present"), StatusFailedRetryable, true},
		{"still inconclusive", StillRelevant(), Ambiguous("read failed"), StatusUnknownNeedsVerify, true},
		{"verifier park stays unknown", StillRelevant(), Parked("client not armed"), StatusUnknownNeedsVerify, true},
		{"irrelevant supersedes", SupersededBy("resumed"), Succeeded(nil), StatusSuperseded, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			intent := testIntent("t", OriginSystem, 2)
			intent.Status = StatusUnknownNeedsVerify
			ledger := &fakeLedger{dueVerify: []gen.BillingProviderIntent{intent}}
			h := &fakeHandler{typ: "t", relevance: tc.relevance, verify: tc.verify}
			_, err := (&Runner{Store: ledger, Registry: NewRegistry(h), Config: modeReadonly}).RunVerifyOnce(context.Background())
			require.NoError(t, err)
			assert.Equal(t, tc.verified, h.verified == 1)
			assert.Zero(t, h.executed)
			assert.Equal(t, tc.status, ledger.recs[intent.ID].status)
			assert.Empty(t, ledger.logs)
		})
	}

	intent := testIntent("unknown_type", OriginSystem, 2)
	ledger := &fakeLedger{dueVerify: []gen.BillingProviderIntent{intent}}
	_, err := (&Runner{Store: ledger, Registry: NewRegistry()}).RunVerifyOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, StatusUnknownNeedsVerify, ledger.recs[intent.ID].status, "no handler: stays unknown, never pending")
}

func TestEnqueueAndExecute(t *testing.T) {
	run := func(status string, refuse bool) (*fakeLedger, *fakeHandler, gen.BillingProviderIntent) {
		ledger := &fakeLedger{row: testIntent("t", OriginSystem, 0), refuseClaim: refuse}
		ledger.row.Status = status
		h := &fakeHandler{typ: "t", relevance: StillRelevant(), execute: Succeeded(nil)}
		row, err := (&Runner{Store: ledger, Registry: NewRegistry(h), Config: modeFull}).
			EnqueueAndExecute(context.Background(), EnqueueParams{IntentType: "t", IdempotencyKey: "k"})
		require.NoError(t, err)
		return ledger, h, row
	}
	for _, status := range []string{StatusPending, StatusFailedRetryable} {
		ledger, h, row := run(status, false)
		assert.Equal(t, 1, h.executed, status)
		assert.Equal(t, 1, ledger.claims)
		assert.Equal(t, StatusSucceeded, row.Status, "returns the post-execution row")
	}
	// An unclaimable conflict row is handed back untouched: the caller acts on the durable prior outcome.
	for _, status := range []string{StatusSucceeded, StatusFailedTerminal, StatusUnknownNeedsVerify, StatusInFlight, StatusSuperseded} {
		ledger, h, row := run(status, false)
		assert.Zero(t, h.executed, status)
		assert.Zero(t, ledger.claims, status)
		assert.Equal(t, status, row.Status)
	}
	// A claim race leaves the row to the scheduled pipeline.
	ledger, h, row := run(StatusPending, true)
	assert.Zero(t, h.executed)
	assert.Equal(t, 1, ledger.claims)
	assert.Equal(t, StatusPending, row.Status)
}

func (f *fakeLedger) CheckRecovery(context.Context, gen.BillingProviderIntent, time.Time) error {
	return f.recoveryErr
}

func TestHeldCollectionsRedispatchOnlyForReadback(t *testing.T) {
	for _, test := range []struct {
		name     string
		kind     string
		mode     ModeView
		evidence []byte
	}{
		{"restored renewal without local fence", "subscription_collection", modeFull, nil},
		{"old retryable submitted operation", "subscription_collection", modeFull, []byte(`{"submitted_at":"2026-10-01T00:00:00Z"}`)},
		{"readonly submitted operation", "subscription_collection", modeReadonly, []byte(`{"submitted_at":"2026-10-01T00:00:00Z"}`)},
		{"restored invoice without local fence", "invoice_collection", modeFull, nil},
		{"readonly invoice without local fence", "invoice_collection", modeReadonly, nil},
		{"stale paid tier change", "nmi_upgrade", modeFull, []byte(`{"proration":{"submitted_at":"2026-10-01T00:00:00Z"}}`)},
		{"readonly paid tier change", "nmi_upgrade", modeReadonly, []byte(`{"proration":{"submitted_at":"2026-10-01T00:00:00Z"}}`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			intent := testIntent(test.kind, OriginSystem, 1)
			intent.Status = StatusFailedRetryable
			intent.ResultEvidence = test.evidence
			store := &fakeLedger{row: intent, due: []gen.BillingProviderIntent{intent}, recoveryErr: errors.New("provider recovery required")}
			handler := &fakeHandler{typ: intent.IntentType, relevance: StillRelevant(), verify: Succeeded(nil)}
			runner := &Runner{Store: store, Registry: NewRegistry(handler), Config: test.mode}
			_, err := runner.RunExecuteOnce(context.Background())
			require.NoError(t, err)
			require.Zero(t, handler.executed)
			require.Equal(t, StatusUnknownNeedsVerify, store.recs[intent.ID].status)
			require.Equal(t, test.evidence, store.row.ResultEvidence, "recovery does not fabricate a submission fence")
			intent.Status = StatusUnknownNeedsVerify
			store.dueVerify = []gen.BillingProviderIntent{intent}
			_, err = runner.RunVerifyOnce(context.Background())
			require.NoError(t, err)
			require.Equal(t, 1, handler.verified)
			require.Zero(t, handler.executed)
			require.Equal(t, StatusSucceeded, store.recs[intent.ID].status)
		})
	}
}

func TestVerifiedUnsubmittedRecoveryReconsidersOnlyWhenAllowed(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mode      ModeView
		submitted bool
		held      bool
		ready     bool
	}{
		{"ready", modeFull, false, false, true},
		{"readonly", modeReadonly, false, false, false},
		{"still stale", modeFull, false, true, false},
		{"submitted replay keeps backoff", modeFull, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := testIntent("subscription_collection", OriginSystem, 1)
			in.Rail = "stripe"
			in.Status = StatusUnknownNeedsVerify
			if tc.submitted {
				in.ResultEvidence = []byte(`{"submitted_at":"2026-10-01T00:00:00Z"}`)
			}
			store := &fakeLedger{row: in, dueVerify: []gen.BillingProviderIntent{in}}
			if tc.held {
				store.recoveryErr = errors.New("provider recovery required")
			}
			handler := &fakeHandler{typ: in.IntentType, relevance: StillRelevant(), verify: Retryable("verified unsubmitted")}
			before := time.Now()
			_, err := (&Runner{Store: store, Registry: NewRegistry(handler), Config: tc.mode}).RunVerifyOnce(context.Background())
			require.NoError(t, err)
			require.Zero(t, handler.executed, "verification never dispatches the provider")
			record := store.recs[in.ID]
			if tc.ready {
				require.Equal(t, StatusFailedRetryable, record.status)
				require.WithinDuration(t, before, record.next, time.Second)
			} else {
				require.True(t, record.next.After(before.Add(time.Second)), "provider/policy backoff is not expedited")
			}
		})
	}
}
